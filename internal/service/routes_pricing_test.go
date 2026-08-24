package service

import (
	"context"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

// TestSettlePricingModeTarget 覆盖 mode=target：结算按 UpstreamModel 匹配规则。
func TestSettlePricingModeTarget(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	// 目标模型高价，别名无规则。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "opus-real", Priority: 10, Enabled: true, PriceInput: 5000, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "auto", Rule: "-> \"opus-real\"", CooldownSeconds: 60, PricingMode: "target", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	issued, err := s.IssueKey(ctx, IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"auto","messages":[]}`)
	meta := ParseRequestMeta(body)
	in, _ := meta.tokenEstimates(s.cfg.Quota.Limits.DefaultOutputReserve, s.cfg.Quota.Limits.MaxTokenEstimate)
	rule := mustRule(t, s, "opus-real")
	reservation, err := s.Reserve(ctx, ReservationRequest{
		KeyID: issued.KID, Model: "auto",
		EstimatedTokens: in + 100,
		PricingOverride: &rule,
	})
	if err != nil {
		t.Fatal(err)
	}
	u := usageparse.Usage{InputTokens: 1000, OutputTokens: 0, TotalTokens: 1000}
	u.InputTokens = 1000
	reqRow := &store.Request{ID: "req-1", UpstreamModel: "opus-real"}
	if _, err := s.Settle(ctx, reservation.ID, u, reqRow); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListRequests(ctx, UsageFilter{}, 10, 0, "ts", "desc")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("行数异常: %d %v", len(page.Items), err)
	}
	row := page.Items[0]
	if !row.Priced || row.CostMicroUSD != 5 { // 1000 token × 5000/百万 = 5 micro
		t.Fatalf("mode=target 应按目标计价: cost=%d priced=%v", row.CostMicroUSD, row.Priced)
	}
}

// TestSettlePricingModeAlias 覆盖 mode=alias：结算按别名自身规则计价，
// UpstreamModel 不参与匹配。
func TestSettlePricingModeAlias(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "auto", Priority: 10, Enabled: true, PriceInput: 1000, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	// 目标也有规则（若误用会得出不同金额）。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "opus-real", Priority: 10, Enabled: true, PriceInput: 9000, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "auto", Rule: "-> \"opus-real\"", CooldownSeconds: 60, PricingMode: "alias", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	issued, err := s.IssueKey(ctx, IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := s.Reserve(ctx, ReservationRequest{KeyID: issued.KID, Model: "auto", EstimatedTokens: 1100})
	if err != nil {
		t.Fatal(err)
	}
	u := usageparse.Usage{}
	u.InputTokens = 1000
	reqRow := &store.Request{ID: "req-2", UpstreamModel: "opus-real"}
	if _, err := s.Settle(ctx, reservation.ID, u, reqRow); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListRequests(ctx, UsageFilter{}, 10, 0, "ts", "desc")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("行数异常: %d %v", len(page.Items), err)
	}
	if row := page.Items[0]; row.CostMicroUSD != 1 { // 1000 token × 1000/百万 = 1 micro
		t.Fatalf("mode=alias 应按别名计价: cost=%d", row.CostMicroUSD)
	}
}

func mustRule(t *testing.T, s *Service, model string) (r store.PricingRule) {
	t.Helper()
	rule, priced, err := s.LookupPricing(context.Background(), model)
	if err != nil || !priced {
		t.Fatalf("%s 规则缺失: %+v %v priced=%v", model, rule, err, priced)
	}
	return rule
}
