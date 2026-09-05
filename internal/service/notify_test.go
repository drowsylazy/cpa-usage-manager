package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

type ntCall struct {
	url, title, body string
}

func setSpent(t *testing.T, st *store.Store, kid string, micro int64) {
	t.Helper()
	err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(),
			`UPDATE plugin_keys SET spent_micro_usd=? WHERE kid=?`, micro, kid)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNotifySweepEdgeTriggers(t *testing.T) {
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

	if err := s.SaveNotifySettings(ctx, NotifySettings{Enabled: true, WarnPct: 20}, "t"); err != nil {
		t.Fatal(err)
	}
	epID, err := s.SaveNotifyEndpoint(ctx, 0, "测试通道", "logger://test", true, "t")
	if err != nil {
		t.Fatal(err)
	}
	quota := money.Micro(100000000) // $100
	issued, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, Label: "告警键", QuotaMicroUSD: &quota, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}

	// 未越线：不发送
	if n, err := s.RunNotifySweep(ctx); err != nil || n != 0 {
		t.Fatalf("未越线不应发送: n=%d err=%v", n, err)
	}

	// 越线（余 $5/$100 = 5% ≤ 20%）：发送一次 Warning
	setSpent(t, st, issued.KID, 95000000)
	if n, err := s.RunNotifySweep(ctx); err != nil || n != 1 {
		t.Fatalf("越线应发送一条: n=%d err=%v", n, err)
	}
	mu.Lock()
	c0 := calls[0]
	mu.Unlock()
	if c0.url != "logger://test" || !strings.Contains(c0.title, "告警") {
		t.Fatalf("首条告警异常: %+v", c0)
	}
	for _, want := range []string{"金额总额", "告警键", "$5"} {
		if !strings.Contains(c0.body, want) {
			t.Fatalf("消息缺 %q：%q", want, c0.body)
		}
	}

	// 状态未翻转：不重复发
	if n, _ := s.RunNotifySweep(ctx); n != 0 {
		t.Fatalf("同一状态不应重复发: n=%d", n)
	}

	// 提额解除 → 再越线：重新武装并发送
	newQuota := money.Micro(200000000) // $200
	qp := &newQuota
	if _, err := s.UpdateKey(ctx, issued.KID, store.KeyUpdate{QuotaMicroUSD: &qp}, "t"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.RunNotifySweep(ctx); n != 0 {
		t.Fatalf("提额后余量充足，不应发送: n=%d", n)
	}
	setSpent(t, st, issued.KID, 190000000)
	if n, err := s.RunNotifySweep(ctx); err != nil || n != 1 {
		t.Fatalf("重新越线应再次发送: n=%d err=%v", n, err)
	}

	// 过期键：Error 级
	exp := time.Now().UTC().Add(-time.Hour)
	issued2, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, Label: "过期键", ExpiresAt: &exp, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	base := len(calls)
	mu.Unlock()
	if n, err := s.RunNotifySweep(ctx); err != nil || n != 1 {
		t.Fatalf("过期应发送一条: n=%d err=%v", n, err)
	}
	mu.Lock()
	cExp := calls[base]
	mu.Unlock()
	if !strings.Contains(cExp.title, "严重") || !strings.Contains(cExp.body, "已过期") {
		t.Fatalf("过期告警异常: %+v", cExp)
	}

	// 端点停用后不再发送（新事件也不发）
	if _, err := s.SaveNotifyEndpoint(ctx, epID, "测试通道", "logger://test", false, "t"); err != nil {
		t.Fatal(err)
	}
	quota3 := money.Micro(50)
	issued3, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, Label: "第三键", QuotaMicroUSD: &quota3, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	setSpent(t, st, issued3.KID, 49)
	if n, _ := s.RunNotifySweep(ctx); n != 0 {
		t.Fatalf("端点停用后不应发送: n=%d", n)
	}

	// 删除 Key 后状态行被清理（重新启用端点，让扫描不再短路）
	if err := s.DeleteKey(ctx, issued2.KID, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveNotifyEndpoint(ctx, epID, "测试通道", "logger://test", true, "t"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	base = len(calls)
	mu.Unlock()
	if n, err := s.RunNotifySweep(ctx); err != nil || n != 1 {
		t.Fatalf("重新启用后应补发第三键的越线: n=%d err=%v", n, err)
	}
	if s.loadNotifyState(ctx)[issued2.KID] != nil {
		t.Fatal("已删除 Key 的状态应被清理")
	}
}

func TestNotifySettingsClampAndRoundtrip(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if err := s.SaveNotifySettings(ctx, NotifySettings{Enabled: true, WarnPct: 999}, "t"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNotifySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.WarnPct != 95 {
		t.Fatalf("阈值应钳制到 95: %+v", got)
	}
	// 非法 JSON 回退默认值
	if err := s.SaveNotifySettings(ctx, NotifySettings{Enabled: true, WarnPct: 30}, "t"); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetPreference(ctx, notifySettingsKey, "{broken"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetNotifySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.WarnPct != 20 || got.Enabled {
		t.Fatalf("坏数据应回退默认: %+v", got)
	}
}

func TestNotifyErrorEvent(t *testing.T) {
	s, _ := testService(t)
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

	if _, err := s.SaveNotifyEndpoint(ctx, 0, "通道", "logger://err", true, "t"); err != nil {
		t.Fatal(err)
	}

	// 开关默认关闭：不上报
	s.NotifyErrorEvent(ctx, "report", "第一次失败")
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("开关关闭不应发送: %d", n)
	}

	if err := s.SaveNotifySettings(ctx, NotifySettings{Enabled: true, WarnPct: 20, ErrorAlerts: true}, "t"); err != nil {
		t.Fatal(err)
	}
	s.NotifyErrorEvent(ctx, "report", "第一次失败")
	mu.Lock()
	n = len(calls)
	var c0 ntCall
	if n > 0 {
		c0 = calls[0]
	}
	mu.Unlock()
	if n != 1 || !strings.Contains(c0.title, "错误上报") || !strings.Contains(c0.body, "report") {
		t.Fatalf("开启后应上报一条: n=%d c0=%+v", n, c0)
	}

	// 冷却：同来源一小时内不重复上报
	s.NotifyErrorEvent(ctx, "report", "第二次失败")
	mu.Lock()
	n = len(calls)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("同来源冷却期内不应重复: %d", n)
	}
	// 不同来源不受限
	s.NotifyErrorEvent(ctx, "storage", "只读降级")
	mu.Lock()
	n = len(calls)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("不同来源应独立上报: %d", n)
	}
}

func TestReportFailureTriggersErrorAlert(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()

	var mu sync.Mutex
	var calls []ntCall
	orig := shoutrrrSend
	shoutrrrSend = func(url, title, body string) error {
		mu.Lock()
		calls = append(calls, ntCall{url, title, body})
		mu.Unlock()
		if strings.Contains(title, "错误上报") {
			return nil // 错误上报走的是同一端点：允许成功，模拟通道只是拒发报告
		}
		return errors.New("模拟通道故障")
	}
	t.Cleanup(func() { shoutrrrSend = orig })

	epID, err := s.SaveNotifyEndpoint(ctx, 0, "故障通道", "logger://bad", true, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveNotifySettings(ctx, NotifySettings{Enabled: true, WarnPct: 20, ErrorAlerts: true}, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveReport(ctx, store.ReportConfig{
		Name: "会失败的报告", Enabled: true, Frequency: "daily", TimeOfDay: "00:00",
		Weekday: 1, Monthday: 1, TZOffsetMin: 0,
		Sections:    mustJSON(map[string]any{"summary": true}),
		EndpointIDs: []int64{epID},
	}, "t"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RunReportsSweep(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := len(calls)
	var last ntCall
	if n > 0 {
		last = calls[n-1]
	}
	mu.Unlock()
	if n < 2 || !strings.Contains(last.title, "错误上报") ||
		!strings.Contains(last.body, "会失败的报告") || !strings.Contains(last.body, "发送失败") {
		t.Fatalf("报告失败应联动错误上报: n=%d last=%+v", n, last)
	}
}

func TestNotifyEndpointEncryptionRoundtrip(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	id, err := s.SaveNotifyEndpoint(ctx, 0, "加密验证", "telegram://secret-token@bot", true, "t")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListNotifyEndpoints(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}
	if list[0].URL != "telegram://secret-token@bot" {
		t.Fatalf("解密回显不符: %q", list[0].URL)
	}
	// 库里不能有明文 URL
	st := s.Store()
	var blob []byte
	err = st.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `SELECT url_enc FROM notify_endpoints WHERE id=?`, id).Scan(&blob)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "secret-token") {
		t.Fatal("URL 不应落明文")
	}
	if _, err := s.SaveNotifyEndpoint(ctx, 0, "", "", true, "t"); err == nil {
		t.Fatal("空 URL 应报错")
	}
	if _, err := s.SaveNotifyEndpoint(ctx, 99999, "x", "logger://y", true, "t"); err == nil {
		t.Fatalf("更新不存在的端点应报错: %v", err)
	}
}

func TestNotifySingleUsageAlert(t *testing.T) {
	s, _ := testService(t)
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

	if err := s.SaveNotifySettings(ctx, NotifySettings{
		SingleCostAlert:      true,
		SingleCostMicroUSD:   1_000_000, // $1
		SingleTokenThreshold: 10_000,
	}, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveNotifyEndpoint(ctx, 0, "通道", "logger://single", true, "t"); err != nil {
		t.Fatal(err)
	}
	issued, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, Label: "异常键", Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}

	// 未达阈值：不发送（钩子直接调用，同步可断言）。
	s.maybeNotifySingleUsage(issued.KID, "gpt-x", money.Micro(500_000), 5_000)
	mu.Lock()
	n0 := len(calls)
	mu.Unlock()
	if n0 != 0 {
		t.Fatalf("未达阈值不应发送: %d", n0)
	}

	// 费用超阈值：结算路径异步发送一条。
	res, err := s.Reserve(ctx, ReservationRequest{KeyID: issued.KID, CallerID: store.DefaultCallerID, Model: "gpt-x", EstimatedTokens: 12_000, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	usage := usageparse.Usage{InputTokens: 6_000, OutputTokens: 6_000, TotalTokens: 12_000}
	// gpt-x 无计价规则 → 费用 0，靠 Token 阈值触发。
	if _, err := s.Settle(ctx, res.ID, usage, nil, false); err != nil {
		t.Fatal(err)
	}
	waitFor := func(cond func(n int) bool) ntCall {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(calls)
			var c ntCall
			if n > 0 {
				c = calls[n-1]
			}
			mu.Unlock()
			if n > 0 && cond(n) {
				return c
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("3s 内未收到告警")
		return ntCall{}
	}
	c := waitFor(func(n int) bool { return n >= 1 })
	if !strings.Contains(c.title, "异常") || !strings.Contains(c.body, "12000") || !strings.Contains(c.body, "异常键") {
		t.Fatalf("告警内容不符: %+v", c)
	}

	// 冷却期内再触发：不再发送。
	s.maybeNotifySingleUsage(issued.KID, "gpt-x", money.Micro(2_000_000), 20_000)
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	n1 := len(calls)
	mu.Unlock()
	if n1 != 1 {
		t.Fatalf("冷却期内不应重复发送: %d", n1)
	}

	// 开关关闭：不发送。
	if err := s.SaveNotifySettings(ctx, NotifySettings{}, "t"); err != nil {
		t.Fatal(err)
	}
	s.notifyCfgMu.Lock()
	s.notifyCfg = nil
	s.notifyCfgMu.Unlock()
	s.maybeNotifySingleUsage(issued.KID, "gpt-x", money.Micro(2_000_000), 20_000)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n2 := len(calls)
	mu.Unlock()
	if n2 != 1 {
		t.Fatalf("开关关闭不应发送: %d", n2)
	}
}

// TestNotifyErrorRateAlert 错误率告警：滑动窗口内失败占比超阈值边沿触发，
// 恢复后重新武装；窗口取分钟聚合口径。
func TestNotifyErrorRateAlert(t *testing.T) {
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

	if err := s.SaveNotifySettings(ctx, NotifySettings{
		Enabled: true, WarnPct: 20,
		ErrorRateAlert: true, ErrorRateWindowMin: 10, ErrorRatePct: 50,
	}, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveNotifyEndpoint(ctx, 0, "测试通道", "logger://test", true, "t"); err != nil {
		t.Fatal(err)
	}

	// 窗口内 10 笔全失败 → 100% ≥ 50%，触发一次。
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		r := store.Request{
			ID: fmt.Sprintf("err-%d", i), TS: now.Add(-time.Duration(i) * time.Second),
			CallerID: store.DefaultCallerID, Model: "gpt-test", Result: store.ResultError,
		}
		if err := st.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.RunNotifySweep(ctx); err != nil || n != 1 {
		t.Fatalf("错误率超阈值应发送一条: n=%d err=%v", n, err)
	}
	mu.Lock()
	c := calls[len(calls)-1]
	mu.Unlock()
	if !strings.Contains(c.title, "错误率") || !strings.Contains(c.body, "10 笔请求失败 10 笔") {
		t.Fatalf("错误率告警内容异常: %+v", c)
	}

	// 条件持续：不重复发。
	if n, _ := s.RunNotifySweep(ctx); n != 0 {
		t.Fatalf("同一状态不应重复发: n=%d", n)
	}
}

// TestNotifyExpiringSoonAlert 临期预警：到期前 N 天推一次，不与过期告警重叠。
func TestNotifyExpiringSoonAlert(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	_ = st

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

	if err := s.SaveNotifySettings(ctx, NotifySettings{
		Enabled: true, WarnPct: 20, ExpireWarnDays: 7,
	}, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveNotifyEndpoint(ctx, 0, "测试通道", "logger://test", true, "t"); err != nil {
		t.Fatal(err)
	}

	// 3 天后到期：落在 7 天预警窗口内，触发一次。
	exp := time.Now().UTC().Add(72 * time.Hour)
	issued, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, Label: "临期键", ExpiresAt: &exp, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_ = issued
	if n, err := s.RunNotifySweep(ctx); err != nil || n != 1 {
		t.Fatalf("临期应发送一条: n=%d err=%v", n, err)
	}
	mu.Lock()
	c := calls[len(calls)-1]
	mu.Unlock()
	if !strings.Contains(c.body, "到期") || !strings.Contains(c.body, "3 天") {
		t.Fatalf("临期告警内容异常: %+v", c)
	}
	// 条件持续：不重复发。
	if n, _ := s.RunNotifySweep(ctx); n != 0 {
		t.Fatalf("同一状态不应重复发: n=%d", n)
	}
}
