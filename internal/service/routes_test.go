package service

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestRouteReport(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	ts := time.Now().UTC().Add(-time.Hour)

	seedRequest(t, st, "rt1", ts, "claude-4", "", "ok", 1_000_000, 100)
	seedRequest(t, st, "rt2", ts, "gpt-5", "", "ok", 2_000_000, 200)
	seedRequest(t, st, "rt3", ts, "gpt-5", "", "fail", 500_000, 50)
	// rt3 模拟二次路由：上游声明了渠道前缀真名。
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE requests SET upstream_model='openrouter/gpt-5' WHERE id='rt3'`)
		return e
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RouteReport(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("应聚合为 3 条映射（直连+直连+二次路由），得到 %d：%+v", len(rows), rows)
	}
	find := func(up string) *RouteRow {
		for i := range rows {
			if rows[i].UpstreamModel == up {
				return &rows[i]
			}
		}
		return nil
	}
	routed := find("openrouter/gpt-5")
	if routed == nil || routed.Requests != 1 || len(routed.Models) != 1 || routed.Models[0] != "gpt-5" {
		t.Fatalf("二次路由行异常: %+v", routed)
	}
	direct := find("claude-4")
	if direct == nil || direct.Requests != 1 || direct.TotalTokens != 100 {
		t.Fatalf("直连行应回退为别名本身: %+v", direct)
	}
	// 时间过滤生效：把窗口收窄到未来，应无数据。
	future, err := s.RouteReport(ctx, UsageFilter{From: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(future) != 0 {
		t.Fatalf("未来窗口不应有数据: %+v", future)
	}
}
