package service

import (
	"context"
	"errors"
	"strconv"
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

// seedDensityHistoryAt 是 seedDensityHistory 的显式起始时刻版本：
// 用于「学习基线设在过去、样本落在基线之后」的重置类测试。
func seedDensityHistoryAt(t *testing.T, st *store.Store, model string, pairs [][2]int64, start time.Time) {
	t.Helper()
	ctx := context.Background()
	batch := seedRowBatch
	seedRowBatch++
	for i, p := range pairs {
		r := store.Request{
			ID:    model + "-den-at-" + strconv.FormatInt(batch, 10) + "-" + strconv.Itoa(i),
			TS:    start.Add(time.Duration(i) * time.Second),
			Model: model, Result: store.ResultOK, InputTokens: p[1], BodyLen: p[0], TotalTokens: p[1],
		}
		if err := st.RecordPassiveUsage(ctx, r, store.PassiveDedupeHint{Models: []string{model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(pairs); n > 0 {
		seedRowLast = start.Add(time.Duration(n-1) * time.Second)
	}
}

// seedRows 落一批成功请求行：outputs 非空时按输出样本写入（body_len=0），
// pairs 非空时按 (body_len, input_tokens) 样本写入（输出 0）。
//
// 跨批次时间与 ID 都严格递增（包级游标）：此前每次调用各自以 now 为锚
// 且 ID 只按批内下标生成——后一批的时间戳比前一批早、ID 还与前一批
// 撞车被 errDuplicateRequest 静默吞掉，漂移测试的「新样本」压根没落库
// （RecentDensities 按时间倒序取样，批次顺序必须与调用顺序一致）。
var (
	seedRowLast  time.Time
	seedRowBatch int64
)

func seedRows(t *testing.T, st *store.Store, model string, outputs []int64, pairs [][2]int64) {
	t.Helper()
	ctx := context.Background()
	n := len(outputs) + len(pairs)
	base := time.Now().UTC().Add(-time.Duration(n+1) * time.Minute)
	if !seedRowLast.IsZero() && base.Before(seedRowLast) {
		base = seedRowLast.Add(time.Minute)
	}
	batch := seedRowBatch
	seedRowBatch++
	idOf := func(kind string, i int) string {
		return model + "-" + kind + "-" + strconv.FormatInt(batch, 10) + "-" + strconv.Itoa(i)
	}
	ts := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }
	for i, o := range outputs {
		r := store.Request{
			ID: idOf("out", i), TS: ts(i),
			Model: model, Result: store.ResultOK, OutputTokens: o, TotalTokens: o,
		}
		if err := st.RecordPassiveUsage(ctx, r, store.PassiveDedupeHint{Models: []string{model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range pairs {
		r := store.Request{
			ID: idOf("den", i), TS: ts(i),
			Model: model, Result: store.ResultOK, InputTokens: p[1], BodyLen: p[0], TotalTokens: p[1],
		}
		if err := st.RecordPassiveUsage(ctx, r, store.PassiveDedupeHint{Models: []string{model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	if n > 0 {
		seedRowLast = ts(n - 1)
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

// TestCalibratedInput 锁输入密度学习路径：
//
//  1. 有 (body_len, input) 样本 → 按中位数密度折算，显著低于混合密度估算；
//  2. 无样本（新模型）→ 回退混合密度估算值；
//  3. 绝对 sanity 带：上游谎报 token 的密度样本（折算值远超 fallback 5×）
//     → 回退混合密度估算。
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

	// 路径 3（绝对 sanity 带）：上游只报 1/10 输入 token——密度样本
	// 40000 毫密度，折算 2500 token 远低于 fallback 的 0.2×（5000），
	// 属于构成级错误，回退混合密度而不是采信。
	liar := make([][2]int64, 5)
	for i := range liar {
		liar[i] = [2]int64{40_000, 1_000}
	}
	seedDensityHistory(t, st, "liar-model", liar)
	liarFallback := estimateInputTokens(make([]byte, 100_000))
	if got := s.calibratedInput("liar-model", 100_000, liarFallback, 1_000_000); got != liarFallback {
		t.Fatalf("单位级错误密度应回退混合密度: got %d fallback %d", got, liarFallback)
	}
}

// TestCalibratedInputBand 锁预测侧的绝对 sanity 带：折算值超出混合密度
// 估算 [0.2×, 5×] 的构成级错误（上游谎报 token、请求体突变成 base64
// 图片密集型）回退固定估算；带内采信学习密度——包括只有混合估算 0.55×
// 的真实模型密度（glm-5.3 实测口径，旧版锚在 fallback 的 [0.5×,1.5×] 带
// 会拦掉这类场景）。旧版还有一层「MAD 相对带」已被判定为死代码删除：
// 预测时无从得知当前请求的真实密度，按折算值反推等效密度再比对样本带
// 是同义反复（反推值恒等于样本密度本身），从不拒绝任何请求。
func TestCalibratedInputBand(t *testing.T) {
	s, st := testService(t)

	// 高度一致样本：全部 7300 毫密度。
	consistent := make([][2]int64, 8)
	for i := range consistent {
		consistent[i] = [2]int64{730_000, 100_000}
	}
	seedDensityHistory(t, st, "consistent-model", consistent)
	fallback := estimateInputTokens(make([]byte, 100_000)) // 25000

	// 折算 7300 → 13698 token：0.55×fallback，在 [0.2×,5×] 带内 → 采信。
	if got := s.calibratedInput("consistent-model", 100_000, fallback, 1_000_000); got < 13_000 || got > 14_000 {
		t.Fatalf("一致样本的 0.55× 折算应被采信: got %d", got)
	}

	// 构成级错误回退：折算 10_000_000×1000/7300 ≈ 1.37M token，
	// 远超 fallback 的 5×（125000）→ 回退固定估算。
	if got := s.calibratedInput("consistent-model", 10_000_000, 25_000, 100_000_000); got != 25_000 {
		t.Fatalf("折算超出 5× 绝对带应回退固定估算: got %d want 25000", got)
	}
	// 反向：折算低于 0.2× 同样回退。
	if got := s.calibratedInput("consistent-model", 10_000, 2_500_000, 100_000_000); got != 2_500_000 {
		t.Fatalf("折算低于 0.2× 绝对带应回退固定估算: got %d want 2500000", got)
	}

	// 离散样本（混杂构成）：中位数仍是最稳的点估计，带内采信。
	mixed := make([][2]int64, 8)
	for i := range mixed {
		if i%2 == 0 {
			mixed[i] = [2]int64{730_000, 100_000}
		} else {
			mixed[i] = [2]int64{400_000, 20_000} // 20000 毫密度
		}
	}
	seedDensityHistory(t, st, "mixed-model", mixed)
	if got := s.calibratedInput("mixed-model", 100_000, fallback, 50_000); got <= 0 || got > 50_000 {
		t.Fatalf("离散样本下折算异常: %d", got)
	}
}

// TestCalibratedInputDrift 锁 compact 场景的漂移适应（端到端）：agent
// 执行 /compact 后请求体构成突变（摘要散文替代原始代码/工具记录），
// 真实密度从 7300 漂到 ~10430 毫密度（实测占比 100%→50% 的反推）。
// 全窗中位数要新样本过半才拉动；漂移探测让 10 条新流量即部分恢复
// （近期窗口半新半旧），20 条完全接管。
func TestCalibratedInputDrift(t *testing.T) {
	s, st := testService(t)

	// 30 条旧构成 7300 毫密度。
	history := make([][2]int64, 0, 50)
	for i := 0; i < 30; i++ {
		history = append(history, [2]int64{730_000, 100_000})
	}
	seedDensityHistory(t, st, "drift-model", history)

	// compact 前：730KB 请求体估 100K（占比 ~100%）。
	fallback := estimateInputTokens(make([]byte, 730_000))
	if got := s.calibratedInput("drift-model", 730_000, fallback, 10_000_000); got < 99_000 || got > 101_000 {
		t.Fatalf("compact 前应按旧密度折算 100K: got %d", got)
	}
	// 清密度缓存：60s TTL 内同模型共享一份读数，落库的新样本要下个
	// 窗口才生效——线上是预期行为，测试里手动失效以验证学习侧逻辑。
	s.denCalMu.Lock()
	s.denCal = nil
	s.denCalMu.Unlock()

	// compact 后：10 条新构成请求（同 730KB 体但真实 token 只有 ~70K
	// → 密度 10430）。近期窗口 20 条 = 10 新 + 10 旧 → 中位
	// (7300+10430)/2 = 8865：730KB 体估 82.4K，对真实 70K 占比 ~118%
	// （旧口径 104K/70K = 148% 预估虚高；全窗中位数要 30+ 条才动）。
	post := make([][2]int64, 0, 10)
	for i := 0; i < 10; i++ {
		post = append(post, [2]int64{730_000, 70_000})
	}
	seedDensityHistory(t, st, "drift-model", post)
	s.denCalMu.Lock()
	s.denCal = nil
	s.denCalMu.Unlock()
	got := s.calibratedInput("drift-model", 730_000, fallback, 10_000_000)
	if got < 81_000 || got > 84_000 {
		t.Fatalf("compact 后 10 条新流量应部分恢复（近期窗口中位）: got %d want ~82400", got)
	}

	// 再来 10 条（窗口完全换血）→ 完全接管 10430：730KB 估 70K。
	more := make([][2]int64, 0, 10)
	for i := 0; i < 10; i++ {
		more = append(more, [2]int64{730_000, 70_000})
	}
	seedDensityHistory(t, st, "drift-model", more)
	s.denCalMu.Lock()
	s.denCal = nil
	s.denCalMu.Unlock()
	got = s.calibratedInput("drift-model", 730_000, fallback, 10_000_000)
	if got < 69_000 || got > 71_000 {
		t.Fatalf("窗口换血完毕应完全接管新密度: got %d want ~70000", got)
	}
}

// TestResetModelDensity 锁「重置估算密度」：把学习基线推到此刻后，
// 旧样本立即退出折算（不等 60s 缓存过期）、新流量按当前构成重新学习。
//
// 时间全部显式给出（不依赖 seedRows 的包级游标）：跨测试残留的
// seedRowLast 可能把本测试的「旧样本」推到未来，导致基线拦不住它们。
func TestResetModelDensity(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	seedRowLast = time.Time{} // 清跨测试残留的批次游标

	// 旧构成：8 条 7300 毫密度，落在 1 小时前。730KB 体估 100K。
	history := make([][2]int64, 0, 8)
	for i := 0; i < 8; i++ {
		history = append(history, [2]int64{730_000, 100_000})
	}
	seedDensityHistoryAt(t, st, "reset-model", history, time.Now().UTC().Add(-time.Hour))
	fallback := estimateInputTokens(make([]byte, 730_000))
	if got := s.calibratedInput("reset-model", 730_000, fallback, 10_000_000); got < 99_000 || got > 101_000 {
		t.Fatalf("重置前应按旧密度折算 100K: got %d", got)
	}

	// 重置：基线推到此刻，旧样本立即退出（缓存同步失效，不等 60s）。
	if err := s.ResetModelDensity(ctx, "reset-model", "test"); err != nil {
		t.Fatal(err)
	}
	// 基线之后还没样本 → 回退固定混合密度（而非沿用旧读数）。
	got := s.calibratedInput("reset-model", 730_000, fallback, 10_000_000)
	if got != fallback {
		t.Fatalf("重置后无新样本应回退固定估算: got %d want %d", got, fallback)
	}
	// 缓存份额同样失效 → 金额拆档回退输入侧最贵档。
	if _, _, ok := s.cacheSharesCached("reset-model"); ok {
		t.Fatal("重置后缓存份额应不可用")
	}

	// 新构成（同体量请求真实 token 少三成）跑够 3 条 → 按新构成接管。
	// 落在基线之后（真实场景里新流量天然晚于重置时刻）。
	post := make([][2]int64, 0, 3)
	for i := 0; i < 3; i++ {
		post = append(post, [2]int64{730_000, 70_000})
	}
	seedDensityHistoryAt(t, st, "reset-model", post, time.Now().UTC().Add(time.Minute))
	s.denCalMu.Lock()
	s.denCal = nil
	s.denCalMu.Unlock()
	if got := s.calibratedInput("reset-model", 730_000, fallback, 10_000_000); got < 69_000 || got > 71_000 {
		t.Fatalf("重置后应按新构成学习: got %d want ~70000", got)
	}

	// 空模型名拒绝。
	if err := s.ResetModelDensity(ctx, "   ", "test"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空模型名应报参数错误: %v", err)
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
