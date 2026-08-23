// Package usageparse 从多种上游协议的响应中解析 token 用量。
//
// 支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Claude Messages、
// Google Gemini generateContent 四类形状，非流式 JSON 与流式 SSE 均可解析。
//
// 各协议对「输入 token 是否已包含缓存命中」口径不同：
//   - OpenAI / Gemini：prompt_tokens 已含缓存命中 token（inclusive）
//   - Claude：input_tokens 不含 cache_read / cache_creation（exclusive）
//
// 因此本包保留上游原样字段用于展示，并由 Billable() 统一归一化为
// 「Input / Output / CacheRead / CacheCreation」四类计费口径，
// 使结算侧只面对一套语义。
package usageparse

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
)

// Protocol 标识识别出的上游协议形状。
type Protocol string

// 已识别的协议形状。
const (
	ProtocolUnknown         Protocol = ""
	ProtocolOpenAIChat      Protocol = "openai_chat"
	ProtocolOpenAIResponses Protocol = "openai_responses"
	ProtocolClaude          Protocol = "claude"
	ProtocolGemini          Protocol = "gemini"
)

const (
	// maxSearchDepth 限制在 JSON 树中搜索 usage 容器的深度，防御恶意深嵌套。
	maxSearchDepth = 12
	// MaxBodyBytes 是单次解析接受的最大字节数。
	MaxBodyBytes = 8 << 20
)

// Usage 是一次请求的 token 用量，字段为上游原样报告值。
type Usage struct {
	// Protocol 是识别出的协议形状（尽力而为，可能为 ProtocolUnknown）。
	Protocol Protocol `json:"protocol,omitempty"`

	// InputTokens 是上游报告的输入 token：
	// OpenAI prompt_tokens / Claude input_tokens / Gemini promptTokenCount。
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens 是输出 token。
	OutputTokens int64 `json:"output_tokens"`
	// ReasoningTokens 是推理/思考 token，通常已计入 OutputTokens，仅供展示。
	ReasoningTokens int64 `json:"reasoning_tokens"`
	// CachedTokens 是 OpenAI/Gemini 口径的缓存命中数（含在 InputTokens 内）。
	CachedTokens int64 `json:"cached_tokens"`
	// CacheReadTokens 是 Claude 口径的缓存读取 token（不含在 InputTokens 内）。
	CacheReadTokens int64 `json:"cache_read_tokens"`
	// CacheCreationTokens 是写入缓存的 token。
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	// TotalTokens 是上游报告的合计（可能缺失）。
	TotalTokens int64 `json:"total_tokens"`

	// InputIncludesCache 标记 InputTokens 是否已包含缓存命中 token。
	InputIncludesCache bool `json:"input_includes_cache"`
	// ReasoningInOutput 标记 ReasoningTokens 是否已包含在 OutputTokens 内。
	// OpenAI / Claude 为 true（推理 token 计入输出）；
	// Gemini 为 false（thoughtsTokenCount 与 candidatesTokenCount 并列），
	// 该差异直接影响计费输出量，故显式建模。
	ReasoningInOutput bool `json:"reasoning_in_output"`
	// ImageCount 用于按张计价的图像/视频请求；普通 token 协议通常为 0。
	ImageCount int64 `json:"image_count,omitempty"`
}

// Billable 是归一化后的计费口径：四类互不重叠，可直接乘以对应单价。
// 锁定决策：只对这四类计价。
type Billable struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
}

// Sum 返回四类计费 token 之和。
func (b Billable) Sum() int64 {
	total := addNonNegSat(b.Input, b.Output)
	total = addNonNegSat(total, b.CacheRead)
	return addNonNegSat(total, b.CacheCreation)
}

// Billable 把上游原样字段归一化为互不重叠的四类计费 token。
//
// 对 inclusive 口径（OpenAI/Gemini），从输入中扣除缓存命中部分；
// 对 exclusive 口径（Claude），输入原样保留。缓存读取合并为一项。
func (u Usage) Billable() Billable {
	cacheRead := u.CacheReadTokens
	input := u.InputTokens
	if u.InputIncludesCache {
		// 缓存命中已含在输入内，拆出来单独计价，避免按输入价重复收费。
		cacheRead += u.CachedTokens
		input -= u.CachedTokens
		if input < 0 {
			// 上游数据自相矛盾（cached > prompt）时保守归零，不产生负费用。
			input = 0
		}
	} else if u.CachedTokens > 0 && u.CacheReadTokens == 0 {
		// exclusive 口径下若只给了 cached_tokens，按缓存读取计。
		cacheRead = u.CachedTokens
	}
	output := u.OutputTokens
	if !u.ReasoningInOutput {
		// 推理 token 未计入输出（Gemini 口径），需并入后按输出价计费。
		output = addNonNegSat(output, u.ReasoningTokens)
	}
	return Billable{
		Input:         nonNeg(input),
		Output:        nonNeg(output),
		CacheRead:     nonNeg(cacheRead),
		CacheCreation: nonNeg(u.CacheCreationTokens),
	}
}

// IsZero 报告是否完全没有解析到任何用量。
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.ReasoningTokens == 0 &&
		u.CachedTokens == 0 && u.CacheReadTokens == 0 && u.CacheCreationTokens == 0 &&
		u.TotalTokens == 0 && u.ImageCount == 0
}

// EffectiveTotal 返回合计 token：优先用上游报告值，缺失时按各项相加。
func (u Usage) EffectiveTotal() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	b := u.Billable()
	return b.Sum()
}

// merge 把 other 合并进 u，逐字段取较大值。
//
// 流式响应中各协议的用量是累计上报的（Claude 在 message_start 给输入、
// message_delta 给累计输出；OpenAI 在末帧给完整 usage；Gemini 每帧累计），
// 因此逐字段取最大值是安全且协议无关的合并方式。
func (u *Usage) merge(other Usage) {
	u.InputTokens = maxI64(u.InputTokens, other.InputTokens)
	u.OutputTokens = maxI64(u.OutputTokens, other.OutputTokens)
	u.ReasoningTokens = maxI64(u.ReasoningTokens, other.ReasoningTokens)
	u.CachedTokens = maxI64(u.CachedTokens, other.CachedTokens)
	u.CacheReadTokens = maxI64(u.CacheReadTokens, other.CacheReadTokens)
	u.CacheCreationTokens = maxI64(u.CacheCreationTokens, other.CacheCreationTokens)
	u.TotalTokens = maxI64(u.TotalTokens, other.TotalTokens)
	u.ImageCount = maxI64(u.ImageCount, other.ImageCount)
	if u.Protocol == ProtocolUnknown {
		u.Protocol = other.Protocol
	}
	// 一旦某帧确定了 inclusive 口径，就保持该口径。
	if other.InputIncludesCache {
		u.InputIncludesCache = true
	}
	if other.ReasoningInOutput {
		u.ReasoningInOutput = true
	}
}

// rawOpenAIClaude 覆盖 OpenAI Chat / OpenAI Responses / Claude 三种 snake_case 形状。
// 全部字段用指针以区分「未出现」与「显式 0」。
type rawOpenAIClaude struct {
	// OpenAI Chat Completions
	PromptTokens      *int64         `json:"prompt_tokens"`
	CompletionTokens  *int64         `json:"completion_tokens"`
	TotalTokens       *int64         `json:"total_tokens"`
	PromptDetails     *cachedDetails `json:"prompt_tokens_details"`
	CompletionDetails *reasonDetails `json:"completion_tokens_details"`

	// OpenAI Responses / Claude Messages 共用 input_tokens / output_tokens
	InputTokens   *int64         `json:"input_tokens"`
	OutputTokens  *int64         `json:"output_tokens"`
	InputDetails  *cachedDetails `json:"input_tokens_details"`
	OutputDetails *reasonDetails `json:"output_tokens_details"`

	// Claude 专有
	CacheCreationInputTokens *int64           `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64           `json:"cache_read_input_tokens"`
	CacheCreation            map[string]int64 `json:"cache_creation"`
}

type cachedDetails struct {
	CachedTokens *int64 `json:"cached_tokens"`
	// Gemini/OpenAI 少数版本使用 cache_read_input_tokens 作为明细键。
	CacheReadInputTokens *int64 `json:"cache_read_input_tokens"`
}

type reasonDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}

// rawGemini 覆盖 Gemini usageMetadata（camelCase 与 snake_case 两种拼写）。
type rawGemini struct {
	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	TotalTokenCount         *int64 `json:"totalTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount"`

	PromptTokenCountSnake        *int64 `json:"prompt_token_count"`
	CandidatesTokenCountSnake    *int64 `json:"candidates_token_count"`
	TotalTokenCountSnake         *int64 `json:"total_token_count"`
	CachedContentTokenCountSnake *int64 `json:"cached_content_token_count"`
	ThoughtsTokenCountSnake      *int64 `json:"thoughts_token_count"`
}

// ParseJSON 从一段 JSON（完整响应体或单个流式事件）中解析用量。
// 返回 ok=false 表示未找到任何 usage 容器。
func ParseJSON(body []byte) (Usage, bool) {
	if len(body) == 0 || len(body) > MaxBodyBytes {
		return Usage{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Usage{}, false
	}
	var acc Usage
	found := false
	for _, c := range findUsageContainers(trimmed, 0) {
		if u, ok := decodeContainer(c.raw, c.gemini); ok {
			acc.merge(u)
			found = true
		}
	}
	if !found {
		return Usage{}, false
	}
	return acc, true
}

// container 是搜索到的一个 usage 对象。
type container struct {
	raw    json.RawMessage
	gemini bool
}

// findUsageContainers 在 JSON 树中递归查找 usage / usageMetadata 容器。
//
// 递归而非只看顶层，是为了同时覆盖 Claude 的 {"message":{"usage":…}}、
// Responses 的 {"response":{"usage":…}} 等包裹形状。
func findUsageContainers(raw json.RawMessage, depth int) []container {
	if depth > maxSearchDepth {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	var out []container
	for k, v := range obj {
		switch k {
		case "usage":
			out = append(out, container{raw: v, gemini: false})
			continue
		case "usageMetadata", "usage_metadata":
			out = append(out, container{raw: v, gemini: true})
			continue
		}
		// 只对对象继续下探；数组与标量不含 usage 容器。
		if t := bytes.TrimSpace(v); len(t) > 0 && t[0] == '{' {
			out = append(out, findUsageContainers(v, depth+1)...)
		}
	}
	return out
}

// decodeContainer 解码单个 usage 容器。
func decodeContainer(raw json.RawMessage, gemini bool) (Usage, bool) {
	if gemini {
		return decodeGemini(raw)
	}
	return decodeOpenAIClaude(raw)
}

func decodeGemini(raw json.RawMessage) (Usage, bool) {
	var g rawGemini
	if err := json.Unmarshal(raw, &g); err != nil {
		return Usage{}, false
	}
	prompt := firstNonNil(g.PromptTokenCount, g.PromptTokenCountSnake)
	cand := firstNonNil(g.CandidatesTokenCount, g.CandidatesTokenCountSnake)
	total := firstNonNil(g.TotalTokenCount, g.TotalTokenCountSnake)
	cached := firstNonNil(g.CachedContentTokenCount, g.CachedContentTokenCountSnake)
	thoughts := firstNonNil(g.ThoughtsTokenCount, g.ThoughtsTokenCountSnake)
	if prompt == nil && cand == nil && total == nil && cached == nil && thoughts == nil {
		return Usage{}, false
	}
	return Usage{
		Protocol:     ProtocolGemini,
		InputTokens:  deref(prompt),
		OutputTokens: deref(cand),
		// thoughtsTokenCount 与 candidatesTokenCount 并列，未含在输出内。
		ReasoningTokens:   deref(thoughts),
		ReasoningInOutput: false,
		CachedTokens:      deref(cached),
		TotalTokens:       deref(total),
		// promptTokenCount 已包含 cachedContentTokenCount。
		InputIncludesCache: true,
	}, true
}

func decodeOpenAIClaude(raw json.RawMessage) (Usage, bool) {
	var r rawOpenAIClaude
	if err := json.Unmarshal(raw, &r); err != nil {
		return Usage{}, false
	}
	u := Usage{}
	isClaude := r.CacheCreationInputTokens != nil || r.CacheReadInputTokens != nil || r.CacheCreation != nil

	switch {
	case r.PromptTokens != nil || r.CompletionTokens != nil:
		// OpenAI Chat Completions：prompt_tokens 含缓存命中，reasoning 含在输出内。
		u.Protocol = ProtocolOpenAIChat
		u.InputTokens = deref(r.PromptTokens)
		u.OutputTokens = deref(r.CompletionTokens)
		u.InputIncludesCache = true
		u.ReasoningInOutput = true
		if r.PromptDetails != nil {
			u.CachedTokens = deref(firstNonNil(r.PromptDetails.CachedTokens, r.PromptDetails.CacheReadInputTokens))
		}
		if r.CompletionDetails != nil {
			u.ReasoningTokens = deref(r.CompletionDetails.ReasoningTokens)
		}
	case isClaude:
		// Claude Messages：input_tokens 不含 cache_read / cache_creation；
		// thinking token 计入 output_tokens。
		u.Protocol = ProtocolClaude
		u.InputTokens = deref(r.InputTokens)
		u.OutputTokens = deref(r.OutputTokens)
		u.InputIncludesCache = false
		u.ReasoningInOutput = true
		u.setClaudeCache(r)
	case r.InputTokens != nil || r.OutputTokens != nil:
		// OpenAI Responses：input_tokens 含缓存命中，reasoning 含在输出内。
		//
		// 注意：无缓存字段的 Claude 响应形状与此完全相同，会被归到本分支。
		// 由于此时 CachedTokens 必为 0，Billable() 结果一致，仅 Protocol 标签
		// 可能不准，不影响计费正确性。
		u.Protocol = ProtocolOpenAIResponses
		u.InputTokens = deref(r.InputTokens)
		u.OutputTokens = deref(r.OutputTokens)
		u.InputIncludesCache = true
		u.ReasoningInOutput = true
		if r.InputDetails != nil {
			u.CachedTokens = deref(firstNonNil(r.InputDetails.CachedTokens, r.InputDetails.CacheReadInputTokens))
		}
		if r.OutputDetails != nil {
			u.ReasoningTokens = deref(r.OutputDetails.ReasoningTokens)
		}
	default:
		return Usage{}, false
	}
	u.TotalTokens = deref(r.TotalTokens)
	return u, true
}

// setClaudeCache 填充 Claude 的缓存读写字段。
// cache_creation_input_tokens 与 cache_creation 明细对象是冗余表示，
// 优先取标量以避免重复计数。
func (u *Usage) setClaudeCache(r rawOpenAIClaude) {
	u.CacheReadTokens = deref(r.CacheReadInputTokens)
	if r.CacheCreationInputTokens != nil {
		u.CacheCreationTokens = nonNeg(*r.CacheCreationInputTokens)
		return
	}
	var sum int64
	for _, v := range r.CacheCreation {
		sum = addNonNegSat(sum, v)
	}
	u.CacheCreationTokens = sum
}

// ParseSSE 解析一段 SSE 流（可为完整流），合并其中所有事件的用量。
func ParseSSE(body []byte) (Usage, bool) {
	var acc Accumulator
	for _, payload := range SSEPayloads(body) {
		acc.Feed(payload)
	}
	return acc.Result()
}

// Parse 自动判别 JSON 与 SSE 两种载荷形式并解析。
func Parse(body []byte) (Usage, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return Usage{}, false
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if u, ok := ParseJSON(trimmed); ok {
			return u, true
		}
	}
	return ParseSSE(body)
}

// SSEPayloads 按 SSE 规范提取所有事件的 data 载荷。
// 同一事件内的多行 data 以换行拼接；[DONE] 哨兵被跳过。
func SSEPayloads(body []byte) [][]byte {
	if len(body) == 0 || len(body) > MaxBodyBytes {
		return nil
	}
	var out [][]byte
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		joined := strings.Join(cur, "\n")
		cur = cur[:0]
		t := strings.TrimSpace(joined)
		if t == "" || t == "[DONE]" {
			return
		}
		out = append(out, []byte(t))
	}
	// 统一换行，逐行扫描。
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	for line := range bytes.SplitSeq(normalized, []byte("\n")) {
		s := string(line)
		if s == "" {
			// 空行表示事件结束。
			flush()
			continue
		}
		if strings.HasPrefix(s, ":") {
			// 注释行。
			continue
		}
		field, value, found := strings.Cut(s, ":")
		if !found {
			continue
		}
		if field != "data" {
			// event / id / retry 等字段与用量无关。
			continue
		}
		cur = append(cur, strings.TrimPrefix(value, " "))
	}
	flush()
	return out
}

// Accumulator 增量累积流式响应中的用量，可安全地逐事件喂入。
// 非并发安全，调用方需自行串行化。
type Accumulator struct {
	usage Usage
	found bool
	// model 是响应载荷里上游声明的模型名（二次路由后可能与请求别名不同）。
	model string
}

// Feed 喂入一个事件载荷（JSON 对象）。非 JSON 或无 usage 的载荷被忽略。
func (a *Accumulator) Feed(payload []byte) {
	a.sniffModel(payload)
	if u, ok := ParseJSON(payload); ok {
		a.usage.merge(u)
		a.found = true
	}
}

// sniffModel 抓取载荷顶层的 "model" 字段；拿到一次后不再重复解析，
// 流式场景下每个 chunk 都带同名字段，首块即可命中。
func (a *Accumulator) sniffModel(payload []byte) {
	if a.model != "" || len(payload) == 0 || payload[0] != '{' {
		return
	}
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &probe) == nil && strings.TrimSpace(probe.Model) != "" {
		a.model = strings.TrimSpace(probe.Model)
	}
}

// FeedSSE 喂入一段原始 SSE 文本（可能含多个事件）。
func (a *Accumulator) FeedSSE(chunk []byte) {
	for _, p := range SSEPayloads(chunk) {
		a.Feed(p)
	}
}

// FeedChunk 喂入宿主 stream_read 交付的一段流载荷。
//
// 宿主交付的形状不止一种：openai→openai 直通翻译会剥掉 SSE 的 data: 前缀并
// 丢弃 [DONE]（裸 JSON 对象），其他协议组合则保留完整 SSE 帧，载荷还可能在
// 帧边界被拆分。这里先按整段解析（JSON 或 SSE），失败再逐行重试，
// 覆盖多个裸 JSON 对象拼接在同一段载荷里的情况。merge 取较大值，重复喂入无害。
func (a *Accumulator) FeedChunk(payload []byte) {
	a.sniffModel(payload)
	if u, ok := Parse(payload); ok {
		a.usage.merge(u)
		a.found = true
		return
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		if u, ok := Parse(bytes.TrimSpace(line)); ok {
			a.usage.merge(u)
			a.found = true
		}
	}
}

// Model 返回响应载荷里上游声明的模型名；未出现过则为空串。
func (a *Accumulator) Model() string { return a.model }

// SniffModel 提取单个载荷顶层 "model" 字段（非流式响应体的一次性用法）。
func SniffModel(payload []byte) string {
	a := &Accumulator{}
	a.sniffModel(payload)
	return a.model
}

// Result 返回累积结果；ok=false 表示整个流都没有用量信息。
func (a *Accumulator) Result() (Usage, bool) {
	if !a.found {
		return Usage{}, false
	}
	return a.usage, true
}

// Reset 清空累积状态以便复用。
func (a *Accumulator) Reset() { a.usage, a.found = Usage{}, false }

func firstNonNil(vals ...*int64) *int64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return nonNeg(*p)
}

// nonNeg 把负值归零：上游偶发的负 token 数没有业务含义，且会污染费用。
func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// addNonNegSat 在不可信上游数值相加时避免 int64 回绕。
// 用量只接受非负 token，超出可表示范围时钳位到 MaxInt64，交给上层再决定如何处理。
func addNonNegSat(a, b int64) int64 {
	a = nonNeg(a)
	b = nonNeg(b)
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
