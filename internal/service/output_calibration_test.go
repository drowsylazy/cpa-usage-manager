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
	seedRows(t, st, model, outputs, nil)
}

// seedDensityHistory 为模型插入带 body_len 的成功行：inputs[i] 是
// body_len 与 input_tokens 构成的样本对。
func seedDensityHistory(t *testing.T, st *store.Store, model string, pairs [][2]int64) {
	t.Helper()
	seedRows(t, st, model, nil, pairs)
}

// seedRows 落一批成功请求行：outputs 非空时按输出样本写入（body_len=0），
// pairs 非空时按 (body_len, input_tokens) 样本写入（输出 0）。
func seedRows(t *testing.T, st *store.Store, model string, outputs []int64, pairs [][2]int64) {
	t.Helper()
	ctx := context.Background()
	n := len(outputs) + len(pairs)
	base := time.Now().UTC().Add(-time.Duration(n+1) * time.Minute)
	for i, o := range outputs {
		r := store.Request{
			ID: model + "-out-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: model, Result: store.ResultOK, OutputTokens: o, TotalTokens: o,
		}
		if err := st.RecordPassiveUsage(ctx, r, store.PassiveDedupeHint{Models: []string{model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range pairs {
		r := store.Request{
			ID: model + "-den-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: model, Result: store.ResultOK, InputTokens: p[1], BodyLen: p[0], TotalTokens: p[1],
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

// TestCalibratedInput 锁输入密度学习三路径：
//  1. 有 (body_len, input) 样本 → 按中位数密度折算，显著低于混合密度估算；
//  2. 无样本（新模型）→ 回退混合密度估算值；
//  3. 学习密度折算异常偏低（上游谎报 token）→ 以混合密度 ×1.5 封底。
func TestCalibratedInput(t *testing.T) {
	s, st := testService(t)

	// 路径 2：无历史，回退混合密度估算（100KB ASCII → 25000）。
	fallback := estimateInputTokens(make([]byte, 100_000)) // 全 ASCII
	if got := s.calibratedInput("fresh-model", 100_000, fallback, 1_000_000); got != fallback {
		t.Fatalf("无样本应回退混合密度: got %d want %d", got, fallback)
	}

	// 路径 1：10 条样本，密度 7000–7600 毫密度（7–7.6 字节/token），
	// 中位数约 7300 → 100KB 体折算约 13698 token，远低于 /4 的 25000。
	pairs := make([][2]int64, 10)
	for i := range pairs {
		density := int64(7000 + i*67) // 7000..7603 毫密度
		pairs[i] = [2]int64{400_000, 400_000 * 1000 / density}
	}
	seedDensityHistory(t, st, "dense-model", pairs)
	got := s.calibratedInput("dense-model", 100_000, fallback, 1_000_000)
	if got < 13_000 || got > 14_500 {
		t.Fatalf("密度学习折算异常: got %d want 13000..14500", got)
	}

	// 路径 3：上游只报 1/10 输入 token（密度样本 40000 毫密度），
	// 折算 2500 会被 ×1.5 封底拉回 fallback 附近（不低于 fallback）。
	liar := make([][2]int64, 5)
	for i := range liar {
		liar[i] = [2]int64{40_000, 1_000}
	}
	seedDensityHistory(t, st, "liar-model", liar)
	liarFallback := estimateInputTokens(make([]byte, 100_000))
	if got := s.calibratedInput("liar-model", 100_000, liarFallback, 1_000_000); got < liarFallback {
		t.Fatalf("异常低密度应被 ×1.5 封底（不低于混合密度估算）: got %d fallback %d", got, liarFallback)
	}
}

// TestBuildReservePlanDensityCalibration 端到端：密度样本存在时
// BuildReservePlan 的输入估算按学习密度走，而不是混合密度。
func TestBuildReservePlanDensityCalibration(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	tieredPricingRule(t, s, "dense-test")

	// 密度 7300 毫密度（glm-5.3 实测口径）。
	pairs := make([][2]int64, 8)
	for i := range pairs {
		pairs[i] = [2]int64{730_000, 100_000}
	}
	seedDensityHistory(t, st, "dense-test", pairs)

	body := make([]byte, 730_000) // 全 ASCII：混合密度会估 182500
	for i := range body {
		body[i] = 'a'
	}
	body[0] = '{'
	body[len(body)-1] = '}'
	plan, err := s.BuildReservePlan(ctx, "dense-test", body)
	if err != nil {
		t.Fatal(err)
	}
	if plan.InputEstimate < 95_000 || plan.InputEstimate > 115_000 {
		t.Fatalf("输入估算应按学习密度（约 100K）而非混合密度（182K）: %d", plan.InputEstimate)
	}
}
