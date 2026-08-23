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
	seedRequest(t, st, "rt4", ts.Add(-time.Minute), "wild", "", "ok", 10, 10)
	seedRequest(t, st, "rt5", ts.Add(time.Minute), "wild", "", "ok", 20, 20)
	// rt3 模拟二次路由：上游声明了渠道前缀真名；rt2 同别名但未捕获到真名。
	// rt4/rt5 模拟一个别名真的路由到两个上游（显式真名不唯一）。
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `UPDATE requests SET upstream_model='openrouter/gpt-5' WHERE id='rt3'`); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `UPDATE requests SET upstream_model='a/wild' WHERE id='rt4'`); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `UPDATE requests SET upstream_model='b/wild' WHERE id='rt5'`)
		return e
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RouteReport(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// gpt-5 的未捕获行归并进唯一真名 openrouter/gpt-5；wild 两真名不唯一保持两行。
	if len(rows) != 4 {
		t.Fatalf("应聚合为 4 条映射（claude-4 + 归并后的 openrouter/gpt-5 + a/b wild），得到 %d：%+v", len(rows), rows)
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
	if routed == nil || routed.Requests != 2 || routed.TotalTokens != 250 || len(routed.Models) != 1 || routed.Models[0] != "gpt-5" {
		t.Fatalf("未捕获真名的请求应并入唯一真名行: %+v", routed)
	}
	direct := find("claude-4")
	if direct == nil || direct.Requests != 1 || direct.TotalTokens != 100 {
		t.Fatalf("无显式真名的直连行应保留为别名本身: %+v", direct)
	}
	for _, name := range []string{"a/wild", "b/wild"} {
		w := find(name)
		if w == nil || w.Requests != 1 {
			t.Fatalf("多真名别名不应被归并，%s 行异常: %+v", name, w)
		}
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
