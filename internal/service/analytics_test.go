package service

import (
	"context"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func TestAnalyticsSummaryRequestsTrendsBalance(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	p := money.Micro(5)
	i, err := s.IssueKey(ctx, IssueRequest{QuotaMicroUSD: &p})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 8, 20, 10, 3, 0, 0, time.UTC)
	for _, r := range []store.Request{
		{ID: "a", TS: ts, KeyID: i.KID, CallerID: store.DefaultCallerID, Model: "m", Provider: "p", Result: store.ResultOK, InputTokens: 2, TotalTokens: 2, CostMicroUSD: 1, Priced: true},
		{ID: "b", TS: ts.Add(time.Minute), KeyID: i.KID, CallerID: store.DefaultCallerID, Model: "m", Provider: "p", Result: store.ResultError, OutputTokens: 3, TotalTokens: 3, CostMicroUSD: 2},
	} {
		if err := st.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := s.UsageSummary(ctx, UsageFilter{KeyID: i.KID})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 2 || sum.Failures != 1 || sum.TotalTokens != 5 || sum.CostMicroUSD != 3 {
		t.Fatalf("summary 异常: %+v", sum)
	}
	page, err := s.ListRequests(ctx, UsageFilter{KeyID: i.KID}, 1, 0, "cost", "desc")
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].ID != "b" {
		t.Fatalf("分页异常: %+v %v", page, err)
	}
	trends, err := s.Trends(ctx, UsageFilter{KeyID: i.KID, From: ts.Add(-time.Minute), To: ts.Add(2 * time.Minute)}, "minute")
	if err != nil || len(trends) != 2 {
		t.Fatalf("趋势异常: %+v %v", trends, err)
	}
	cov, err := s.Costs(ctx, UsageFilter{KeyID: i.KID})
	if err != nil || cov.PricedRequests != 1 {
		t.Fatalf("覆盖率异常: %+v %v", cov, err)
	}
	b, err := s.Balance(ctx, i.KID, ts)
	if err != nil || b.Total == nil || *b.Total != 5 {
		t.Fatalf("余额异常: %+v %v", b, err)
	}
}
