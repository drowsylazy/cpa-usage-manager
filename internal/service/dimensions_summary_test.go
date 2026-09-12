package service

import (
	"context"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestUsageSummaryByKeyBeyondPanelLimit 钉住汇总绕过面板 500 截断：
// Key 数超过 maxDimensionLimit 时曾只取前 500 组，超出的 Key 在汇总里
// 静默显示 0 用量。汇总走内部 groupByDimension，分组数受真实 Key 基数约束。
func TestUsageSummaryByKeyBeyondPanelLimit(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	const n = maxDimensionLimit + 10
	// 单 Key 免费计价下金额相同，按费用排序无法保证第 501 个 Key 进前 500，
	// 所以给每个 Key 都造用量并逐一断言。
	kids := make([]string, 0, n)
	ts := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		k, err := s.IssueKey(ctx, IssueRequest{})
		if err != nil {
			t.Fatalf("签发 Key #%d: %v", i, err)
		}
		kids = append(kids, k.KID)
		req := store.Request{
			ID: "req-" + k.KID, TS: ts, KeyID: k.KID, CallerID: store.DefaultCallerID,
			Model: "m", Result: store.ResultOK,
			InputTokens: 100, OutputTokens: 10, TotalTokens: 110,
			CostMicroUSD: money.Micro(i + 1), Priced: true,
		}
		if err := st.RecordUsage(ctx, req); err != nil {
			t.Fatalf("记录用量 #%d: %v", i, err)
		}
	}
	sums, err := s.UsageSummaryByKey(ctx, UsageFilter{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != n {
		t.Fatalf("汇总 Key 数 = %d, 期望 %d", len(sums), n)
	}
	byKID := make(map[string]KeySummary, len(sums))
	for _, ks := range sums {
		byKID[ks.KID] = ks
	}
	for i, kid := range kids {
		ks, ok := byKID[kid]
		if !ok {
			t.Fatalf("Key #%d (%s) 不在汇总里", i, kid)
		}
		if ks.Requests != 1 || ks.CostMicroUSD == 0 {
			t.Errorf("Key #%d (%s) 用量被截断: requests=%d cost=%v", i, kid, ks.Requests, ks.CostMicroUSD)
		}
	}
}
