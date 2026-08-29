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
	// BodyLen 是原始请求体长度（输入 token 按 len/2+1 估算）。
	BodyLen int

	Model           string          `json:"model"`
	N               json.RawMessage `json:"n"`
	ServiceTier     string          `json:"service_tier"`
	Tier            string          `json:"tier"`
	ReasoningEffort string          `json:"reasoning_effort"`

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

// ParseRequestMeta 对请求体做一次类型化解析。body 非 JSON 对象时返回零值
// （与旧实现的 map 解析失败分支等价），BodyLen 始终保留。
func ParseRequestMeta(body []byte) RequestMeta {
	m := RequestMeta{BodyLen: len(body)}
	if len(body) == 0 || json.Unmarshal(body, &m) != nil {
		return RequestMeta{BodyLen: len(body)}
	}
	m.Model = strings.TrimSpace(m.Model)
	m.ResolvedTier = FirstNonEmpty(m.ServiceTier, m.Tier)
	m.ResolvedThinking = FirstNonEmpty(m.Reasoning.Effort, m.Thinking.Effort, m.ReasoningEffort)
	return m
}

// tokenEstimates 按锁定决策估算输入/输出上限：输入 body 字符数/2+1，
// 输出取 max_tokens / max_completion_tokens / max_output_tokens /
// generationConfig.maxOutputTokens 中首个存在者（否则 defaultOutput），封顶 max。
func (m RequestMeta) tokenEstimates(defaultOutput, max int64) (in, out int64) {
	in = int64(m.BodyLen)/2 + 1
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
// 锁定决策：预占使用保守上限（输入按 body 字符数/2+1，输出取 max_tokens 或
// default_output_reserve，均封顶 max_token_estimate），由 Reserve 按规则算出金额。
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
	plan.InputEstimate = in
	plan.OutputEstimate = out
	plan.TokenEstimate = in + out
	return plan, nil
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
