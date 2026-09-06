package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

// glm 型规则：输入 $3/M、缓存读 $0.6/M、输出 $12/M（价格单位为 USD/百万 token）。
func tieredPricingRule(t *testing.T, s *Service, pattern string) {
	t.Helper()
	if _, err := s.st.UpsertPricingRule(context.Background(), store.PricingRule{
		MatchKind: store.MatchExact, Pattern: pattern, Priority: 10, Enabled: true,
		PriceInput: 3_000_000, PriceOutput: 12_000_000, PriceCacheRead: 600_000,
		Source: store.PricingSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReserveTieredHeldCost 锁定分档预占：输入估算按输入侧最贵档、输出估算按
// 输出价，替代旧的「总额 × 四档最高价」。回归背景：agent 流量 22 万 token
// 上下文 + max_tokens=128000 时旧口径预占 7.05 USD，真实成本 0.13 USD。
func TestReserveTieredHeldCost(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	tieredPricingRule(t, s, "gpt-test")
	issued, err := s.IssueKey(ctx, IssueRequest{AllowedModels: []string{"gpt-*"}})
	if err != nil {
		t.Fatal(err)
	}

	// 分档：300k 输入 × $3/M + 128k 输出 × $12/M = 0.9 + 1.536 = 2.436 USD。
	res, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-test",
		EstimatedTokens: 428_000, EstimatedInput: 300_000, EstimatedOutput: 128_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.HeldMicroUSD != 2_436_000 {
		t.Fatalf("分档预占金额异常: got %d want 2436000", res.HeldMicroUSD)
	}

	// 未携带拆分的调用方保持旧行为：总额 × 四档最高价（输出 12）。
	legacy, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-test", EstimatedTokens: 428_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.HeldMicroUSD != 5_136_000 {
		t.Fatalf("旧口径预占金额异常: got %d want 5136000", legacy.HeldMicroUSD)
	}

	// 缓存写档高于输入价时取输入侧最高（3.75 > 3），仍不波及输出档。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "gpt-cc", Priority: 10, Enabled: true,
		PriceInput: 3_000_000, PriceOutput: 12_000_000, PriceCacheCreation: 3_750_000,
		Source: store.PricingSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	cc, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-cc",
		EstimatedTokens: 428_000, EstimatedInput: 300_000, EstimatedOutput: 128_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cc.HeldMicroUSD != 1_125_000+1_536_000 {
		t.Fatalf("缓存写档参与输入侧取最贵异常: got %d", cc.HeldMicroUSD)
	}
}

// TestSettleNoResponseZeroCost 锁定：上游未产生任何响应数据（HTTP 错误/空
// 响应/零流块）时零用量结算不再按预占估算入账——预占随结算退回，请求行
// 照常落库保留可观测性。响应数据存在但缺 usage 的场景仍按预占入账。
func TestSettleNoResponseZeroCost(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	tieredPricingRule(t, s, "gpt-test")
	quota := money.Micro(10_000_000) // $10
	issued, err := s.IssueKey(ctx, IssueRequest{QuotaMicroUSD: &quota, AllowedModels: []string{"gpt-*"}})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-test",
		EstimatedTokens: 428_000, EstimatedInput: 300_000, EstimatedOutput: 128_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	row := &store.Request{ID: "req-noresp", KeyID: issued.KID, CallerID: store.DefaultCallerID, Model: "gpt-test", Result: store.ResultError, StatusCode: 502}
	settled, err := s.Settle(ctx, res.ID, usageparse.Usage{}, row, true)
	if err != nil || settled.Status != store.ReservationSettled {
		t.Fatalf("无响应结算异常: %+v %v", settled, err)
	}
	got, err := st.GetRequest(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CostMicroUSD != 0 || got.TotalTokens != 0 || got.Currency != store.PricingCurrencyUSD {
		t.Fatalf("无响应请求行应零成本落库: %+v", got)
	}

	// 响应数据存在但上游未回 usage（noResponse=false）：仍按预占入账，token
	// 回填预占估算值（settle_reserved 防逃逸语义保留）。
	res2, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-test",
		EstimatedTokens: 428_000, EstimatedInput: 300_000, EstimatedOutput: 128_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	row2 := &store.Request{ID: "req-noresponse-data", KeyID: issued.KID, CallerID: store.DefaultCallerID, Model: "gpt-test", Result: store.ResultOK}
	if _, err := s.Settle(ctx, res2.ID, usageparse.Usage{}, row2, false); err != nil {
		t.Fatal(err)
	}
	got2, err := st.GetRequest(ctx, row2.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 请求行 token 恒记真实值（零用量即 0，与线上表现一致），预占估算只
	// 进额度累计器。
	if got2.CostMicroUSD != 2_436_000 || got2.TotalTokens != 0 {
		t.Fatalf("缺 usage 但有响应应按分档预占入账: cost=%d tokens=%d", got2.CostMicroUSD, got2.TotalTokens)
	}

	// 额度口径：第一笔零成本不占额度，第二笔按预占扣 $2.436。
	bal, err := s.Balance(ctx, issued.KID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if bal.Total == nil || *bal.Total != 10_000_000-2_436_000 {
		t.Fatalf("额度扣减异常: got %v want 7564000", bal.Total)
	}
	if _, err := s.Settle(ctx, "missing-id", usageparse.Usage{}, nil, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的预占应报 NotFound: %v", err)
	}
}

// seedCacheHistory 为模型插入带缓存 token 的成功行（Claude 口径：
// input 不含 cache_read/cache_creation），供缓存份额学习采样。
func seedCacheHistory(t *testing.T, st *store.Store, model string, rows [][3]int64) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Duration(len(rows)+1) * time.Minute)
	for i, c := range rows {
		r := store.Request{
			ID: model + "-cache-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: model, Result: store.ResultOK,
			InputTokens: c[0], CacheReadTokens: c[1], CacheCreationTokens: c[2],
			TotalTokens: c[0] + c[1] + c[2],
		}
		if err := st.RecordPassiveUsage(ctx, r, store.PassiveDedupeHint{Models: []string{model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReserveCacheTieredCost 锁金额拆档：模型近期流量带缓存构成时，
// 输入侧预占按学习份额拆成 新鲜×输入价 + 读×读价 + 写×写价 三档，
// 而非最贵档全额；无样本模型保持最贵档兜底。
func TestReserveCacheTieredCost(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	// 输入 $3 / 输出 $12 / 读 $0.6 / 写 $3.75（每 M，四档齐备）。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "gpt-cache", Priority: 10, Enabled: true,
		PriceInput: 3_000_000, PriceOutput: 12_000_000, PriceCacheRead: 600_000, PriceCacheCreation: 3_750_000,
		Source: store.PricingSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	issued, err := s.IssueKey(ctx, IssueRequest{AllowedModels: []string{"gpt-*"}})
	if err != nil {
		t.Fatal(err)
	}

	// 历史：每条 20000 上下文，其中读 16000、写 2000、新鲜 2000 →
	// 读份额 80%、写份额 10%（万分比 8000/1000）。
	seedCacheHistory(t, st, "gpt-cache", [][3]int64{
		{2000, 16000, 2000}, {2000, 16000, 2000}, {2000, 16000, 2000},
	})

	res, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-cache",
		EstimatedTokens: 200_000, EstimatedInput: 190_000, EstimatedOutput: 10_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 拆档：190K 输入 → 新鲜 19000×$3 + 读 152000×$0.6 + 写 19000×$3.75
	// = 57000 + 91200 + 71250 = 219450；输出 10K×$12 = 120000。合计 339450。
	// 对照：不拆档的最贵口径是 190K×$3.75 = 712500，拆档后金额贴近真实构成。
	if res.HeldMicroUSD != 339_450 {
		t.Fatalf("缓存拆档预占金额异常: got %d want 339450", res.HeldMicroUSD)
	}

	// 无样本模型回退最贵档兜底：输入侧 max(3, 0.6, 3.75)=3.75。
	tieredPricingRule(t, s, "gpt-nocache")
	fallback, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-nocache", // gpt-nocache 无历史
		EstimatedTokens: 200_000, EstimatedInput: 190_000, EstimatedOutput: 10_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// gpt-nocache 规则四档：输入 $3 / 输出 $12 / 读 $0.6 / 写 $0 → 最贵 $3。
	if fallback.HeldMicroUSD != 190_000*3_000_000/1_000_000+10_000*12_000_000/1_000_000 {
		t.Fatalf("无样本应回退输入侧最贵档: got %d", fallback.HeldMicroUSD)
	}

	// 样本不足（<3 条）同样回退最贵档。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "gpt-thin", Priority: 10, Enabled: true,
		PriceInput: 3_000_000, PriceOutput: 12_000_000, PriceCacheRead: 600_000, PriceCacheCreation: 3_750_000,
		Source: store.PricingSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	seedCacheHistory(t, st, "gpt-thin", [][3]int64{{2000, 16000, 2000}})
	thin, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "gpt-thin",
		EstimatedTokens: 200_000, EstimatedInput: 190_000, EstimatedOutput: 10_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// gpt-thin 规则含写价 3.75：最贵档 = 190K×3.75 + 10K×12 = 832500。
	if thin.HeldMicroUSD != 190_000*3_750_000/1_000_000+10_000*12_000_000/1_000_000 {
		t.Fatalf("样本不足应回退输入侧最贵档: got %d want 832500", thin.HeldMicroUSD)
	}
}

// TestReserveCNYConversion 锁预占侧的 CNY 折算：CNY 规则四档价以
// micro-CNY 存储，预占金额必须与结算同口径按当前汇率折算成 micro-USD
// （此前预占侧漏折算，CNY 价格被当 USD 数值直接扣，虚占约汇率倍数）。
func TestReserveCNYConversion(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "cny-model", Priority: 10, Enabled: true,
		// ¥3/M 输入、¥12/M 输出、¥0.6/M 缓存读（micro-CNY）。
		PriceInput: 3_000_000, PriceOutput: 12_000_000, PriceCacheRead: 600_000,
		Currency: store.PricingCurrencyCNY, Source: store.PricingSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	issued, err := s.IssueKey(ctx, IssueRequest{AllowedModels: []string{"cny-*"}})
	if err != nil {
		t.Fatal(err)
	}
	// 别名模式 cny-model 需允许该模型名——直接用模式名当模型名。
	res, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "cny-model",
		EstimatedTokens: 200_000, EstimatedInput: 190_000, EstimatedOutput: 10_000,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 无缓存样本 → 输入侧最贵档 ¥3/M × 190K + 输出 ¥12/M × 10K = ¥690 CNY，
	// 按当前实时汇率 ceil 折算（测试机可能连上真实汇率源或走兜底 7.20）。
	rate := s.ExchangeRate(ctx).USDToCNY
	if !rate.Valid() {
		t.Fatalf("汇率应有效: %v", rate)
	}
	native := money.Micro(190_000*3_000_000/1_000_000 + 10_000*12_000_000/1_000_000)
	want := (int64(native)*1_000_000 + int64(rate) - 1) / int64(rate)
	if int64(res.HeldMicroUSD) != want {
		t.Fatalf("CNY 预占应按当前汇率折算: got %d want %d（native=%d rate=%v）",
			res.HeldMicroUSD, want, native, rate)
	}
}
