package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

// caller_scope 取值。
const (
	// CallerScopeCaller 表示该 Key 的额度归属于其 caller。
	CallerScopeCaller = "caller"
	// CallerScopeKey 表示该 Key 独立计额，不与同 caller 的其他 Key 共享。
	CallerScopeKey = "key"
)

// 预占状态。
const (
	ReservationHeld     = "held"
	ReservationSettled  = "settled"
	ReservationReleased = "released"
)

// 计价规则匹配方式。
const (
	MatchExact  = "exact"
	MatchGlob   = "glob"
	MatchRegexp = "regexp"
)

// 计价规则来源。
const (
	PricingSourceManual    = "manual"
	PricingSourceModelsDev = "models_dev"
)

// 请求结果。
const (
	ResultOK      = "ok"
	ResultError   = "error"
	ResultBlocked = "blocked"
)

// Caller 是归属记录（组织/团队），本身不承载额度。
type Caller struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PluginKey 是一枚插件 Key 的完整记录。
//
// 额度字段为 nil 表示不限。安全材料（KeyHash / EncryptedMaterial）
// 绝不可出现在任何 API 响应中。
type PluginKey struct {
	KID string `json:"kid"`

	// KeyHash 是 HMAC(pepper, 明文) 校验值，仅用于服务端比较。
	KeyHash []byte `json:"-"`
	// EncryptedMaterial 是明文的 AES-GCM 密文，供 keys/reveal 解密回显。
	EncryptedMaterial []byte `json:"-"`
	// PepperID 标识签发时使用的 pepper 代际。
	PepperID string `json:"-"`
	// Fingerprint 是可安全展示的短指纹，用于在面板上区分 Key。
	Fingerprint string `json:"fingerprint"`

	Principal   string `json:"principal"`
	CallerScope string `json:"caller_scope"`
	CallerID    string `json:"caller_id"`
	Label       string `json:"label"`

	Enabled   bool       `json:"enabled"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// 额度上限；nil 表示不限。
	QuotaMicroUSD   *money.Micro `json:"quota_micro_usd,omitempty"`
	DailyMicroUSD   *money.Micro `json:"daily_micro_usd,omitempty"`
	WeeklyMicroUSD  *money.Micro `json:"weekly_micro_usd,omitempty"`
	MonthlyMicroUSD *money.Micro `json:"monthly_micro_usd,omitempty"`

	// Token 上限；nil 表示不限。与金额限额并列生效（任一触顶即拒绝），
	// 口径为计费四类合计（输入+输出+缓存读+缓存写，见 usageparse.Billable）。
	// 金额限额控制成本，token 限额控制用量 —— 混合模型价差大时后者更精确。
	TokenLimit        *int64 `json:"token_limit,omitempty"`
	DailyTokenLimit   *int64 `json:"daily_token_limit,omitempty"`
	WeeklyTokenLimit  *int64 `json:"weekly_token_limit,omitempty"`
	MonthlyTokenLimit *int64 `json:"monthly_token_limit,omitempty"`

	// MaxConcurrentRequests 为 0 表示不限。
	MaxConcurrentRequests int `json:"max_concurrent_requests"`
	// AllowedModels 为空表示不限制模型。
	AllowedModels []string `json:"allowed_models,omitempty"`

	// 持久累计器：已结算金额。周期计数器带 CycleKey，跨期自动归零。
	SpentMicroUSD        money.Micro `json:"spent_micro_usd"`
	DailyCycleKey        string      `json:"daily_cycle_key,omitempty"`
	DailySpentMicroUSD   money.Micro `json:"daily_spent_micro_usd"`
	WeeklyCycleKey       string      `json:"weekly_cycle_key,omitempty"`
	WeeklySpentMicroUSD  money.Micro `json:"weekly_spent_micro_usd"`
	MonthlyCycleKey      string      `json:"monthly_cycle_key,omitempty"`
	MonthlySpentMicroUSD money.Micro `json:"monthly_spent_micro_usd"`

	// Token 累计器；周期归零复用金额那套 *CycleKey（同一次结算两种口径周期必然相同）。
	TokensUsed        int64 `json:"tokens_used"`
	DailyTokensUsed   int64 `json:"daily_tokens_used"`
	WeeklyTokensUsed  int64 `json:"weekly_tokens_used"`
	MonthlyTokensUsed int64 `json:"monthly_tokens_used"`

	// 请求次数上限（日/月）；nil 表示不限。与金额/Token 并列生效，
	// 计数器同样复用 *CycleKey 跨期归零（每成功结算一笔请求 +1）。
	DailyRequestsLimit   *int64 `json:"daily_requests_limit,omitempty"`
	MonthlyRequestsLimit *int64 `json:"monthly_requests_limit,omitempty"`
	DailyRequestsUsed    int64  `json:"daily_requests_used"`
	MonthlyRequestsUsed  int64  `json:"monthly_requests_used"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Revoked 报告 Key 是否已撤销。
func (k *PluginKey) Revoked() bool { return k.RevokedAt != nil }

// Expired 报告 Key 在给定时刻是否已过期。
func (k *PluginKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// Usable 报告 Key 在给定时刻是否可用于鉴权（不含额度判定）。
func (k *PluginKey) Usable(now time.Time) bool {
	return k.Enabled && !k.Revoked() && !k.Expired(now)
}

// ModelAllowed 报告模型是否在该 Key 的可用模型清单内。
// 清单为空表示不限制。清单项支持 glob（`*`/`?`）。
func (k *PluginKey) ModelAllowed(model string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, pat := range k.AllowedModels {
		if MatchGlobPattern(pat, model) {
			return true
		}
	}
	return false
}

// SpentForCycle 返回该 Key 在给定周期标识下的已结算金额。
// 存储的 cycle_key 与当前周期不一致时视为 0（周期已滚动）。
func spentForCycle(storedKey, currentKey string, spent money.Micro) money.Micro {
	if storedKey != currentKey {
		return 0
	}
	return spent
}

// tokensForCycle 与 spentForCycle 同语义，用于 token 累计器：
// 存储的周期标识与当前周期不同说明已跨期，累计值作废按 0 计。
func tokensForCycle(storedKey, currentKey string, used int64) int64 {
	if storedKey != currentKey {
		return 0
	}
	return used
}

// requestsForCycle 与 tokensForCycle 同语义，用于请求次数累计器。
func requestsForCycle(storedKey, currentKey string, used int64) int64 {
	if storedKey != currentKey {
		return 0
	}
	return used
}

// PricingRule 是一条计价规则。价格单位为「每百万 token 的 micro-USD」。
type PricingRule struct {
	ID        int64  `json:"id"`
	MatchKind string `json:"match_kind"`
	Pattern   string `json:"pattern"`
	Priority  int    `json:"priority"`
	Enabled   bool   `json:"enabled"`

	// 计价四档，与 costForRule 的计费口径一一对应。
	// 不设「推理价」与「缓存价」：推理 token 由 usageparse.Billable() 并入输出
	// 按输出价计，上游的 cached_tokens 并入缓存读按缓存读价计 —— 独立档位
	// 无处可用，留着只会让配置者以为设了就生效（v0.3.4 起已从库与 API 移除）。
	PriceInput         money.Price `json:"price_input"`
	PriceOutput        money.Price `json:"price_output"`
	PriceCacheRead     money.Price `json:"price_cache_read"`
	PriceCacheCreation money.Price `json:"price_cache_creation"`

	AccountingMode   string      `json:"accounting_mode"`
	BillingMode      string      `json:"billing_mode"`
	PerImageMicroUSD money.Micro `json:"per_image_micro_usd"`

	Source      string `json:"source"`
	ModelsDevID string `json:"models_dev_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IsFallback 报告该规则是否为空库自动创建的全模型兜底规则。
func (r *PricingRule) IsFallback() bool {
	return r.MatchKind == MatchGlob && r.Pattern == "*" && r.Priority == fallbackRulePriority
}

// Free 报告该规则的所有计价档位是否均为 0。
func (r *PricingRule) Free() bool {
	return r.PriceInput == 0 && r.PriceOutput == 0 &&
		r.PriceCacheRead == 0 && r.PriceCacheCreation == 0 &&
		r.PerImageMicroUSD == 0
}

// Validate 校验规则字段。
func (r *PricingRule) Validate() error {
	switch r.MatchKind {
	case MatchExact, MatchGlob, MatchRegexp:
	default:
		return fmt.Errorf("match_kind 须为 exact/glob/regexp，得到 %q", r.MatchKind)
	}
	if strings.TrimSpace(r.Pattern) == "" {
		return fmt.Errorf("pattern 不能为空")
	}
	if r.MatchKind == MatchRegexp {
		if _, err := compileRegexp(r.Pattern); err != nil {
			return fmt.Errorf("regexp 规则 %q 无法编译: %w", r.Pattern, err)
		}
	}
	switch r.Source {
	case PricingSourceManual, PricingSourceModelsDev:
	default:
		return fmt.Errorf("source 须为 manual/models_dev，得到 %q", r.Source)
	}
	prices := map[string]money.Price{
		"price_input":          r.PriceInput,
		"price_output":         r.PriceOutput,
		"price_cache_read":     r.PriceCacheRead,
		"price_cache_creation": r.PriceCacheCreation,
	}
	for name, p := range prices {
		if p < 0 {
			return fmt.Errorf("%s 不能为负，得到 %d", name, p)
		}
	}
	if r.PerImageMicroUSD < 0 {
		return fmt.Errorf("per_image_micro_usd 不能为负，得到 %d", r.PerImageMicroUSD)
	}
	return nil
}

// Reservation 是一次额度预占。
type Reservation struct {
	ID              string      `json:"id"`
	KeyID           string      `json:"key_id"`
	CallerID        string      `json:"caller_id"`
	Model           string      `json:"model"`
	IdempotencyKey  string      `json:"idempotency_key,omitempty"`
	Status          string      `json:"status"`
	HeldMicroUSD    money.Micro `json:"held_micro_usd"`
	SettledMicroUSD money.Micro `json:"settled_micro_usd"`
	ReservedTokens  int64       `json:"reserved_tokens"`

	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	HeartbeatAt time.Time  `json:"heartbeat_at"`
	SettledAt   *time.Time `json:"settled_at,omitempty"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

// UsageBackfill 是宿主 usage.handle 上报的 token 明细，用于回填零用量记录。
type UsageBackfill struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
	TTFTMS              int64
}

// Request 是一条逐请求记录（tracker 明细与 credit-manager 账本合并）。
type Request struct {
	ID       string    `json:"id"`
	TS       time.Time `json:"ts"`
	KeyID    string    `json:"key_id"`
	CallerID string    `json:"caller_id"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	Source   string    `json:"source"`

	// UpstreamModel 是上游实际声明的模型名（二次路由后的真名），
	// 仅在与 Model 不同时填写；空串表示直连或未知。
	UpstreamModel string `json:"upstream_model,omitempty"`

	// 认证字段已做凭据清洗，不含任何上游 Key 明文。
	AuthID    string `json:"auth_id"`
	AuthLabel string `json:"auth_label"`
	AuthType  string `json:"auth_type"`

	Tier   string `json:"tier"`
	Result string `json:"result"`

	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`

	LatencyMS    int64 `json:"latency_ms"`
	TTFTMS       int64 `json:"ttft_ms"`
	GenerationMS int64 `json:"generation_ms"`
	// TPSMilli 是 TPS×1000 的整数表示，避免存储浮点。
	TPSMilli int64 `json:"tps_milli"`

	ThinkingIntensity string      `json:"thinking_intensity"`
	CostMicroUSD      money.Micro `json:"cost_micro_usd"`
	// Priced 标记该请求是否命中了非兜底计价规则（用于价格覆盖率统计）。
	Priced        bool   `json:"priced"`
	ReservationID string `json:"reservation_id,omitempty"`
}

// AuditEvent 是一条审计事件。
type AuditEvent struct {
	ID         int64     `json:"id"`
	TS         time.Time `json:"ts"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	// Detail 是结构化补充信息，序列化为 detail_json。绝不可含密钥材料。
	Detail map[string]any `json:"detail,omitempty"`
}

// 周期标识 -----------------------------------------------------------------

// cycleOffsetMinutes 是周期计算相对 UTC 的固定偏移（分钟），进程级配置：
// 启动/reconfigure 时经 SetCycleOffsetMinutes 写入，请求 goroutine 并发读，
// 用 atomic 避免 data race。0=纯 UTC。
var cycleOffsetMinutes atomic.Int64

// SetCycleOffsetMinutes 设置周期偏移。只影响新产生的周期标识：cycle_key
// 是按「切换时刻所在周期」写入的字符串，旧键随下一次跨期自然归零，无需迁移。
func SetCycleOffsetMinutes(minutes int64) { cycleOffsetMinutes.Store(minutes) }

// CycleOffsetMinutes 返回当前周期偏移。
func CycleOffsetMinutes() int64 { return cycleOffsetMinutes.Load() }

// cycleTime 把时刻换算到「本地周期坐标系」：先加偏移再按 UTC 取年月日。
// 偏移 480（UTC+8）时，本地 2026-08-29 00:00 起的流量记入 08-29 的日周期。
func cycleTime(t time.Time) time.Time {
	u := t.UTC()
	if m := cycleOffsetMinutes.Load(); m != 0 {
		u = u.Add(time.Duration(m) * time.Minute)
	}
	return u
}

// CycleKeys 是某一时刻对应的三个周期标识。
type CycleKeys struct {
	Daily   string
	Weekly  string
	Monthly string
}

// CyclesFor 计算给定时刻的日/周/月周期标识（按 quota.cycle_offset_minutes
// 偏移后的本地日历，周以周一为起点）。
func CyclesFor(t time.Time) CycleKeys {
	u := cycleTime(t)
	year, week := u.ISOWeek()
	return CycleKeys{
		Daily:   u.Format("2006-01-02"),
		Weekly:  fmt.Sprintf("%04d-W%02d", year, week),
		Monthly: u.Format("2006-01"),
	}
}

// CycleStart 返回给定时刻所在日/周/月周期的起点（真实时刻）：本地坐标系里
// 的边界再减回偏移。用于把在途预占按创建时刻归入当前周期。
func CycleStart(t time.Time) (daily, weekly, monthly time.Time) {
	u := cycleTime(t)
	var off time.Duration
	if m := cycleOffsetMinutes.Load(); m != 0 {
		off = time.Duration(m) * time.Minute
	}
	daily = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).Add(-off)
	// ISO 周以周一为起点；Go 的 Weekday() 周日为 0，需换算。
	offset := (int(u.Weekday()) + 6) % 7
	weekly = daily.AddDate(0, 0, -offset)
	monthly = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-off)
	return daily, weekly, monthly
}

// JSON 辅助 ---------------------------------------------------------------

// encodeModels 把可用模型清单序列化为存储形式。空清单存为空串。
func encodeModels(models []string) (string, error) {
	cleaned := make([]string, 0, len(models))
	for _, m := range models {
		if t := strings.TrimSpace(m); t != "" {
			cleaned = append(cleaned, t)
		}
	}
	if len(cleaned) == 0 {
		return "", nil
	}
	b, err := json.Marshal(cleaned)
	if err != nil {
		return "", fmt.Errorf("序列化可用模型清单失败: %w", err)
	}
	return string(b), nil
}

// decodeModels 解析可用模型清单。
func decodeModels(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("解析可用模型清单失败: %w", err)
	}
	return out, nil
}

// 时间/指针转换 ----------------------------------------------------------

// millisPtr 把可空时间转为可空毫秒。
func millisPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixMilli()
}

// timePtr 把可空毫秒转为可空时间。
func timePtr(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}
	t := time.UnixMilli(*ms).UTC()
	return &t
}

// microPtr 把可空金额转为可空整数。
func microPtr(m *money.Micro) any {
	if m == nil {
		return nil
	}
	return int64(*m)
}

// moneyPtr 把可空整数转为可空金额。
func moneyPtr(v *int64) *money.Micro {
	if v == nil {
		return nil
	}
	m := money.Micro(*v)
	return &m
}

// int64Ptr 复制可空整数（token 限额列直接是 int64，不经 money 类型）。
func int64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

// countPtr 把可空整数转为可空 SQL 参数。
func countPtr(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
