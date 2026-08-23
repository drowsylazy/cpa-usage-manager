package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func seedRequest(t *testing.T, st *store.Store, id string, ts time.Time, model, keyID, result string, costMicro, tokens int64) {
	t.Helper()
	err := st.Write(context.Background(), func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(context.Background(),
			`INSERT INTO requests (id, ts, key_id, caller_id, model, result, input_tokens, output_tokens, total_tokens, cost_micro_usd)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, ts.UTC().UnixMilli(), keyID, "default", model, result,
			tokens*3/4, tokens/4, tokens, costMicro); e != nil {
			return e
		}
		// 维度聚合读 usage_rollups（生产上由结算路径同步维护），测试同步种入。
		_, e := tx.ExecContext(context.Background(),
			`INSERT INTO usage_rollups (bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result, req_count, input_tokens, output_tokens, total_tokens, cost_micro_usd)
			 VALUES (?,?,?,?,?,?,?,?,?,?,1,?,?,?)`,
			ts.UTC().Unix()/60, model, keyID, "default", "", "", "", "", result,
			tokens*3/4, tokens/4, tokens, costMicro)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReportPeriodDaily(t *testing.T) {
	local := time.Date(2026, 2, 5, 14, 30, 0, 0, time.UTC)
	key, from, to := reportPeriod("daily", local)
	if key != "2026-02-04" {
		t.Fatalf("key=%s", key)
	}
	if !from.Equal(time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC)) ||
		!to.Equal(time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("range=%v..%v", from, to)
	}
}

func TestReportPeriodWeeklyMonthly(t *testing.T) {
	// 2026-02-11 是周三；上一自然周为 02-02（周一）至 02-09（周一）零点。
	wed := time.Date(2026, 2, 11, 8, 0, 0, 0, time.UTC)
	key, from, to := reportPeriod("weekly", wed)
	if key != "2026-W06" {
		t.Fatalf("weekly key=%s", key)
	}
	if !from.Equal(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)) ||
		!to.Equal(time.Date(2026, 2, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly range=%v..%v", from, to)
	}
	feb15 := time.Date(2026, 2, 15, 23, 0, 0, 0, time.UTC)
	mk, mf, mt := reportPeriod("monthly", feb15)
	if mk != "2026-01" || !mf.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		!mt.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly=%s %v..%v", mk, mf, mt)
	}
}

func TestReportDueScheduleAndTimezone(t *testing.T) {
	// 固定参考时刻：UTC 周四 2026-02-05 20:00。
	now := time.Date(2026, 2, 5, 20, 0, 0, 0, time.UTC)
	cfg := store.ReportConfig{Frequency: "daily", TimeOfDay: "09:00", Weekday: 1, Monthday: 1}

	// 北京 (+480)：本地已是周五 04:00，早于触发点。
	cfg.TZOffsetMin = 480
	if _, _, _, due := reportDue(cfg, now); due {
		t.Fatal("本地时刻 04:00 早于触发点 09:00，不应到期")
	}
	// UTC (-720)：本地周四 08:00，同样早于触发点。
	cfg.TZOffsetMin = -720
	if _, _, _, due := reportDue(cfg, now); due {
		t.Fatal("本地 08:00 不应到期")
	}
	// UTC：本地周四 20:00，已过点，周期=本地昨天 2026-02-04。
	cfg.TZOffsetMin = 0
	key, _, _, due := reportDue(cfg, now)
	if !due || key != "2026-02-04" {
		t.Fatalf("due=%v key=%s", due, key)
	}
	// 本地日期跨天：周六 02:00 UTC（+600）→ 本地 12:00，昨天=周五 02-05。
	cfg.TZOffsetMin = 600
	later := time.Date(2026, 2, 6, 2, 0, 0, 0, time.UTC)
	key, _, _, due = reportDue(cfg, later)
	if !due || key != "2026-02-05" {
		t.Fatalf("due=%v key=%s（期望本地时区的昨天）", due, key)
	}

	// 周报：配置周一，周一过点才到期；周三对「配置周一」不到期，
	// 对「配置周三」则到期。
	wcfg := store.ReportConfig{Frequency: "weekly", TimeOfDay: "09:00", Weekday: 1, Monthday: 1}
	if k, _, _, d := reportDue(wcfg, time.Date(2026, 2, 9, 10, 0, 0, 0, time.UTC)); !d || k != "2026-W06" {
		t.Fatalf("周一应到期: d=%v k=%s", d, k)
	}
	wednesday := time.Date(2026, 2, 11, 10, 0, 0, 0, time.UTC)
	if _, _, _, d := reportDue(wcfg, wednesday); d {
		t.Fatal("周报配置周一、当天周三，不应到期")
	}
	wedCfg := wcfg
	wedCfg.Weekday = 3
	if _, _, _, d := reportDue(wedCfg, wednesday); !d {
		t.Fatal("周报配置周三、当天周三已过点，应到期")
	}

	// 月报：1 日过点发上月。
	mcfg := store.ReportConfig{Frequency: "monthly", TimeOfDay: "09:00", Monthday: 1, Weekday: 1}
	k, _, _, d := reportDue(mcfg, time.Date(2026, 2, 1, 9, 30, 0, 0, time.UTC))
	if !d || k != "2026-01" {
		t.Fatalf("月报应到期: d=%v k=%s", d, k)
	}
	if _, _, _, d := reportDue(mcfg, time.Date(2026, 2, 1, 8, 0, 0, 0, time.UTC)); d {
		t.Fatal("月报过点前不应到期")
	}
}

func TestValidateReportNormalizes(t *testing.T) {
	in := store.ReportConfig{Name: "  ", Frequency: "hourly", TimeOfDay: "25:99",
		TZOffsetMin: 9999, EndpointIDs: []int64{}}
	if err := validateReport(&in); err == nil {
		t.Fatal("非法频率应报错")
	}
	in.Frequency = "daily"
	in.TimeOfDay = "9:00"
	if err := validateReport(&in); err == nil {
		t.Fatal("非法时刻应报错")
	}
	in.TimeOfDay = "09:05"
	in.TZOffsetMin = 480
	if err := validateReport(&in); err == nil {
		t.Fatal("缺少端点应报错")
	}
	in.EndpointIDs = []int64{3, 3, 0, 2}
	if err := validateReport(&in); err != nil {
		t.Fatal(err)
	}
	if in.Name != "用量报告" || len(in.EndpointIDs) != 2 || in.EndpointIDs[0] != 3 {
		t.Fatalf("归一结果异常: %+v", in)
	}
}

func TestBuildReportContent(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()

	quota := money.Micro(100000000)
	issued, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, Label: "报表键", QuotaMicroUSD: &quota, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	seedRequest(t, st, "r1", from.Add(time.Hour), "gpt-4o", issued.KID, "ok", 5_000_000, 1_200)
	seedRequest(t, st, "r2", from.Add(2*time.Hour), "gpt-4o", issued.KID, "ok", 2_500_000, 800)
	seedRequest(t, st, "r3", from.Add(3*time.Hour), "claude-3", "", "rate_limited", 500_000, 300)

	cfg := store.ReportConfig{
		Name: "日报", Frequency: "daily", TimeOfDay: "09:00",
		Sections: mustJSON(map[string]any{
			"summary": true, "failures": true,
			"by_model": map[string]any{"on": true, "top": 5, "metric": "cost"},
			"by_key":   map[string]any{"on": true, "top": 5, "metric": "tokens"},
		}),
		EndpointIDs: []int64{1},
	}
	title, body, err := s.buildReport(ctx, cfg, "2026-02-04", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(title, "日报") || !strings.Contains(title, "2026-02-04") {
		t.Fatalf("title=%q", title)
	}
	for _, want := range []string{
		"请求 3 次",                        // 总览
		"gpt-4o",                        // 模型 Top
		"报表键",                           // 密钥 Top 用标签展示
		"$8",                            // 费用合计 8.0 USD（7.5+0.5）
		"失败请求", "rate_limited", "33.3%", // 失败占比 1/3
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺 %q:\n%s", want, body)
		}
	}
}

func TestReportsSweepEndToEnd(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()

	var mu sync.Mutex
	var calls []ntCall
	orig := shoutrrrSend
	shoutrrrSend = func(url, title, body string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, ntCall{url, title, body})
		return nil
	}
	t.Cleanup(func() { shoutrrrSend = orig })

	epID, err := s.SaveNotifyEndpoint(ctx, 0, "通道", "logger://r", true, "t")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	wantKey, from, to := reportPeriod("daily", now)
	quota := money.Micro(100000000)
	issued, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, Label: "日结键", QuotaMicroUSD: &quota, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	seedRequest(t, st, "e1", from.Add(30*time.Minute), "gpt-4o", issued.KID, "ok", 1_000_000, 900)

	if _, err := s.SaveReport(ctx, store.ReportConfig{
		Name: "每日报告", Enabled: true, Frequency: "daily", TimeOfDay: "00:00",
		Weekday: 1, Monthday: 1, TZOffsetMin: 0,
		Sections:    mustJSON(map[string]any{"summary": true}),
		EndpointIDs: []int64{epID},
	}, "t"); err != nil {
		t.Fatal(err)
	}

	// 首扫：到期发送一条，并推进 last_period。
	n, err := s.RunReportsSweep(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sent=%d err=%v", n, err)
	}
	mu.Lock()
	c := calls[0]
	mu.Unlock()
	if c.url != "logger://r" || !strings.Contains(c.title, wantKey) {
		t.Fatalf("call=%+v wantKey=%s", c, wantKey)
	}
	list, _ := s.ListReports(ctx)
	if list[0].LastPeriod != wantKey {
		t.Fatalf("last_period=%s want=%s", list[0].LastPeriod, wantKey)
	}

	// 同周期不重发。
	if n, _ := s.RunReportsSweep(ctx); n != 0 {
		t.Fatalf("同周期不应重发: n=%d", n)
	}
	_ = to
}
