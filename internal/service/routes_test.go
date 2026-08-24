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
	seedRequest(t, st, "rt6", ts, "multi3", "", "ok", 300, 300)
	seedRequest(t, st, "rt7", ts.Add(-time.Minute), "multi3", "", "ok", 100, 100)
	seedRequest(t, st, "rt8", ts.Add(time.Minute), "multi3", "", "ok", 100, 100)
	// rt3 模拟二次路由：上游声明了渠道前缀真名；rt2 同别名但未捕获到真名。
	// rt4/rt5 模拟一个别名真的路由到两个上游；rt6/rt7 显式真名 + rt8 未捕获，
	// 未捕获请求应按已捕获量分摊进 p1/p2（请求数 1:1、token 3:1）。
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		for _, p := range []struct{ id, up string }{
			{"rt3", "openrouter/gpt-5"},
			{"rt4", "a/wild"},
			{"rt5", "b/wild"},
			{"rt6", "p/multi3"},
			{"rt7", "q/multi3"},
		} {
			if _, e := tx.ExecContext(ctx, `UPDATE requests SET upstream_model=? WHERE id=?`, p.up, p.id); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RouteReport(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// gpt-5 的未捕获行归并进唯一真名 openrouter/gpt-5；wild 两真名保持两行；
	// multi3 的未捕获行按比例分摊进 p/q 后别名行消失。
	if len(rows) != 6 {
		t.Fatalf("应聚合为 6 条映射（claude-4 + openrouter/gpt-5 + a/b wild + p/q multi3），得到 %d：%+v", len(rows), rows)
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
			t.Fatalf("无未捕获请求时多真名各行保持原样，%s 行异常: %+v", name, w)
		}
	}
	if find("multi3") != nil {
		t.Fatalf("有显式真名的别名不应再单独成行: %+v", rows)
	}
	p, q := find("p/multi3"), find("q/multi3")
	// token 按 300:100 分摊 100 条 → 精确 75/25；请求数权重 1:1 时归入方不定，只验总量守恒。
	if p == nil || q == nil || p.Requests+q.Requests != 3 || p.Requests < 1 || q.Requests < 1 || p.TotalTokens != 375 || q.TotalTokens != 125 {
		t.Fatalf("未捕获请求应按已捕获量加权分摊且总量守恒: p=%+v q=%+v", p, q)
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
