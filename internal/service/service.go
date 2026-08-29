// Package service 实现 P1 的密钥、额度、计价和审计业务语义。
package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/fx"
	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
	"github.com/google/uuid"
)

var (
	ErrInvalidKey          = errors.New("service: Key 格式或密钥不正确")
	ErrQuotaExceeded       = errors.New("service: 额度不足")
	ErrConcurrencyExceeded = errors.New("service: 并发额度不足")
	ErrModelNotAllowed     = errors.New("service: 模型不在 Key 允许清单")
	ErrUnknownPricing      = errors.New("service: 模型没有计价规则")
)

// maxPlausibleTPSMilli 是自算 TPS 的可信上限（3000 token/s，单位毫 TPS）。
// 当下最快的商用推理 API 峰值也远低于此；超过即为缓冲整转导致的坏测量。
const maxPlausibleTPSMilli = 3_000_000

type Pepper struct {
	ID    string
	Value []byte
}
type PepperSet struct {
	Active string
	Items  map[string]Pepper
}

// LoadPeppers 读取环境变量或 key-peppers 文件。格式支持 JSON 对象、id=base64 行，
// 以及单个原始值（使用 active_pepper_id）。缺失时自动生成并以 0600 写入文件。
func LoadPeppers(c config.Config, lookup func(string) (string, bool)) (PepperSet, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	ps := PepperSet{Active: c.Quota.Keys.ActivePepperID, Items: map[string]Pepper{}}
	raw, ok := lookup(c.Quota.Keys.PepperEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		b, err := os.ReadFile(c.PepperFilePath())
		if err == nil {
			raw = string(b)
			ok = true
		}
	}
	if ok {
		if err := parsePeppers(raw, ps.Active, &ps); err != nil {
			return PepperSet{}, err
		}
	}
	if len(ps.Items) == 0 {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return PepperSet{}, err
		}
		ps.Items[ps.Active] = Pepper{ID: ps.Active, Value: b}
		if err := c.EnsureDataDir(); err != nil {
			return PepperSet{}, err
		}
		data := base64.RawStdEncoding.EncodeToString(b) + "\n"
		if err := os.WriteFile(c.PepperFilePath(), []byte(data), config.PepperFilePerm); err != nil {
			return PepperSet{}, fmt.Errorf("写入 pepper 文件失败: %w", err)
		}
	}
	if _, ok := ps.Items[ps.Active]; !ok {
		return PepperSet{}, fmt.Errorf("active pepper %q 不存在", ps.Active)
	}
	return ps, nil
}
func parsePeppers(raw, active string, ps *PepperSet) error {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		for id, v := range m {
			b, e := decodePepper(v)
			if e != nil {
				return e
			}
			ps.Items[id] = Pepper{ID: id, Value: b}
		}
		return nil
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, val := active, line
		if i := strings.IndexByte(line, '='); i > 0 {
			id, val = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		}
		b, e := decodePepper(val)
		if e != nil {
			return e
		}
		ps.Items[id] = Pepper{ID: id, Value: b}
	}
	return nil
}
func decodePepper(v string) ([]byte, error) {
	if b, e := base64.StdEncoding.DecodeString(v); e == nil && len(b) >= 16 {
		return b, nil
	}
	if b, e := base64.RawStdEncoding.DecodeString(v); e == nil && len(b) >= 16 {
		return b, nil
	}
	if b, e := hex.DecodeString(v); e == nil && len(b) >= 16 {
		return b, nil
	}
	if len(v) >= 16 {
		return []byte(v), nil
	}
	return nil, errors.New("pepper 长度至少 16 字节")
}

type Service struct {
	st      *store.Store
	cfg     config.Config
	peppers PepperSet
	mu      sync.RWMutex
	// fxSvc 懒初始化，只在面板请求汇率时创建。
	fxSvc *fx.Service

	// models.dev 价格簿搜索缓存：整本目录较大，10 分钟内复用同一份。
	catalogMu  sync.Mutex
	catalogRaw map[string]ModelsDevProvider
	catalogAt  time.Time

	// pricingSnap 是启用计价规则的只读快照（已按匹配顺序排列）。
	// 计价规则是管理员低频写入的静态数据，Reserve/Settle 每请求都要匹配，
	// 快照命中时省去每次 1 次全表 SELECT。失效走 Store 的写后回调；
	// TTL 兜底覆盖跨进程改写（另一实例持租约修改）的极端场景。
	pricingSnap atomic.Pointer[pricingSnapshot]

	// lastSweepAt 记录上次内联清扫陈旧预占的时刻（Unix 秒）。
	// HoldReservation 的清扫在写锁事务内执行，节流到每 sweepInterval 一次，
	// 避免高并发下每个 Reserve 都附带一条 UPDATE。
	lastSweepAt atomic.Int64

	// 集中式预占心跳注册表：所有在途预占共用一个 goroutine 批量续期。
	// beatsStop 是该 goroutine 的退出通道，由 Close 关闭——reconfigure 换新
	// Service 时旧循环若不退出，会永久泄漏并反复触碰已关闭的库。
	beatsMu      sync.Mutex
	beats        map[string]struct{}
	beatsStarted bool
	beatsStop    chan struct{}

	// 鉴权成功的 Key 挂起表：由同一个心跳协程按周期批量刷 last_used_at，
	// 鉴权热路径上不再出现独立写事务（见 requestpath.go queueKeyTouch）。
	touchMu      sync.Mutex
	touchPending map[string]int64

	// 模型路由：快照（atomic + TTL，routes.go）、目标冷却状态器、ai_judge
	// 的设置缓存 / LRU 结果缓存 / single-flight 与宿主执行钩子。
	routeSnap atomic.Pointer[routeSnapshot]
	// routeReloadMu / pricingReloadMu 合并 TTL 过期瞬间的并发重载（惊群防护）。
	routeReloadMu    sync.Mutex
	pricingReloadMu  sync.Mutex
	coolMu           sync.Mutex
	cooldowns        map[string]time.Time

	judgeCfgMu  sync.Mutex
	judgeConf   judgeSettings
	judgeConfAt time.Time
	// notifyCfg* 是告警设置的 60s TTL 缓存：结算热路径每次都要比对单请求
	// 异常阈值，不能逐请求读 preferences；SaveNotifySettings 时失效。
	notifyCfgMu sync.Mutex
	notifyCfg   *NotifySettings
	notifyCfgAt time.Time
	judgeExec   atomic.Pointer[func(ctx context.Context, model string, body []byte) ([]byte, int, error)]
	judgeFlMu   sync.Mutex
	judgeFlights map[string]*judgeFlight
	judgeLRU     *judgeLRU
	// 评判子调用归属：宿主被动回调据此把无主行记到触发请求的 Key 名下。
	judgeTrk judgeTracker
}

// pricingSnapshot 是一份不可变的计价规则快照。
type pricingSnapshot struct {
	rules []store.PricingRule
	at    time.Time
}

const (
	// pricingCacheTTL 是计价快照的最长存活时间；事件失效为主，此值只兜底跨进程改写。
	pricingCacheTTL = time.Minute
	// sweepInterval 是两次内联清扫陈旧预占的最小间隔。预占默认过期阈值 2h，
	// 30s 的清扫粒度远低于它，不影响僵死预占的回收时效。
	sweepInterval = 30 * time.Second
)

// Config 返回当前生效的配置副本，供 httpapi 读取展示项与开关。
func (s *Service) Config() config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Store 暴露底层存储句柄，供 httpapi 直接调用只读/维护接口。
func (s *Service) Store() *store.Store { return s.st }

func New(st *store.Store, c config.Config, ps PepperSet) *Service {
	s := &Service{st: st, cfg: c, peppers: ps, beatsStop: make(chan struct{})}
	st.SetPricingChangeHandler(func() { s.pricingSnap.Store(nil) })
	// 路由写后即时失效快照；TTL 只兜底跨进程改写。
	st.SetRoutesChangedHandler(s.invalidateRoutes)
	s.judgeLRU = newJudgeLRU(judgeCacheMax)
	s.judgeFlights = make(map[string]*judgeFlight)
	return s
}

// Close 停止后台心跳协程（预占续期与 last_used_at 批量落库共用）。
// reconfigure 构建新 Service 前对旧实例调用；关闭后再启动的心跳循环会
// 立即退出，不会泄漏。
func (s *Service) Close() {
	s.beatsMu.Lock()
	defer s.beatsMu.Unlock()
	if s.beatsStop != nil {
		select {
		case <-s.beatsStop:
		default:
			close(s.beatsStop)
		}
		s.beatsStop = nil
	}
}
func (s *Service) pepper(id string) (Pepper, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.peppers.Items[id]
	return p, ok
}

type IssueRequest struct {
	Principal             string       `json:"principal"`
	CallerID              string       `json:"caller_id"`
	CallerScope           string       `json:"caller_scope"`
	Label                 string       `json:"label"`
	ExpiresAt             *time.Time   `json:"expires_at"`
	QuotaMicroUSD         *money.Micro `json:"quota_micro_usd"`
	DailyMicroUSD         *money.Micro `json:"daily_micro_usd"`
	WeeklyMicroUSD        *money.Micro `json:"weekly_micro_usd"`
	MonthlyMicroUSD       *money.Micro `json:"monthly_micro_usd"`
	TokenLimit            *int64       `json:"token_limit"`
	DailyTokenLimit       *int64       `json:"daily_token_limit"`
	WeeklyTokenLimit      *int64       `json:"weekly_token_limit"`
	MonthlyTokenLimit     *int64       `json:"monthly_token_limit"`
	DailyRequestsLimit    *int64       `json:"daily_requests_limit"`
	MonthlyRequestsLimit  *int64       `json:"monthly_requests_limit"`
	MaxConcurrentRequests int          `json:"max_concurrent_requests"`
	AllowedModels         []string     `json:"allowed_models"`
	Actor                 string       `json:"actor"`
}
type IssuedKey struct {
	Key         string
	KID         string
	Fingerprint string
	Record      store.PluginKey
}

// quotaModeConflict 钉住产品口径：金额限额与 Token 限额互斥，
// 一个 Key 只能选一种计费口径（「双闸任一触顶即拒」的旧语义已废弃）。
func quotaModeConflict(hasMoney, hasTok bool) error {
	if hasMoney && hasTok {
		return errors.New("service: 金额限额与 Token 限额二选一，不能同时配置")
	}
	return nil
}

func (s *Service) IssueKey(ctx context.Context, r IssueRequest) (IssuedKey, error) {
	if err := quotaModeConflict(
		r.QuotaMicroUSD != nil || r.DailyMicroUSD != nil || r.WeeklyMicroUSD != nil || r.MonthlyMicroUSD != nil,
		r.TokenLimit != nil || r.DailyTokenLimit != nil || r.WeeklyTokenLimit != nil || r.MonthlyTokenLimit != nil,
	); err != nil {
		return IssuedKey{}, err
	}
	if r.CallerID == "" {
		r.CallerID = store.DefaultCallerID
	}
	if r.CallerScope == "" {
		r.CallerScope = store.CallerScopeCaller
	}
	p := s.peppers.Items[s.peppers.Active]
	kid := randomID(8)
	secret := randomSecret(32)
	plain := "cum-" + kid + "-" + secret
	hash := hashKey(p.Value, plain)
	enc, err := encrypt(p.Value, []byte(plain))
	if err != nil {
		return IssuedKey{}, err
	}
	rec, err := s.st.InsertKey(ctx, store.InsertKeyParams{KID: kid, KeyHash: hash, EncryptedMaterial: enc, PepperID: p.ID, Fingerprint: fingerprint(plain), Principal: r.Principal, CallerScope: r.CallerScope, CallerID: r.CallerID, Label: r.Label, ExpiresAt: r.ExpiresAt, QuotaMicroUSD: r.QuotaMicroUSD, DailyMicroUSD: r.DailyMicroUSD, WeeklyMicroUSD: r.WeeklyMicroUSD, MonthlyMicroUSD: r.MonthlyMicroUSD, TokenLimit: r.TokenLimit, DailyTokenLimit: r.DailyTokenLimit, WeeklyTokenLimit: r.WeeklyTokenLimit, MonthlyTokenLimit: r.MonthlyTokenLimit, DailyRequestsLimit: r.DailyRequestsLimit, MonthlyRequestsLimit: r.MonthlyRequestsLimit, MaxConcurrentRequests: r.MaxConcurrentRequests, AllowedModels: r.AllowedModels})
	if err != nil {
		return IssuedKey{}, err
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{Actor: r.Actor, Action: "key.issue", EntityType: "key", EntityID: kid, Detail: map[string]any{"fingerprint": rec.Fingerprint}})
	return IssuedKey{Key: plain, KID: kid, Fingerprint: rec.Fingerprint, Record: rec}, nil
}

type AuthenticatedKey struct {
	Record    store.PluginKey
	Plaintext string
}

func (s *Service) Authenticate(ctx context.Context, plain string) (AuthenticatedKey, error) {
	kid, _, ok := parseKey(plain)
	if !ok {
		return AuthenticatedKey{}, ErrInvalidKey
	}
	k, err := s.st.GetKey(ctx, kid)
	if err != nil {
		return AuthenticatedKey{}, ErrInvalidKey
	}
	p, ok := s.pepper(k.PepperID)
	if !ok {
		return AuthenticatedKey{}, ErrInvalidKey
	}
	got := hashKey(p.Value, plain)
	if subtle.ConstantTimeCompare(got, k.KeyHash) != 1 || !k.Usable(time.Now().UTC()) {
		return AuthenticatedKey{}, ErrInvalidKey
	}
	s.queueKeyTouch(kid)
	return AuthenticatedKey{Record: k, Plaintext: plain}, nil
}
func parseKey(v string) (string, string, bool) {
	if !strings.HasPrefix(v, "cum-") {
		return "", "", false
	}
	x := strings.SplitN(strings.TrimPrefix(v, "cum-"), "-", 2)
	if len(x) != 2 || x[0] == "" || x[1] == "" {
		return "", "", false
	}
	return x[0], x[1], true
}

// ParseKeyID 返回 cum-<kid>-<secret> 形式明文中的 kid。非本插件 Key 返回 false。
func ParseKeyID(plain string) (string, bool) {
	kid, _, ok := parseKey(plain)
	return kid, ok
}

func (s *Service) RotateKey(ctx context.Context, kid, actor string) (IssuedKey, error) {
	k, err := s.st.GetKey(ctx, kid)
	if err != nil {
		return IssuedKey{}, err
	}
	p := s.peppers.Items[s.peppers.Active]
	secret := randomSecret(32)
	plain := "cum-" + kid + "-" + secret
	enc, err := encrypt(p.Value, []byte(plain))
	if err != nil {
		return IssuedKey{}, err
	}
	fp := fingerprint(plain)
	if err := s.st.RotateKeyMaterial(ctx, kid, hashKey(p.Value, plain), enc, p.ID, fp); err != nil {
		return IssuedKey{}, err
	}
	k, err = s.st.GetKey(ctx, kid)
	if err == nil {
		_ = s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "key.rotate", EntityType: "key", EntityID: kid, Detail: map[string]any{"fingerprint": fp}})
	}
	return IssuedKey{Key: plain, KID: kid, Fingerprint: fp, Record: k}, err
}
func (s *Service) RevealKey(ctx context.Context, kid, actor string) (string, error) {
	k, err := s.st.GetKey(ctx, kid)
	if err != nil {
		return "", err
	}
	p, ok := s.pepper(k.PepperID)
	if !ok {
		return "", ErrInvalidKey
	}
	plain, err := decrypt(p.Value, k.EncryptedMaterial)
	if err == nil {
		_ = s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "key.reveal", EntityType: "key", EntityID: kid, Detail: map[string]any{"fingerprint": k.Fingerprint}})
	}
	return string(plain), err
}

type ReservationRequest struct {
	KeyID, CallerID, Model, IdempotencyKey string
	EstimatedTokens                        int64
	EstimatedImages                        int64
	Actor                                  string
	ExpiresAt                              time.Time
	// PricingOverride 非空时跳过按 Model 的规则匹配，直接采用该计价规则
	// （别名路由 mode=target：执行器已按首选目标匹配）。
	PricingOverride *store.PricingRule
}

func (s *Service) Reserve(ctx context.Context, r ReservationRequest) (store.Reservation, error) {
	k, err := s.st.GetKey(ctx, r.KeyID)
	if err != nil {
		return store.Reservation{}, err
	}
	now := time.Now().UTC()
	if !k.Usable(now) {
		return store.Reservation{}, ErrInvalidKey
	}
	if !k.ModelAllowed(r.Model) {
		return store.Reservation{}, ErrModelNotAllowed
	}
	if r.EstimatedTokens < 0 || r.EstimatedTokens > s.cfg.Quota.Limits.MaxTokenEstimate {
		return store.Reservation{}, fmt.Errorf("%w: token estimate", ErrQuotaExceeded)
	}
	if r.EstimatedImages < 0 {
		return store.Reservation{}, fmt.Errorf("%w: image estimate", ErrQuotaExceeded)
	}
	var rule store.PricingRule
	priced := false
	if r.PricingOverride != nil {
		rule, priced = *r.PricingOverride, true
	} else {
		var err error
		rule, priced, err = s.matchPricing(ctx, r.Model)
		if err != nil {
			return store.Reservation{}, err
		}
	}
	if r.EstimatedTokens == 0 && r.EstimatedImages == 0 && s.cfg.Quota.Limits.RequireEstimate {
		return store.Reservation{}, fmt.Errorf("%w: 缺少用量估算", ErrQuotaExceeded)
	}
	if r.EstimatedTokens == 0 && rule.BillingMode == store.BillingModeToken {
		r.EstimatedTokens = s.cfg.Quota.Limits.DefaultOutputReserve
	}
	maxPrice := rule.PriceInput
	for _, p := range []money.Price{rule.PriceOutput, rule.PriceCacheRead, rule.PriceCacheCreation} {
		if p > maxPrice {
			maxPrice = p
		}
	}
	cost, err := money.CostForTokens(r.EstimatedTokens, maxPrice)
	if err != nil {
		return store.Reservation{}, err
	}
	if rule.BillingMode == store.BillingModeFree {
		cost = 0
	}
	if rule.BillingMode == store.BillingModePerImage {
		cost, err = multiplyMicro(rule.PerImageMicroUSD, r.EstimatedImages)
		if err != nil {
			return store.Reservation{}, err
		}
	}
	if !priced && s.cfg.Pricing.UnknownPolicy == config.UnknownPolicyDeny {
		return store.Reservation{}, ErrUnknownPricing
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = now.Add(s.cfg.Quota.Stream.StaleReservationTimeout.Std())
	}
	// 清扫节流：写锁内的 DELETE/UPDATE 只有到间隔才执行一次，
	// 高并发 Reserve 不再各自附带一条清扫语句。
	var sweepBefore time.Time
	last := s.lastSweepAt.Load()
	if now.Unix()-last >= int64(sweepInterval.Seconds()) && s.lastSweepAt.CompareAndSwap(last, now.Unix()) {
		sweepBefore = now.Add(-s.cfg.Quota.Stream.StaleReservationTimeout.Std())
	}
	// 审计并入预占同一写事务：每请求写事务 3→2；审计失败不回滚扣占。
	holdID := uuid.NewString()
	res, _, err := s.st.HoldReservation(ctx, store.HoldReservationParams{
		ID: holdID, KeyID: r.KeyID, CallerID: r.CallerID, Model: r.Model,
		IdempotencyKey: r.IdempotencyKey, HeldMicroUSD: cost, ReservedTokens: r.EstimatedTokens,
		ExpiresAt: r.ExpiresAt, Now: now, SweepStaleBefore: sweepBefore,
		Audit: &store.AuditEvent{Actor: r.Actor, Action: "quota.reserve", EntityType: "reservation", EntityID: holdID, Detail: map[string]any{"key_id": r.KeyID, "cost_micro_usd": cost}},
	})
	return res, err
}
func (s *Service) Settle(ctx context.Context, id string, u usageparse.Usage, req *store.Request) (store.Reservation, error) {
	r, err := s.st.GetReservation(ctx, id)
	if err != nil {
		return store.Reservation{}, err
	}
	// 别名路由 mode=target：按最终成功目标（剥思考后缀）计价；
	// mode=alias 或直连流量仍按预占模型名匹配。
	pricingName := r.Model
	if req != nil && strings.TrimSpace(req.UpstreamModel) != "" {
		if m, ok := s.MatchRoute(ctx, r.Model); ok && m.Route.PricingMode == "target" {
			if base, _ := StripThinkingSuffix(req.UpstreamModel); base != "" {
				pricingName = base
			}
		}
	}
	rule, priced, e := s.matchPricing(ctx, pricingName)
	if e != nil {
		return store.Reservation{}, e
	}
	if !priced && s.cfg.Pricing.UnknownPolicy == config.UnknownPolicyDeny && !u.IsZero() {
		return store.Reservation{}, ErrUnknownPricing
	}
	var cost money.Micro
	if u.IsZero() {
		if s.cfg.Quota.Settlement.MissingUsage == config.MissingUsageRelease {
			return s.st.ReleaseReservation(ctx, id, time.Now())
		}
		cost = r.HeldMicroUSD
	} else {
		cost, e = costForRule(rule, u, u.ImageCount)
		if e != nil {
			return store.Reservation{}, e
		}
	}
	if req != nil {
		req.InputTokens = u.InputTokens
		req.OutputTokens = u.OutputTokens
		req.ReasoningTokens = u.ReasoningTokens
		req.CachedTokens = u.CachedTokens
		req.CacheReadTokens = u.CacheReadTokens
		req.CacheCreationTokens = u.CacheCreationTokens
		req.TotalTokens = u.EffectiveTotal()
		req.CostMicroUSD = cost
		req.Priced = priced
		// 上游不回 TPS 时按 输出token/生成时长 自算（毫单位整数，避免浮点）。
		// 宿主把整段响应缓冲后一次转发时 generation 只有几毫秒，算出的 TPS
		// 物理上不可能（数千上万），会污染维度聚合均值；超过 3000 token/s
		// 视为不可信测量，不落库（面板显示 "-"）。
		if req.TPSMilli == 0 && req.OutputTokens > 0 && req.GenerationMS > 0 {
			if tps := req.OutputTokens * 1_000_000 / req.GenerationMS; tps <= maxPlausibleTPSMilli {
				req.TPSMilli = tps
			}
		}
	}
	// 结算 token 与费用同一口径：Billable() 已把 inclusive/exclusive 归一、
	// cached 并入缓存读不重复计、推理并入输出。u 为零值（上游未回用量）时
	// 退回预占的估算值，与 cost 走 HeldMicroUSD 的兜底逻辑保持对称。
	billableTokens := billableForRule(rule, u).Sum()
	if u.IsZero() {
		billableTokens = r.ReservedTokens
	}
	out, err := s.st.SettleReservation(ctx, id, cost, billableTokens, time.Now(), req,
		store.AuditEvent{Action: "quota.settle", EntityType: "reservation", EntityID: id, Detail: map[string]any{"cost_micro_usd": cost}})
	if err == nil {
		s.maybeNotifySingleUsage(r.KeyID, r.Model, cost, billableTokens)
	}
	// 双写防重已前移到 SettleReservation 事务内（入库时探测合并），
	// 不再有逐请求的事后对账；历史遗留对由 Maintain 的 DedupeRequests 兜底。
	return out, err
}
func (s *Service) Release(ctx context.Context, id string) (store.Reservation, error) {
	out, err := s.st.ReleaseReservation(ctx, id, time.Now())
	if err == nil {
		_ = s.st.AppendAudit(ctx, store.AuditEvent{Action: "quota.release", EntityType: "reservation", EntityID: id})
	}
	return out, err
}

// BackfillRequestUsage 用宿主 usage.handle 的权威用量回填最近的零用量记录。
// 执行器流式解析拿不到上游用量时（费用按预占估算入账），token 明细在此补齐；
// 分钟聚合不回填，属可接受的统计口径差异。
func (s *Service) BackfillRequestUsage(ctx context.Context, kid string, models []string, near time.Time, b store.UsageBackfill) (bool, error) {
	return s.st.BackfillRequestUsage(ctx, kid, models, near, b)
}

// FindDuplicateExecutor 按 时间+延迟+模型 关联执行器已入库的记录，用于判重。
func (s *Service) FindDuplicateExecutor(ctx context.Context, models []string, near time.Time, latencyMS int64) (string, bool, error) {
	return s.st.FindDuplicateExecutor(ctx, models, near, latencyMS)
}

// BackfillRequestUsageByID 按 ID 回填宿主上报的用量明细。
func (s *Service) BackfillRequestUsageByID(ctx context.Context, id string, b store.UsageBackfill) error {
	return s.st.BackfillRequestUsageByID(ctx, id, b)
}

// UpdateKey、RevokeKey、DeleteKey 是管理面调用的薄封装，并统一写审计。
func (s *Service) UpdateKey(ctx context.Context, kid string, u store.KeyUpdate, actor string) (store.PluginKey, error) {
	// 二选一按「合并后的最终状态」校验：部分更新只改 label 时若存量已双配，
	// 也会被拦下，强制用户在下次编辑时选边（面板表单总是全量提交两族限额）。
	cur, err := s.st.GetKey(ctx, kid)
	if err != nil {
		return store.PluginKey{}, err
	}
	effMicro := func(a *money.Micro, b **money.Micro) *money.Micro {
		if b != nil {
			return *b
		}
		return a
	}
	effInt64 := func(a *int64, b **int64) *int64 {
		if b != nil {
			return *b
		}
		return a
	}
	hasMoney := effMicro(cur.QuotaMicroUSD, u.QuotaMicroUSD) != nil ||
		effMicro(cur.DailyMicroUSD, u.DailyMicroUSD) != nil ||
		effMicro(cur.WeeklyMicroUSD, u.WeeklyMicroUSD) != nil ||
		effMicro(cur.MonthlyMicroUSD, u.MonthlyMicroUSD) != nil
	hasTok := effInt64(cur.TokenLimit, u.TokenLimit) != nil ||
		effInt64(cur.DailyTokenLimit, u.DailyTokenLimit) != nil ||
		effInt64(cur.WeeklyTokenLimit, u.WeeklyTokenLimit) != nil ||
		effInt64(cur.MonthlyTokenLimit, u.MonthlyTokenLimit) != nil
	if err := quotaModeConflict(hasMoney, hasTok); err != nil {
		return store.PluginKey{}, err
	}
	k, err := s.st.UpdateKey(ctx, kid, u)
	if err == nil {
		_ = s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "key.update", EntityType: "key", EntityID: kid})
	}
	return k, err
}

func (s *Service) RevokeKey(ctx context.Context, kid, actor string) error {
	err := s.st.RevokeKey(ctx, kid)
	if err == nil {
		_ = s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "key.revoke", EntityType: "key", EntityID: kid})
	}
	return err
}

func (s *Service) DeleteKey(ctx context.Context, kid, actor string) error {
	err := s.st.DeleteKey(ctx, kid)
	if err == nil {
		_ = s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "key.delete", EntityType: "key", EntityID: kid})
	}
	return err
}

// LookupPricing 按模型名匹配启用计价规则（执行器为别名路由构造
// PricingOverride 用）。priced=false 表示只命中兜底免费规则或无规则。
func (s *Service) LookupPricing(ctx context.Context, model string) (store.PricingRule, bool, error) {
	return s.matchPricing(ctx, model)
}

func (s *Service) matchPricing(ctx context.Context, model string) (store.PricingRule, bool, error) {
	rules, err := s.pricingRules(ctx)
	if err != nil {
		return store.PricingRule{}, false, err
	}
	for _, r := range rules {
		if r.Matches(model) {
			return r, !r.IsFallback(), nil
		}
	}
	return store.PricingRule{}, false, nil
}

// pricingRules 返回按匹配顺序排列的启用规则快照：命中内存快照时零 DB 往返，
// 过期（或被写回调失效）时重新加载。ListPricingRules 的 SQL 已按
// priority DESC, id ASC 排序，与匹配顺序一致，无需再排。
// 重载经 double-checked locking 合并：TTL 过期瞬间只放行一个构建者。
func (s *Service) pricingRules(ctx context.Context) ([]store.PricingRule, error) {
	if snap := s.pricingSnap.Load(); snap != nil && time.Since(snap.at) < pricingCacheTTL {
		return snap.rules, nil
	}
	s.pricingReloadMu.Lock()
	defer s.pricingReloadMu.Unlock()
	if snap := s.pricingSnap.Load(); snap != nil && time.Since(snap.at) < pricingCacheTTL {
		return snap.rules, nil
	}
	rules, err := s.st.ListPricingRules(ctx, true)
	if err != nil {
		return nil, err
	}
	s.pricingSnap.Store(&pricingSnapshot{rules: rules, at: time.Now()})
	return rules, nil
}
func (s *Service) Price(model string, u usageparse.Usage) (money.Micro, bool, error) {
	r, p, e := s.matchPricing(context.Background(), model)
	if e != nil {
		return 0, false, e
	}
	if !p && s.cfg.Pricing.UnknownPolicy == config.UnknownPolicyDeny {
		return 0, false, ErrUnknownPricing
	}
	c, e := costForRule(r, u, u.ImageCount)
	return c, p, e
}

func billableForRule(r store.PricingRule, u usageparse.Usage) usageparse.Billable {
	switch r.AccountingMode {
	case store.AccountingModeInputInclusive:
		u.InputIncludesCache = true
	case store.AccountingModeInputExclusive:
		u.InputIncludesCache = false
	}
	return u.Billable()
}

func costForRule(r store.PricingRule, u usageparse.Usage, images int64) (money.Micro, error) {
	switch r.BillingMode {
	case store.BillingModeFree:
		return 0, nil
	case store.BillingModePerImage:
		return multiplyMicro(r.PerImageMicroUSD, images)
	}
	b := billableForRule(r, u)
	parts := make([]money.Micro, 0, 4)
	for _, x := range []struct {
		t int64
		p money.Price
	}{{b.Input, r.PriceInput}, {b.Output, r.PriceOutput}, {b.CacheRead, r.PriceCacheRead}, {b.CacheCreation, r.PriceCacheCreation}} {
		v, err := money.CostForTokens(x.t, x.p)
		if err != nil {
			return 0, err
		}
		parts = append(parts, v)
	}
	return money.SumCeil(parts...)
}

func multiplyMicro(unit money.Micro, count int64) (money.Micro, error) {
	if unit < 0 || count < 0 {
		return 0, money.ErrNegative
	}
	if count == 0 || unit == 0 {
		return 0, nil
	}
	if int64(unit) > int64(^uint64(0)>>1)/count {
		return 0, money.ErrOverflow
	}
	return unit * money.Micro(count), nil
}

func hashKey(pepper []byte, plain string) []byte {
	h := hmac.New(sha256.New, pepper)
	_, _ = h.Write([]byte(plain))
	return h.Sum(nil)
}
func fingerprint(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])[:16]
}
func randomID(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func randomSecret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func aesKey(pepper []byte) []byte {
	h := sha256.Sum256(append([]byte("cpa-usage-manager/aes\x00"), pepper...))
	return h[:]
}
func encrypt(pepper, plain []byte) ([]byte, error) {
	b, e := aes.NewCipher(aesKey(pepper))
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, g.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return g.Seal(nonce, nonce, plain, nil), nil
}
func decrypt(pepper, ciphertext []byte) ([]byte, error) {
	b, e := aes.NewCipher(aesKey(pepper))
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	n := g.NonceSize()
	if len(ciphertext) < n {
		return nil, ErrInvalidKey
	}
	return g.Open(nil, ciphertext[:n], ciphertext[n:], nil)
}
