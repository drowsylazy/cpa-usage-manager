package service

import (
	"context"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestListRequestsModelSuffixMatch 覆盖模型筛选的「精确名或渠道/后缀」匹配：
// 输入裸名 ox-alpha 应命中 openrouter/ox-alpha，完整名精确命中，无关名不命中。
func TestListRequestsModelSuffixMatch(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	i, err := s.IssueKey(ctx, IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 8, 20, 10, 3, 0, 0, time.UTC)
	for _, r := range []store.Request{
		{ID: "a", TS: ts, KeyID: i.KID, CallerID: store.DefaultCallerID, Model: "openrouter/ox-alpha", Result: store.ResultOK},
		{ID: "b", TS: ts, KeyID: i.KID, CallerID: store.DefaultCallerID, Model: "stealth/ox-alpha", Result: store.ResultOK},
		{ID: "c", TS: ts, KeyID: i.KID, CallerID: store.DefaultCallerID, Model: "stepfun/step-3.7-flash", Result: store.ResultOK},
	} {
		if err := st.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		model string
		want  int
	}{
		{"ox-alpha", 2},               // 裸名命中两个渠道前缀
		{"openrouter/ox-alpha", 1},    // 完整名精确命中
		{"stepfun/step-3.7-flash", 1}, // 含下划线语义字符的字面匹配
		{"step-3.7-flash", 1},         // 渠道后缀
		{"nope", 0},                   // 无关名
	}
	for _, c := range cases {
		page, err := s.ListRequests(ctx, UsageFilter{KeyID: i.KID, Model: c.model}, 10, 0, "ts", "desc")
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != int64(c.want) {
			t.Fatalf("model=%q 命中 %d 条, 期望 %d", c.model, page.Total, c.want)
		}
	}
}

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

// TestRouteReportBareNameMerge 验证上游路由聚合的渠道前缀归一：同一别名下
// 嗅探成功的裸名行与嗅探失败落库的「渠道/模型」行合并为一行，展示名取裸名。
func TestRouteReportBareNameMerge(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	ts := time.Date(2026, 8, 20, 10, 3, 0, 0, time.UTC)
	for _, r := range []store.Request{
		{ID: "a", TS: ts, Model: "swellrouter/free", UpstreamModel: "deepseek-v4-flash", Result: store.ResultOK, InputTokens: 100, TotalTokens: 100},
		{ID: "b", TS: ts.Add(time.Second), Model: "swellrouter/free", UpstreamModel: "deepseek-v4-flash", Result: store.ResultOK, InputTokens: 60, TotalTokens: 60},
		{ID: "c", TS: ts.Add(2 * time.Second), Model: "swellrouter/free", UpstreamModel: "orcarouter/deepseek-v4-flash", Result: store.ResultOK, InputTokens: 40, TotalTokens: 40},
	} {
		if err := st.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.RouteReport(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应合并为一行: %+v", rows)
	}
	if rows[0].UpstreamModel != "deepseek-v4-flash" {
		t.Fatalf("展示名应为裸名: %q", rows[0].UpstreamModel)
	}
	if rows[0].Requests != 3 || rows[0].TotalTokens != 200 {
		t.Fatalf("聚合错误: req=%d tok=%d", rows[0].Requests, rows[0].TotalTokens)
	}
}

// TestTrendsWeekBucketMonday 锁定周粒度分桶：SQLite 的 weekday 修饰符在日期
// 已是周一时不前进，旧口径（先 weekday 1 再 -7 days）会把周一的桶退到上一
// 周；修正后同 ISO 周的周一/周三/周日必须聚成同周一起点的一桶。
func TestTrendsWeekBucketMonday(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	i, err := s.IssueKey(ctx, IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2026, 8, 24, 10, 3, 0, 0, time.UTC)
	if monday.Weekday() != time.Monday {
		t.Fatalf("测试锚点应为周一: %v", monday.Weekday())
	}
	ids := []string{"w-mon", "w-wed", "w-sun"}
	for n, ts := range []time.Time{
		monday,
		monday.Add(48 * time.Hour),
		monday.Add(6 * 24 * time.Hour),
	} {
		r := store.Request{ID: ids[n], TS: ts, KeyID: i.KID, CallerID: store.DefaultCallerID, Model: "m", Result: store.ResultOK}
		if err := st.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	pts, err := s.Trends(ctx, UsageFilter{}, "week")
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("同 ISO 周三天应聚成 1 桶，实际 %d 桶: %+v", len(pts), pts)
	}
	want := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if !pts[0].Bucket.Equal(want) {
		t.Fatalf("周桶应为本周一零点 %v，实际 %v", want, pts[0].Bucket)
	}
}
