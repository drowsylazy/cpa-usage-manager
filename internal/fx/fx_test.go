package fx

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want RateMicro
	}{
		{"7.2345", 7_234_500},
		{"7", 7_000_000},
		{"7.123456", 7_123_456},
		{`"7.25"`, 7_250_000},
		{"  6.9  ", 6_900_000},
	}
	for _, c := range cases {
		got, err := ParseRate(c.in)
		if err != nil {
			t.Errorf("ParseRate(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseRate(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

func TestParseRateErrors(t *testing.T) {
	for _, s := range []string{"", "abc", "0", "-7", "7.1234567", "null"} {
		if _, err := ParseRate(s); err == nil {
			t.Errorf("ParseRate(%q) 应报错", s)
		}
	}
}

func TestRateValid(t *testing.T) {
	if !RateMicro(7_200_000).Valid() {
		t.Error("7.2 应为有效汇率")
	}
	for _, r := range []RateMicro{0, -1, 500_000, 31_000_000} {
		if r.Valid() {
			t.Errorf("%s 应被判为无效", r)
		}
	}
}

func TestConvertUSD(t *testing.T) {
	rate := RateMicro(7_200_000) // 7.20
	cases := []struct {
		usd  money.Micro
		want money.Micro
	}{
		{0, 0},
		{1_000_000, 7_200_000},   // $1 → ¥7.20
		{500_000, 3_600_000},     // $0.5 → ¥3.60
		{1, 7},                   // 1 micro-USD → 7.2 micro-CNY，四舍五入 7
		{-1_000_000, -7_200_000}, // 负数对称
		{123_456, 888_883},       // 123456×7.2 = 888883.2 → 888883
	}
	for _, c := range cases {
		if got := ConvertUSD(c.usd, rate); got != c.want {
			t.Errorf("ConvertUSD(%d, %d) = %d, 期望 %d", c.usd, rate, got, c.want)
		}
	}
}

func TestConvertUSDRoundHalfUp(t *testing.T) {
	// 汇率 1.5 时，1 micro-USD → 1.5 micro-CNY，四舍五入应进位到 2。
	if got := ConvertUSD(1, RateMicro(1_500_000)); got != 2 {
		t.Errorf("四舍五入应进位，得到 %d", got)
	}
	// 汇率 1.4 时应舍去。
	if got := ConvertUSD(1, RateMicro(1_400_000)); got != 1 {
		t.Errorf("应舍去，得到 %d", got)
	}
}

func TestConvertUSDZeroRate(t *testing.T) {
	if got := ConvertUSD(1_000_000, 0); got != 0 {
		t.Errorf("汇率为 0 应返回 0，得到 %d", got)
	}
	if got := ConvertUSD(1_000_000, -1); got != 0 {
		t.Errorf("负汇率应返回 0，得到 %d", got)
	}
}

func TestConvertUSDSaturatesInsteadOfWrapping(t *testing.T) {
	// 极端输入必须饱和而非回绕成负数。
	if got := ConvertUSD(math.MaxInt64, maxPlausibleRate); got != math.MaxInt64 {
		t.Errorf("应饱和到 MaxInt64，得到 %d", got)
	}
	if got := ConvertUSD(math.MinInt64, maxPlausibleRate); got != math.MinInt64 {
		t.Errorf("应饱和到 MinInt64，得到 %d", got)
	}
}

// memCache 是测试用的内存 Cache 实现。
type memCache struct {
	mu    sync.Mutex
	rate  Rate
	has   bool
	saves int
	// loadErr/saveErr 用于模拟存储故障。
	loadErr error
	saveErr error
}

func (c *memCache) LoadRate(context.Context) (Rate, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loadErr != nil {
		return Rate{}, false, c.loadErr
	}
	return c.rate, c.has, nil
}

func (c *memCache) SaveRate(_ context.Context, r Rate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saveErr != nil {
		return c.saveErr
	}
	c.rate, c.has = r, true
	c.saves++
	return nil
}

// countingProvider 记录调用次数。
type countingProvider struct {
	mu    sync.Mutex
	calls int
	rate  RateMicro
	err   error
}

func (p *countingProvider) Name() string { return "counting" }

func (p *countingProvider) Fetch(context.Context) (Rate, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.err != nil {
		return Rate{}, p.err
	}
	return Rate{USDToCNY: p.rate, FetchedAt: time.Now().UTC()}, nil
}

func (p *countingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestServiceFetchesAndCaches(t *testing.T) {
	cache := &memCache{}
	prov := &countingProvider{rate: 7_150_000}
	svc := NewService(cache, time.Hour, prov)

	r := svc.Get(context.Background())
	if r.USDToCNY != 7_150_000 {
		t.Errorf("汇率 = %s", r.USDToCNY)
	}
	if r.Fallback {
		t.Error("成功获取时不应标记 fallback")
	}
	if r.Source != "counting" {
		t.Errorf("Source = %q, 期望 counting", r.Source)
	}
	if cache.saves != 1 {
		t.Errorf("应写入缓存一次，实际 %d", cache.saves)
	}
	// TTL 内重复调用不应再打上游。
	svc.Get(context.Background())
	svc.Get(context.Background())
	if prov.count() != 1 {
		t.Errorf("TTL 内应只拉取一次，实际 %d", prov.count())
	}
}

func TestServiceUsesPersistentCacheOnFirstCall(t *testing.T) {
	cached := Rate{USDToCNY: 7_050_000, Source: "prev", FetchedAt: time.Now().UTC()}
	cache := &memCache{rate: cached, has: true}
	prov := &countingProvider{rate: 7_990_000}
	svc := NewService(cache, time.Hour, prov)

	r := svc.Get(context.Background())
	if r.USDToCNY != 7_050_000 {
		t.Errorf("应命中持久缓存，得到 %s", r.USDToCNY)
	}
	if prov.count() != 0 {
		t.Errorf("缓存新鲜时不应打上游，实际 %d 次", prov.count())
	}
}

func TestServiceRefetchesWhenCacheStale(t *testing.T) {
	stale := Rate{USDToCNY: 7_050_000, Source: "prev", FetchedAt: time.Now().Add(-48 * time.Hour).UTC()}
	cache := &memCache{rate: stale, has: true}
	prov := &countingProvider{rate: 7_990_000}
	svc := NewService(cache, time.Hour, prov)

	r := svc.Get(context.Background())
	if r.USDToCNY != 7_990_000 {
		t.Errorf("缓存过期应重新拉取，得到 %s", r.USDToCNY)
	}
	if prov.count() != 1 {
		t.Errorf("应拉取一次，实际 %d", prov.count())
	}
}

func TestServiceFallsBackToStaleOnFetchFailure(t *testing.T) {
	stale := Rate{USDToCNY: 7_050_000, Source: "prev", FetchedAt: time.Now().Add(-48 * time.Hour).UTC()}
	cache := &memCache{rate: stale, has: true}
	prov := &countingProvider{err: errors.New("网络不可达")}
	svc := NewService(cache, time.Hour, prov)

	r := svc.Get(context.Background())
	if r.USDToCNY != 7_050_000 {
		t.Errorf("上游失败应回落过期缓存，得到 %s", r.USDToCNY)
	}
	if r.Fallback {
		t.Error("过期缓存不是内置兜底，不应标记 fallback")
	}
}

func TestServiceFallsBackToBuiltinWhenNothingAvailable(t *testing.T) {
	prov := &countingProvider{err: errors.New("离线")}
	svc := NewService(nil, time.Hour, prov)

	r := svc.Get(context.Background())
	if r.USDToCNY != FallbackRate {
		t.Errorf("应回落内置汇率，得到 %s", r.USDToCNY)
	}
	if !r.Fallback {
		t.Error("应标记 fallback")
	}
	if r.Source != "fallback" {
		t.Errorf("Source = %q", r.Source)
	}
}

func TestServiceGetNeverReturnsInvalidRate(t *testing.T) {
	// 无论上游怎样出错，Get 都必须给出可用汇率（面板不能因汇率失败而空白）。
	for _, prov := range []Provider{
		&countingProvider{err: errors.New("boom")},
		StaticProvider{Rate: 0},           // 不合理值
		StaticProvider{Rate: 999_000_000}, // 超出区间
	} {
		svc := NewService(nil, time.Hour, prov)
		r := svc.Get(context.Background())
		if !r.USDToCNY.Valid() {
			t.Errorf("provider %s 情形下返回了无效汇率 %s", prov.Name(), r.USDToCNY)
		}
	}
}

func TestServiceSkipsImplausibleProviderAndTriesNext(t *testing.T) {
	bad := StaticProvider{Rate: 999_000_000} // 超出理性区间
	good := &countingProvider{rate: 7_300_000}
	svc := NewService(nil, time.Hour, bad, good)

	r := svc.Get(context.Background())
	if r.USDToCNY != 7_300_000 {
		t.Errorf("应跳过不合理来源改用下一个，得到 %s", r.USDToCNY)
	}
}

func TestServiceCacheLoadErrorIsNotFatal(t *testing.T) {
	cache := &memCache{loadErr: errors.New("库坏了")}
	prov := &countingProvider{rate: 7_400_000}
	svc := NewService(cache, time.Hour, prov)

	r := svc.Get(context.Background())
	if r.USDToCNY != 7_400_000 {
		t.Errorf("缓存读失败应继续拉上游，得到 %s", r.USDToCNY)
	}
}

func TestServiceCacheSaveErrorIsNotFatal(t *testing.T) {
	cache := &memCache{saveErr: errors.New("磁盘满")}
	prov := &countingProvider{rate: 7_400_000}
	svc := NewService(cache, time.Hour, prov)

	r := svc.Get(context.Background())
	if r.USDToCNY != 7_400_000 {
		t.Errorf("缓存写失败不应影响返回值，得到 %s", r.USDToCNY)
	}
}

func TestServiceRefreshIgnoresTTL(t *testing.T) {
	cache := &memCache{}
	prov := &countingProvider{rate: 7_100_000}
	svc := NewService(cache, time.Hour, prov)

	svc.Get(context.Background())
	if prov.count() != 1 {
		t.Fatalf("首次应拉取一次，实际 %d", prov.count())
	}
	prov.rate = 7_800_000
	r, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh 报错: %v", err)
	}
	if r.USDToCNY != 7_800_000 {
		t.Errorf("Refresh 应取到新值，得到 %s", r.USDToCNY)
	}
	if prov.count() != 2 {
		t.Errorf("Refresh 应忽略 TTL 再拉一次，实际 %d", prov.count())
	}
}

func TestServiceRefreshReturnsErrorWhenAllProvidersFail(t *testing.T) {
	svc := NewService(nil, time.Hour, &countingProvider{err: errors.New("离线")})
	if _, err := svc.Refresh(context.Background()); err == nil {
		t.Fatal("全部来源失败时 Refresh 应报错（与 Get 的静默回落不同）")
	} else if !errors.Is(err, ErrNoRate) {
		t.Errorf("应包裹 ErrNoRate，得到 %v", err)
	}
}

func TestServiceConcurrentGet(t *testing.T) {
	// 并发调用不得触发数据竞争（配合 -race 运行）。
	svc := NewService(&memCache{}, time.Hour, &countingProvider{rate: 7_200_000})
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := svc.Get(context.Background()); !r.USDToCNY.Valid() {
				t.Errorf("并发下取到无效汇率 %s", r.USDToCNY)
			}
		}()
	}
	wg.Wait()
}

func TestHTTPProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"base":"USD","date":"2026-08-20","rates":{"CNY":7.1834}}`))
	}))
	defer srv.Close()

	p := &HTTPProvider{ProviderName: "test", URL: srv.URL, RatePath: []string{"rates", "CNY"}, Client: srv.Client()}
	r, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch 报错: %v", err)
	}
	if r.USDToCNY != 7_183_400 {
		t.Errorf("汇率 = %s, 期望 7.183400", r.USDToCNY)
	}
	if r.Source != "test" {
		t.Errorf("Source = %q", r.Source)
	}
	if r.FetchedAt.IsZero() {
		t.Error("FetchedAt 应被填充")
	}
}

func TestHTTPProviderPrecisionNotLostViaFloat(t *testing.T) {
	// 6 位小数必须完整保留（float64 中转会在此类值上产生尾差）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rates":{"CNY":7.123457}}`))
	}))
	defer srv.Close()

	p := &HTTPProvider{ProviderName: "test", URL: srv.URL, RatePath: []string{"rates", "CNY"}, Client: srv.Client()}
	r, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch 报错: %v", err)
	}
	if r.USDToCNY != 7_123_457 {
		t.Errorf("汇率 = %d, 期望 7123457", r.USDToCNY)
	}
}

func TestHTTPProviderErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"非 200", `{}`, http.StatusInternalServerError},
		{"缺字段", `{"rates":{"EUR":0.9}}`, http.StatusOK},
		{"路径类型错", `{"rates":123}`, http.StatusOK},
		{"非法 JSON", `not json`, http.StatusOK},
		{"汇率为 null", `{"rates":{"CNY":null}}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.code)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			p := &HTTPProvider{ProviderName: "test", URL: srv.URL, RatePath: []string{"rates", "CNY"}, Client: srv.Client()}
			if _, err := p.Fetch(context.Background()); err == nil {
				t.Error("期望报错")
			}
		})
	}
}

func TestHTTPProviderRespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"rates":{"CNY":7.2}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p := &HTTPProvider{ProviderName: "test", URL: srv.URL, RatePath: []string{"rates", "CNY"}, Client: srv.Client()}
	if _, err := p.Fetch(ctx); err == nil {
		t.Error("超时应报错")
	}
}

func TestDefaultProvidersShape(t *testing.T) {
	ps := DefaultProviders(nil)
	if len(ps) < 2 {
		t.Fatalf("应有多个兜底来源，实际 %d", len(ps))
	}
	seen := map[string]bool{}
	for _, p := range ps {
		if p.Name() == "" {
			t.Error("provider 名称不能为空")
		}
		if seen[p.Name()] {
			t.Errorf("provider 名称重复: %s", p.Name())
		}
		seen[p.Name()] = true
	}
}

func TestNewServiceDefaults(t *testing.T) {
	// ttl <= 0 时应回落默认 TTL，而不是每次都打上游。
	prov := &countingProvider{rate: 7_200_000}
	svc := NewService(nil, 0, prov)
	svc.Get(context.Background())
	svc.Get(context.Background())
	if prov.count() != 1 {
		t.Errorf("ttl<=0 应回落 DefaultTTL，实际拉取 %d 次", prov.count())
	}
}

func TestRateFloatForDisplay(t *testing.T) {
	r := Rate{USDToCNY: 7_250_000}
	if got := r.Float(); got != 7.25 {
		t.Errorf("Float() = %v, 期望 7.25", got)
	}
}
