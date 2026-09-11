// Package service 的请求路径辅助：身份解析、预占估算、心跳。
//
// 本文件服务于 quota.enabled=true 时的宿主请求路径：
//
//	frontend_auth.authenticate → model.route → executor.execute(_stream) →
//	预占 → host.model.execute(_stream) → 解析 usage → 结算。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// CallerScopeMetadataKey 是宿主 model.route 元数据里可能携带的 caller_scope 键。
// 本插件的主身份来源是 Authorization: Bearer cum-…，caller_scope 仅作兜底。
const CallerScopeMetadataKey = "caller_scope"

// ErrModelDisabled 表示命中了一条被禁用的计价规则。
var ErrModelDisabled = errors.New("service: 模型计价规则已禁用")

// ReservePlan 是单次请求预占的估算结果。
type ReservePlan struct {
	Model          string
	PricingRuleID  int64
	BillingMode    string
	InputEstimate  int64
	OutputEstimate int64
	TokenEstimate  int64
	ImageCount     int64

	// Rule / Priced 是预占实际采用的计价规则（Priced=false 表示未命中任何
	// 非兜底规则）。别名流量 mode=target 时执行器据此构造 PricingOverride，
	// 免去 Reserve 重复匹配。
	Rule   store.PricingRule
	Priced bool

	// Meta 是请求体的单次解析结果：执行器入口解析一次，
	// 预占估算与结算落库（tier/thinking_intensity）共用，不再重复整包反序列化。
	Meta RequestMeta
}

// heartbeatInterval 是集中式预占心跳的批量续期间隔。预占默认过期阈值 2h，
// 30s 的续期粒度远低于它；全部活跃预占合并为每轮一个写事务。
const heartbeatInterval = 30 * time.Second

// TrackReservation 登记一条在途预占，由服务内唯一的后台协程按
// heartbeatInterval 批量续期心跳（替代每请求一个 ticker goroutine +
// 每分钟一次独立写事务）。返回的 stop 函数注销该预占，结算/释放后必须调用。
func (s *Service) TrackReservation(id string) (stop func()) {
	s.beatsMu.Lock()
	if s.beats == nil {
		s.beats = make(map[string]struct{})
	}
	s.beats[id] = struct{}{}
	if !s.beatsStarted {
		s.beatsStarted = true
		go s.reservationBeatLoop()
	}
	s.beatsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.beatsMu.Lock()
			delete(s.beats, id)
			s.beatsMu.Unlock()
		})
	}
}

func (s *Service) reservationBeatLoop() {
	s.beatsMu.Lock()
	stop := s.beatsStop
	s.beatsMu.Unlock()
	if stop == nil {
		// Close 已执行（reconfigure 换新 Service 的竞态尾部）：立即退出。
		return
	}
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		s.flushKeyTouches()
		s.beatsMu.Lock()
		ids := make([]string, 0, len(s.beats))
		for id := range s.beats {
			ids = append(ids, id)
		}
		s.beatsMu.Unlock()
		if len(ids) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = s.st.TouchReservations(ctx, ids)
		cancel()
	}
}

// queueKeyTouch 把鉴权成功的 Key 记入挂起表，由集中心跳协程批量落库。
// last_used_at 只服务面板展示，≤一个心跳周期的延迟可接受；
// 换来的是鉴权热路径上零写事务。进程意外退出丢失尾部更新，无碍。
func (s *Service) queueKeyTouch(kid string) {
	now := time.Now().UnixMilli()
	s.touchMu.Lock()
	if s.touchPending == nil {
		s.touchPending = make(map[string]int64)
	}
	s.touchPending[kid] = now
	s.touchMu.Unlock()
	// 纯鉴权（无在途预占）场景也要有心跳协程来刷挂起表。
	s.beatsMu.Lock()
	start := !s.beatsStarted
	s.beatsStarted = true
	s.beatsMu.Unlock()
	if start {
		go s.reservationBeatLoop()
	}
}

func (s *Service) flushKeyTouches() {
	s.touchMu.Lock()
	pending := s.touchPending
	s.touchPending = nil
	s.touchMu.Unlock()
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = s.st.TouchKeysLastUsed(ctx, pending)
	cancel()
}

// ResolveIdentity 从请求头解析插件 Key。
//
// 主路径：Authorization: Bearer cum-…（前端独占鉴权已通过，这里再次校验以拿到 Key 记录）。
// 兜底：model.route 元数据里的 caller_scope（宿主在未透传原始头时使用）。
func (s *Service) ResolveIdentity(ctx context.Context, headers http.Header, metadata map[string]any) (store.PluginKey, error) {
	if raw := bearerToken(headers); raw != "" {
		a, err := s.Authenticate(ctx, raw)
		if err == nil {
			return a.Record, nil
		}
		return store.PluginKey{}, err
	}
	if scope := metadataString(metadata, CallerScopeMetadataKey); scope != "" {
		return s.st.FindKeyByCallerScope(ctx, scope)
	}
	return store.PluginKey{}, fmt.Errorf("%w: 缺少 Bearer 身份", ErrInvalidKey)
}

// RequestMeta 是请求体中与额度估算、落库维度相关的字段的单次解析结果。
// 此前同一请求体在一次执行器调用中被整包 Unmarshal 到 map 最多四次
// （估算 token、提模型、提 tier、提 reasoning_effort）；长上下文/带图请求
// 可达数 MB，这里改为入口一次类型化解析后全程复用。
type RequestMeta struct {
	// BodyLen 是原始请求体长度（输入 token 按混合密度估算，见 estimateInputTokens）。
	BodyLen int

	Model           string          `json:"model"`
	N               json.RawMessage `json:"n"`
	ServiceTier     string          `json:"service_tier"`
	Tier            string          `json:"tier"`
	ReasoningEffort string          `json:"reasoning_effort"`

	// Tools / System / SystemInstruction 只做存在性探测（路由规则的
	// has_tools / has_system 变量）：RawMessage 零拷贝切片，无额外解析开销。
	// system 覆盖 Claude 顶层 system 与 Gemini systemInstruction；OpenAI 的
	// system role 藏在 messages 数组里，不做逐元素重解析（热路径成本），不探测。
	Tools             json.RawMessage `json:"tools"`
	System            json.RawMessage `json:"system"`
	SystemInstruction json.RawMessage `json:"systemInstruction"`

	// InputEstimate 是 ParseRequestMeta 时按混合密度算好的输入 token 估算
	// （ASCII/4 + 多字节/3）：tokenEstimates 与路由 env 的 input_tokens 都
	// 用它，Meta 离开请求体作用域后不再重扫 body。
	InputEstimate int64

	HasTools  bool
	HasSystem bool

	MaxTokens        *int64               `json:"max_tokens"`
	MaxCompletion    *int64               `json:"max_completion_tokens"`
	MaxOutput        *int64               `json:"max_output_tokens"`
	GenerationConfig generationConfigMeta `json:"generationConfig"`
	Reasoning        effortMeta           `json:"reasoning"`
	Thinking         effortMeta           `json:"thinking"`

	// ResolvedTier / ResolvedThinking 是落库展示字段，解析时按既有优先序归并：
	// tier 取 service_tier → tier；推理强度取 reasoning.effort → thinking.effort
	// → reasoning_effort。
	ResolvedTier     string
	ResolvedThinking string
}

type generationConfigMeta struct {
	MaxOutputTokens *int64 `json:"maxOutputTokens"`
}

type effortMeta struct {
	Effort string `json:"effort"`
}

// ParseRequestMeta 对请求体做一次类型化解析。body 非 JSON 对象时除
// BodyLen/InputEstimate 外返回零值（与旧实现的 map 解析失败分支等价）。
func ParseRequestMeta(body []byte) RequestMeta {
	m := RequestMeta{BodyLen: len(body), InputEstimate: estimateInputTokens(body)}
	if len(body) == 0 || json.Unmarshal(body, &m) != nil {
		return RequestMeta{BodyLen: len(body), InputEstimate: estimateInputTokens(body)}
	}
	m.Model = strings.TrimSpace(m.Model)
	m.ResolvedTier = FirstNonEmpty(m.ServiceTier, m.Tier)
	m.ResolvedThinking = FirstNonEmpty(m.Reasoning.Effort, m.Thinking.Effort, m.ReasoningEffort)
	m.HasTools = rawNonEmpty(m.Tools)
	m.HasSystem = rawNonEmpty(m.System) || rawNonEmpty(m.SystemInstruction)
	m.InputEstimate = estimateInputTokens(body)
	return m
}

// rawNonEmpty 判定 RawMessage 是否承载实际值：null 与缺省/空容器都算空。
func rawNonEmpty(raw json.RawMessage) bool {
	if len(raw) <= 2 {
		return false
	}
	return string(raw) != "null"
}

// estimateInputTokens 按混合密度估算输入 token：ASCII 字节 /4、多字节
// 序列 /3（UTF-8 中文 3 字节、emoji 4 字节，多字节文本实测 2.5–3 字节/token）。
// 此前的整包 /3 对英文/代码为主的 JSON 请求体高估约 60%（实测 626KB 体
// /3 得 209K，真实输入 token 约 130K）。逐字节扫描成本 O(body)，与原先
// 的整除同级。
func estimateInputTokens(body []byte) int64 {
	var ascii, multi int64
	for _, b := range body {
		if b < 0x80 {
			ascii++
		} else {
			multi++
		}
	}
	return ascii/4 + multi/3 + 1
}

// tokenEstimates 按锁定决策估算输入/输出上限：输入走 estimateInputTokens
// （混合密度），输出取 max_tokens / max_completion_tokens / max_output_tokens /
// generationConfig.maxOutputTokens 中首个存在者（否则 defaultOutput），均封顶 max。
func (m RequestMeta) tokenEstimates(defaultOutput, max int64) (in, out int64) {
	in = m.InputEstimate
	if in > max {
		in = max
	}
	out = defaultOutput
	for _, v := range []*int64{m.MaxTokens, m.MaxCompletion, m.MaxOutput, m.GenerationConfig.MaxOutputTokens} {
		if v != nil {
			out = *v
			break
		}
	}
	if out < 0 {
		out = 0
	}
	if out > max {
		out = max
	}
	return in, out
}

// imageCount 返回按张计价请求的图片张数（缺省或非法时为 1）。
func (m RequestMeta) imageCount() int64 {
	if m.N == nil {
		return 1
	}
	var n int64
	if err := json.Unmarshal(m.N, &n); err != nil || n < 1 {
		return 1
	}
	return n
}

// BuildReservePlan 按模型计价规则与请求体估算预占额度。
//
// 锁定决策：预占使用保守上限——输入按混合密度估算（ASCII/4 + 多字节/3），
// 输出按该模型最近成功请求的输出 P95 校准（无历史时回退保守口径），
// 均封顶 max_token_estimate；金额按分档价计：输入估算按输入侧最贵档
// （输入/缓存读/缓存写），输出估算按输出价；不再把整段估算按四档最高价
// 计（那是长上下文请求预占虚高的主因）。
func (s *Service) BuildReservePlan(ctx context.Context, model string, body []byte) (ReservePlan, error) {
	return s.buildPlanFromMeta(ctx, model, ParseRequestMeta(body), model)
}

// BuildReservePlanWithPricing 是别名路由的变体：计价按 pricingModel 匹配，
// plan.Model 仍为 model（集合别名，维度统计锚点）。
func (s *Service) BuildReservePlanWithPricing(ctx context.Context, model string, body []byte, pricingModel string) (ReservePlan, error) {
	return s.buildPlanFromMeta(ctx, model, ParseRequestMeta(body), pricingModel)
}

// BuildReservePlanFromMeta 接受调用方已解析好的元数据：路由路径在执行器入口
// 只解析一次请求体，这里不再重复 O(body) 扫描。
func (s *Service) BuildReservePlanFromMeta(ctx context.Context, model string, meta RequestMeta, pricingModel string) (ReservePlan, error) {
	return s.buildPlanFromMeta(ctx, model, meta, pricingModel)
}

// ---------- 输出预占的历史校准 ----------

// outputCalTTL / outputCalSamples 是输出 P95 校准的缓存窗口与样本量；
// densityCalTTL / densityCalSamples 是输入密度学习的同款参数；
// shareCalTTL / shareCalSamples 是缓存份额学习（金额拆档）的同款参数。
const (
	outputCalTTL     = 60 * time.Second
	outputCalSamples = 500

	densityCalTTL     = 60 * time.Second
	densityCalSamples = 200

	shareCalTTL     = 60 * time.Second
	shareCalSamples = 200
)

// outputCalEntry 是 outCal 缓存桶：p95 为加权分位值，ok=false 表示
// 样本不足（<3 条成功请求），调用方应回退保守口径而非使用 p95=0。
type outputCalEntry struct {
	p95 int64
	ok  bool
	at  time.Time
}

// densityCalEntry 是 denCal 缓存桶：milliDensity 是该模型近期采纳的
// body_len÷input_tokens 密度 ×1000（7.3 字节/token 存 7300）；
// mad 是全窗样本绝对中位差（同为毫单位）；ok=false 表示无有效样本，
// 输入估算回退固定混合密度。drifted 标记近期窗口判定组成突变、
// 已改跟近期密度（agent compact 场景）。
type densityCalEntry struct {
	milliDensity int64
	mad          int64
	drifted      bool
	ok           bool
	at           time.Time
}

// cacheShareCalEntry 是 shareCal 缓存桶：readBP/createBP 是该模型近期
// 缓存读/写占完整上下文的份额（万分比，token 加权），供预占金额拆档；
// ok=false 表示无有效样本，金额回退「输入侧最贵档」的保守口径。
type cacheShareCalEntry struct {
	readBP   int64
	createBP int64
	ok       bool
	at       time.Time
}

// calibratedOutput 输出预占的历史校准（service 级 60s 缓存，按模型分桶）。
// maxOut 是 tokenEstimates 的原口径（max_tokens 或 defaultOutputReserve）。
// 有该模型的成功历史时取输出 P95（近端加权：新样本 1.5×，适应 agent 输出
// 水平的变化），min(max_tokens, P95×1.25) 保底不越过客户端显式允许的上限；
// 无历史（新模型/空库）回退 min(maxOut, max(defaultOutput, maxOut/8))——
// agent 客户端的 max_tokens 普遍是拍脑袋的大数（ZCode 128000，实际输出
// 几 K），1/8 已数倍于常见实际输出；defaultOutput 抬底保证无 max_tokens
// 的普通请求不受影响。
func (s *Service) calibratedOutput(model string, maxOut, defaultOutput int64) int64 {
	p95, ok := s.outputP95Cached(model)
	if !ok {
		if maxOut <= defaultOutput {
			return maxOut
		}
		fallback := maxOut / 8
		if fallback < defaultOutput {
			fallback = defaultOutput
		}
		if fallback > maxOut {
			fallback = maxOut
		}
		return fallback
	}
	reserve := p95 + p95/4 // ×1.25 余量吸收输出长尾
	if reserve < defaultOutput {
		reserve = defaultOutput
	}
	if maxOut > 0 && reserve > maxOut {
		reserve = maxOut
	}
	return reserve
}

// outputP95Cached 返回模型最近成功输出的 P95（近端加权）与是否可用。
// 缓存 60s：校准查询走 (model,result,ts) 索引取 500 行，每次预占都查会把
// 热路径的读放大一截；60s 内同一模型共享一份样本，结算路径写入新样本后
// 自然在下个窗口生效。
func (s *Service) outputP95Cached(model string) (int64, bool) {
	now := time.Now()
	s.outCalMu.Lock()
	if s.outCal != nil {
		if e, ok := s.outCal[model]; ok && now.Sub(e.at) < outputCalTTL {
			s.outCalMu.Unlock()
			return e.p95, e.ok
		}
	}
	s.outCalMu.Unlock()
	// 查询失败（库瞬时忙等）时样本为空，按「样本不足」回退保守口径，不阻断预占。
	samples, _ := s.st.RecentOutputTokens(context.Background(), model, outputCalSamples)
	p95, ok := outputP95(samples)
	s.outCalMu.Lock()
	if s.outCal == nil {
		s.outCal = make(map[string]outputCalEntry)
	}
	s.outCal[model] = outputCalEntry{p95: p95, ok: ok, at: now}
	s.outCalMu.Unlock()
	return p95, ok
}

// outputP95 对升序样本取近端加权 P95：最新一半样本权重 1.5×（时间倒序
// 取出后升序排列，数组后半段是较新样本）。样本 <3 条视为不可用——
// 一两次请求说明不了输出水平，回退保守口径。
func outputP95(samples []int64) (int64, bool) {
	n := len(samples)
	if n < 3 {
		return 0, false
	}
	var total float64
	for range samples {
		total += 1.0
	}
	// 近端加权：后半段（较新）样本 1.5×。
	for i := n / 2; i < n; i++ {
		total += 0.5
	}
	target := total * 0.95
	var acc float64
	for i, v := range samples {
		w := 1.0
		if i >= n/2 {
			w = 1.5
		}
		acc += w
		if acc >= target {
			return v, true
		}
	}
	return samples[n-1], true
}

func (s *Service) buildPlanFromMeta(ctx context.Context, model string, meta RequestMeta, pricingModel string) (ReservePlan, error) {
	model = FirstNonEmpty(model, meta.Model)
	rule, priced, err := s.matchPricing(ctx, pricingModel)
	if err != nil {
		return ReservePlan{}, err
	}
	if !rule.Enabled {
		return ReservePlan{}, ErrModelDisabled
	}
	plan := ReservePlan{Model: model, PricingRuleID: rule.ID, BillingMode: rule.BillingMode, Rule: rule, Priced: priced, Meta: meta}
	switch rule.BillingMode {
	case store.BillingModePerImage:
		plan.ImageCount = meta.imageCount()
		return plan, nil
	case store.BillingModeFree:
		plan.TokenEstimate = 1
		return plan, nil
	}
	in, out := meta.tokenEstimates(s.cfg.Quota.Limits.DefaultOutputReserve, s.cfg.Quota.Limits.MaxTokenEstimate)
	// 输出预占历史校准：max_tokens 是客户端允许的上限，不是预期输出——
	// agent 客户端普遍拍脑袋给 128000，实际输出几 K，全额预占让
	// 「最近预占」实际占比长期 27% 上下（实测 ZCode 流量）。改按该模型
	// 最近成功请求的输出 P95 预占，max_tokens 只做封顶；无历史时回退
	// min(max_tokens, p95) 的保守口径：P95 未知时取 max_tokens 的 1/8
	// 与 defaultOutput 的较大者，既不再全额虚占，也覆盖首次请求。
	out = s.calibratedOutput(model, out, s.cfg.Quota.Limits.DefaultOutputReserve)
	// 输入预占密度学习：固定混合密度（ASCII/4+多字节/3）对 agent 长对话
	// 上下文仍高估近一倍（实测 glm-5.3 密度 7.3 字节/token——token 表
	// 内部频率加权后英文语料高于 4 字节/token 的直觉值）。该模型已有
	// body_len÷input 样本时按中位数密度折算；无样本回退混合密度。
	if in2 := s.calibratedInput(model, int64(meta.BodyLen), in, s.cfg.Quota.Limits.MaxTokenEstimate); in2 > 0 {
		in = in2
	}
	plan.InputEstimate = in
	plan.OutputEstimate = out
	plan.TokenEstimate = in + out
	return plan, nil
}

// calibratedInput 输入预占的密度学习：按该模型近期采纳的
// body_len÷input_tokens 密度把当前请求体折算成 token。fallback 是固定
// 混合密度的估算值，无样本（新模型/历史行无 body_len）时原样返回。
//
// 预测时无从得知当前请求的真实密度（token 要结算才有），所以这里没有
// 「逐请求接受带」——旧版按折算值反推等效密度再比对样本带是同义反复
// （反推值恒等于样本密度本身），从不拒绝任何请求，属死代码，已删。
// 组成突变（agent compact 后摘要散文替代原始代码，密度从 ~7.3 漂到
// ~10 字节/token）的适应在**学习侧**完成：EstimateDensity 的近期窗口
// 漂移探测，约 10 条新流量即改跟新密度。这里只保留极宽的绝对 sanity
// 带 [0.2×, 5×]（锚在混合密度估算上），拦「样本与当前请求体构成
// 完全无关」的单位级错误（如上游谎报 token、请求体突变成 base64
// 图片密集型）。
func (s *Service) calibratedInput(model string, bodyLen, fallback, maxEstimate int64) int64 {
	if bodyLen <= 0 {
		return fallback
	}
	milli, _, ok := s.densityMedianCached(model)
	if !ok {
		return fallback
	}
	// 毫密度折算：tokens = bodyLen ÷ (milli/1000) = bodyLen×1000/milli。
	in := bodyLen * 1000 / milli
	if in <= 0 {
		in = 1
	}
	// 绝对 sanity 带：只拦构成级错误，不拦模型间密度差异与正常漂移。
	if aLo, aHi := fallback/5, fallback*5; aHi >= fallback && (in < aLo || in > aHi) {
		return fallback
	}
	if in > maxEstimate {
		in = maxEstimate
	}
	return in
}

// densityMedianCached 返回模型近期采纳的输入密度（×1000 整数）、全窗
// MAD 与是否可用，60s 分桶缓存与输出校准同款。样本 <3 条视为不可用。
// 采纳值含漂移探测（EstimateDensity）：近期窗口中位数漂出全窗带时
// 改跟近期，compact 类组成突变后约 10 条新流量即恢复。
func (s *Service) densityMedianCached(model string) (int64, int64, bool) {
	now := time.Now()
	s.denCalMu.Lock()
	if s.denCal != nil {
		if e, ok := s.denCal[model]; ok && now.Sub(e.at) < densityCalTTL {
			s.denCalMu.Unlock()
			return e.milliDensity, e.mad, e.ok
		}
	}
	s.denCalMu.Unlock()
	// 查询失败（库瞬时忙）时样本为空，按「样本不足」回退混合密度。
	// 学习基线（面板「重置」写入）之后的样本才算数；查基线失败按未重置
	// 处理（宁可沿用旧样本，也不因一次读库抖动回退固定估算）。
	since, _ := s.st.DensityEpoch(context.Background(), model)
	samples, _ := s.st.RecentDensities(context.Background(), model, densityCalSamples, since)
	est, ok := store.EstimateDensity(samples)
	s.denCalMu.Lock()
	if s.denCal == nil {
		s.denCal = make(map[string]densityCalEntry)
	}
	s.denCal[model] = densityCalEntry{milliDensity: est.Milli, mad: est.MAD, drifted: est.Drifted, ok: ok, at: now}
	s.denCalMu.Unlock()
	return est.Milli, est.MAD, ok
}

// cacheSharesCached 返回模型近期缓存读/写占完整上下文的份额（万分比，
// token 加权合计），60s 分桶缓存与密度/输出校准同款；样本 <3 条不可用。
// 该份额只用于预占金额的档位拆分：结算恒按真实 token 逐档计，份额偏差
// 只影响在途预占的金额观感，不影响账本。
func (s *Service) cacheSharesCached(model string) (readBP, createBP int64, ok bool) {
	now := time.Now()
	s.shareCalMu.Lock()
	if s.shareCal != nil {
		if e, hit := s.shareCal[model]; hit && now.Sub(e.at) < shareCalTTL {
			s.shareCalMu.Unlock()
			return e.readBP, e.createBP, e.ok
		}
	}
	s.shareCalMu.Unlock()
	// 学习基线（面板「重置」写入）之后的样本才算数，与密度学习同口径。
	since, _ := s.st.DensityEpoch(context.Background(), model)
	cs, hit, err := s.st.RecentCacheShares(context.Background(), model, shareCalSamples, since)
	ok = err == nil && hit && cs.Samples >= 3
	if ok {
		readBP, createBP = cs.ReadBP, cs.CreateBP
	}
	s.shareCalMu.Lock()
	if s.shareCal == nil {
		s.shareCal = make(map[string]cacheShareCalEntry)
	}
	s.shareCal[model] = cacheShareCalEntry{readBP: readBP, createBP: createBP, ok: ok, at: now}
	s.shareCalMu.Unlock()
	return readBP, createBP, ok
}

func bearerToken(headers http.Header) string {
	if headers == nil {
		return ""
	}
	auth := strings.TrimSpace(headers.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[len("bearer "):])
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok && v != nil {
		if text, ok := v.(string); ok {
			return strings.TrimSpace(text)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

// FirstNonEmpty 返回第一个非空字符串。
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
