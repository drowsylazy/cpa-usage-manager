package service

import (
	"context"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// seedOutputHistory 为模型插入 n 条成功请求行，输出 token 取
// outputs[i%len]，时间递增（越靠后越新）。
func seedOutputHistory(t *testing.T, st *store.Store, model string, outputs []int64) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Duration(len(outputs)+1) * time.Minute)
	for i, o := range outputs {
		r := store.Request{
			ID: model + "-hist-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: model, Result: store.ResultOK, OutputTokens: o, TotalTokens: o,
		}
		if err := st.RecordPassiveUsage(ctx, r, store.PassiveDedupeHint{Models: []string{model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCalibratedOutput 锁住输出预占的历史校准三路径：
//  1. 有成功历史 → P95×1.25，且不越过 max_tokens；
//  2. max_tokens 小于校准值 → 封顶到 max_tokens（客户端显式上限优先）；
//  3. 无历史（新模型/空库）→ 回退 min(max, max(default, max/8))，
//     不再按 max_tokens 全额虚占（agent 客户端拍 128000 的主场景）。
func TestCalibratedOutput(t *testing.T) {
	s, st := testService(t)

	// 路径 3：无历史回退。max_tokens=128000 → 128000/8=16000。
	if got := s.calibratedOutput("new-model", 128_000, 4096); got != 16_000 {
		t.Fatalf("无历史回退异常: got %d want 16000", got)
	}
	// 无历史且 max_tokens 小（普通客户端）→ 不放大、不缩小。
	if got := s.calibratedOutput("new-model", 512, 4096); got != 512 {
		t.Fatalf("小 max_tokens 不应被抬高: got %d want 512", got)
	}
	// 无 max_tokens：defaultOutput 原样返回。
	if got := s.calibratedOutput("new-model", 4096, 4096); got != 4096 {
		t.Fatalf("default 输出不应变化: got %d want 4096", got)
	}

	// 路径 1：20 条历史输出 1000..20000，P95 约 19000，×1.25 ≈ 23750，
	// 低于 max_tokens=128000 → 采用校准值。
	outs := make([]int64, 20)
	for i := range outs {
		outs[i] = int64(1000 + i*1000)
	}
	seedOutputHistory(t, st, "hist-model", outs)
	if got := s.calibratedOutput("hist-model", 128_000, 4096); got < 19_000 || got > 25_000 {
		t.Fatalf("历史校准值异常: got %d want 19000..25000", got)
	}

	// 路径 2：max_tokens=8000 < 校准值 → 封顶。
	if got := s.calibratedOutput("hist-model", 8_000, 4096); got != 8_000 {
		t.Fatalf("max_tokens 封顶异常: got %d want 8000", got)
	}
}

// TestBuildReservePlanCalibration 端到端锁校准进入预占计划：
// ZCode 实测场景（max_tokens=128000、实际输出几 K）修复后输出预占
// 应落在历史 P95 量级，而不是 128000。
func TestBuildReservePlanCalibration(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	tieredPricingRule(t, s, "glm-test")

	outs := make([]int64, 30)
	for i := range outs {
		outs[i] = int64(4000 + i*100) // 实际输出 4K..6.9K
	}
	seedOutputHistory(t, st, "glm-test", outs)

	body := []byte(`{"model":"glm-test","max_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`)
	plan, err := s.BuildReservePlan(ctx, "glm-test", body)
	if err != nil {
		t.Fatal(err)
	}
	if plan.OutputEstimate > 12_000 {
		t.Fatalf("输出预占应按历史 P95 校准（约 8K），不是 max_tokens: %d", plan.OutputEstimate)
	}
	if plan.OutputEstimate < 5_000 {
		t.Fatalf("校准值不应低于历史区间: %d", plan.OutputEstimate)
	}
	// 输入估算：body 为纯 ASCII，应显著低于 len/3。
	if plan.InputEstimate > int64(len(body))/3 {
		t.Fatalf("ASCII 输入估算应低于整包 /3: %d", plan.InputEstimate)
	}
}

// TestOutputP95 锁分位函数：样本不足回退、加权 P95 落点、单调性。
func TestOutputP95(t *testing.T) {
	if _, ok := outputP95(nil); ok {
		t.Fatal("空样本不应可用")
	}
	if _, ok := outputP95([]int64{100}); ok {
		t.Fatal("单样本不应可用")
	}
	if _, ok := outputP95([]int64{100, 200}); ok {
		t.Fatal("双样本不应可用")
	}
	// 均匀 1..100：P95 应落在 94..100（近端加权把分位略拉高）。
	samples := make([]int64, 100)
	for i := range samples {
		samples[i] = int64(i + 1)
	}
	p, ok := outputP95(samples)
	if !ok || p < 94 || p > 100 {
		t.Fatalf("P95 落点异常: %d %v", p, ok)
	}
	// 单点长尾：前 99 个 1000、最后 1 个 50000 → P95 仍在主体内
	// （长尾由 ×1.25 余量与 max_tokens 封顶兜住，不追长尾）。
	tailed := make([]int64, 100)
	for i := range tailed[:99] {
		tailed[i] = 1000
	}
	tailed[99] = 50000
	if p, _ := outputP95(tailed); p > 2000 {
		t.Fatalf("P95 不应被单点长尾拉飞: %d", p)
	}
}
