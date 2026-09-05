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
