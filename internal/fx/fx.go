// Package fx 提供 USD/CNY 汇率获取与缓存。
//
// 汇率以 micro 为单位的整数表示（7.2345 → 7234500），全程不使用浮点，
// 与 money 包的 micro-USD 口径保持一致。汇率仅用于面板展示，不参与额度结算。
package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

// RateMicro 是以 micro 为单位的汇率（1 USD 折合多少 CNY，乘以 1e6）。
type RateMicro int64

const (
	// MicroScale 是汇率的放大倍数。
	MicroScale = 1_000_000

	// FallbackRate 是无法获取且无缓存时使用的保守兜底汇率（7.20）。
	FallbackRate RateMicro = 7_200_000

	// DefaultTTL 是汇率缓存的默认有效期。
	DefaultTTL = 6 * time.Hour

	// DefaultTimeout 是单次汇率请求的超时。
	DefaultTimeout = 8 * time.Second

	// minPlausibleRate/maxPlausibleRate 是理性区间，用于拒绝明显异常的上游数据。
	minPlausibleRate RateMicro = 1_000_000  // 1.0
	maxPlausibleRate RateMicro = 30_000_000 // 30.0
)

// ErrNoRate 表示既取不到上游汇率也没有可用缓存。
var ErrNoRate = errors.New("fx: 无可用汇率")

// Rate 是一次汇率观测。
type Rate struct {
	USDToCNY  RateMicro `json:"usd_to_cny_micro"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	// Fallback 标记该汇率来自内置兜底值而非上游或缓存。
	Fallback bool `json:"fallback"`
}

// Float 以 float64 返回汇率，仅供 JSON 展示，不参与任何计算。
func (r Rate) Float() float64 { return float64(r.USDToCNY) / MicroScale }

// String 渲染为定长 6 位小数。
func (r RateMicro) String() string { return money.Micro(r).USDString() }

// Valid 报告汇率是否落在理性区间内。
func (r RateMicro) Valid() bool { return r >= minPlausibleRate && r <= maxPlausibleRate }

// ConvertUSD 把 micro-USD 金额换算为 micro-CNY，四舍五入到整数 micro。
// 仅用于展示，不用于计费。
func ConvertUSD(usd money.Micro, rate RateMicro) money.Micro {
	if rate <= 0 || usd == 0 {
		return 0
	}
	neg := usd < 0
	var abs uint64
	if neg {
		abs = uint64(-(usd + 1)) + 1
	} else {
		abs = uint64(usd)
	}
	hi, lo := bits.Mul64(abs, uint64(rate))
	if hi >= MicroScale {
		// 超出 int64 可表示范围，饱和到边界（真实金额不会走到这里）。
		if neg {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	q, r := bits.Div64(hi, lo, MicroScale)
	if r >= (MicroScale+1)/2 && q != math.MaxUint64 {
		q++
	}
	if q > uint64(math.MaxInt64) {
		if neg {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	if neg {
		return -money.Micro(q)
	}
	return money.Micro(q)
}

// Cache 是汇率的持久化载体（由 store 实现，落在 meta 表）。
type Cache interface {
	LoadRate(ctx context.Context) (Rate, bool, error)
	SaveRate(ctx context.Context, r Rate) error
}

// Provider 从上游获取汇率。
type Provider interface {
	Fetch(ctx context.Context) (Rate, error)
	Name() string
}

// Service 按 TTL 缓存汇率，上游失败时回落到过期缓存，最后回落到内置值。
type Service struct {
	providers []Provider
	cache     Cache
	ttl       time.Duration

	mu      sync.Mutex
	current Rate
	loaded  bool
}

// NewService 构造汇率服务。providers 按顺序尝试；cache 可为 nil。
func NewService(cache Cache, ttl time.Duration, providers ...Provider) *Service {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if len(providers) == 0 {
		providers = DefaultProviders(nil)
	}
	return &Service{providers: providers, cache: cache, ttl: ttl}
}

// Get 返回当前汇率：内存新鲜则直接返回，否则读缓存，再否则拉取上游。
// 任何一步失败都不会返回错误，而是退化到更旧的可用值（最终为内置兜底），
// 因为汇率只影响展示，不应阻断面板加载。
func (s *Service) Get(ctx context.Context) Rate {
	s.mu.Lock()
	if s.loaded && s.fresh(s.current) {
		r := s.current
		s.mu.Unlock()
		return r
	}
	// 首次调用时尝试加载持久缓存。
	if !s.loaded && s.cache != nil {
		if r, ok, err := s.cache.LoadRate(ctx); err == nil && ok && r.USDToCNY.Valid() {
			s.current, s.loaded = r, true
			if s.fresh(r) {
				s.mu.Unlock()
				return r
			}
		}
	}
	stale := s.current
	hadStale := s.loaded
	s.mu.Unlock()

	fresh, err := s.fetch(ctx)
	if err == nil {
		s.mu.Lock()
		s.current, s.loaded = fresh, true
		s.mu.Unlock()
		if s.cache != nil {
			// 缓存写失败不影响返回值。
			_ = s.cache.SaveRate(ctx, fresh)
		}
		return fresh
	}
	if hadStale && stale.USDToCNY.Valid() {
		return stale
	}
	return Rate{USDToCNY: FallbackRate, Source: "fallback", FetchedAt: time.Now().UTC(), Fallback: true}
}

// Refresh 强制忽略 TTL 拉取一次上游。
func (s *Service) Refresh(ctx context.Context) (Rate, error) {
	r, err := s.fetch(ctx)
	if err != nil {
		return Rate{}, err
	}
	s.mu.Lock()
	s.current, s.loaded = r, true
	s.mu.Unlock()
	if s.cache != nil {
		_ = s.cache.SaveRate(ctx, r)
	}
	return r, nil
}

func (s *Service) fresh(r Rate) bool {
	return r.USDToCNY.Valid() && !r.Fallback && time.Since(r.FetchedAt) < s.ttl
}

// fetch 依次尝试各 provider，返回首个理性结果。
func (s *Service) fetch(ctx context.Context) (Rate, error) {
	var errs []error
	for _, p := range s.providers {
		r, err := p.Fetch(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		if !r.USDToCNY.Valid() {
			errs = append(errs, fmt.Errorf("%s: 汇率 %s 超出理性区间", p.Name(), r.USDToCNY))
			continue
		}
		if r.FetchedAt.IsZero() {
			r.FetchedAt = time.Now().UTC()
		}
		if r.Source == "" {
			r.Source = p.Name()
		}
		return r, nil
	}
	if len(errs) == 0 {
		return Rate{}, ErrNoRate
	}
	return Rate{}, errors.Join(append([]error{ErrNoRate}, errs...)...)
}

// HTTPProvider 从一个返回 JSON 汇率表的 HTTP 端点获取汇率。
type HTTPProvider struct {
	// ProviderName 用于日志与 Rate.Source。
	ProviderName string
	// URL 是请求地址。
	URL string
	// RatePath 是汇率数值在 JSON 中的路径，例如 []string{"rates", "CNY"}。
	RatePath []string
	Client   *http.Client
}

// Name 实现 Provider。
func (p *HTTPProvider) Name() string { return p.ProviderName }

// Fetch 实现 Provider。
func (p *HTTPProvider) Fetch(ctx context.Context) (Rate, error) {
	cl := p.Client
	if cl == nil {
		cl = &http.Client{Timeout: DefaultTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return Rate{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return Rate{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Rate{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 限制读取量，避免异常端点拖垮插件。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Rate{}, err
	}
	raw, err := extractPath(body, p.RatePath)
	if err != nil {
		return Rate{}, err
	}
	v, err := ParseRate(raw)
	if err != nil {
		return Rate{}, err
	}
	return Rate{USDToCNY: v, Source: p.ProviderName, FetchedAt: time.Now().UTC()}, nil
}

// DefaultProviders 返回内置的汇率来源（按顺序尝试，均为无需密钥的公开端点）。
func DefaultProviders(client *http.Client) []Provider {
	return []Provider{
		&HTTPProvider{
			ProviderName: "frankfurter",
			URL:          "https://api.frankfurter.app/latest?from=USD&to=CNY",
			RatePath:     []string{"rates", "CNY"},
			Client:       client,
		},
		&HTTPProvider{
			ProviderName: "open.er-api",
			URL:          "https://open.er-api.com/v6/latest/USD",
			RatePath:     []string{"rates", "CNY"},
			Client:       client,
		},
	}
}

// ParseRate 把十进制字符串解析为 micro 汇率，不经过浮点。
func ParseRate(s string) (RateMicro, error) {
	t := strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"`))
	if t == "" {
		return 0, fmt.Errorf("汇率为空")
	}
	m, err := money.ParseUSD(t)
	if err != nil {
		return 0, fmt.Errorf("无法解析汇率 %q: %w", s, err)
	}
	if m <= 0 {
		return 0, fmt.Errorf("汇率必须为正，得到 %q", s)
	}
	return RateMicro(m), nil
}

// extractPath 沿 path 逐层取出 JSON 值，返回其原始标量文本。
func extractPath(body []byte, path []string) (string, error) {
	cur := json.RawMessage(body)
	for _, key := range path {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err != nil {
			return "", fmt.Errorf("JSON 路径 %s 处不是对象: %w", key, err)
		}
		next, ok := obj[key]
		if !ok {
			return "", fmt.Errorf("JSON 缺少字段 %s", key)
		}
		cur = next
	}
	// 数值以原始文本返回，避免 float64 中间转换丢精度。
	return strings.TrimSpace(string(cur)), nil
}

// StaticProvider 是返回固定汇率的 provider，便于测试与离线部署。
type StaticProvider struct {
	Rate RateMicro
	Err  error
}

// Name 实现 Provider。
func (p StaticProvider) Name() string { return "static" }

// Fetch 实现 Provider。
func (p StaticProvider) Fetch(context.Context) (Rate, error) {
	if p.Err != nil {
		return Rate{}, p.Err
	}
	return Rate{USDToCNY: p.Rate, Source: "static", FetchedAt: time.Now().UTC()}, nil
}

// mul64/divRoundHalfUp 已由 math/bits 提供，无需自行实现。
