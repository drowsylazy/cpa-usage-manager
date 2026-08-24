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
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for range ticker.C {
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

// BuildReservePlan 按模型计价规则与请求体估算预占额度。
//
// 锁定决策：预占使用保守上限（输入按 body 字符数/2+1，输出取 max_tokens 或
// default_output_reserve，均封顶 max_token_estimate），由 Reserve 按规则算出金额。
func (s *Service) BuildReservePlan(ctx context.Context, model string, body []byte) (ReservePlan, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = extractModel(body)
	}
	rule, _, err := s.matchPricing(ctx, model)
	if err != nil {
		return ReservePlan{}, err
	}
	if !rule.Enabled {
		return ReservePlan{}, ErrModelDisabled
	}
	plan := ReservePlan{Model: model, PricingRuleID: rule.ID, BillingMode: rule.BillingMode}
	switch rule.BillingMode {
	case store.BillingModePerImage:
		plan.ImageCount = extractImageCount(body)
		return plan, nil
	case store.BillingModeFree:
		plan.TokenEstimate = 1
		return plan, nil
	}
	in, out := estimateTokens(body, s.cfg.Quota.Limits.DefaultOutputReserve, s.cfg.Quota.Limits.MaxTokenEstimate)
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

// estimateTokens 从请求体估算输入/输出 token 上限。
func estimateTokens(body []byte, defaultOutput, max int64) (int64, int64) {
	input := int64(len(body))/2 + 1
	if input > max {
		input = max
	}
	output := defaultOutput
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if v, ok := jsonNumber(payload, "max_tokens", "max_completion_tokens", "max_output_tokens"); ok {
			output = v
		} else if gen, ok := payload["generationConfig"].(map[string]any); ok {
			if v, ok := jsonNumber(gen, "maxOutputTokens"); ok {
				output = v
			}
		}
	}
	if output < 0 {
		output = 0
	}
	if output > max {
		output = max
	}
	return input, output
}

func extractModel(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if text, ok := payload["model"].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// extractImageCount 读取按张计价请求里的图片张数（默认 1）。
func extractImageCount(body []byte) int64 {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return 1
	}
	switch v := payload["n"].(type) {
	case float64:
		n := int64(v)
		if n < 1 {
			return 1
		}
		return n
	case json.Number:
		if n, err := v.Int64(); err == nil && n >= 1 {
			return n
		}
	}
	return 1
}

func jsonNumber(m map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return int64(v), true
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n, true
			}
		}
	}
	return 0, false
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
