package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestRecentOutputTokens 锁输出预占校准的数据源查询：只取该模型
// result='ok' 且 output_tokens>0 的行，按 ts 倒序取样本后升序返回。
// 失败行与零输出行（无响应释放/空回复/缺 usage 兜底）会污染校准基线。
func TestRecentOutputTokens(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "recent-output")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	mk := func(id, model, result string, out int64, at time.Time) Request {
		return Request{ID: id, TS: at, Model: model, Result: result,
			OutputTokens: out, TotalTokens: out}
	}
	rows := []Request{
		mk("ok-3", "m", ResultOK, 3000, base.Add(3*time.Minute)),
		mk("ok-1", "m", ResultOK, 1000, base.Add(1*time.Minute)),
		mk("err", "m", ResultError, 9999, base.Add(2*time.Minute)), // 失败行剔除
		mk("zero", "m", ResultOK, 0, base.Add(4*time.Minute)),      // 零输出剔除
		mk("other", "m2", ResultOK, 7777, base.Add(5*time.Minute)), // 其他模型剔除
		mk("ok-2", "m", ResultOK, 2000, base.Add(6*time.Minute)),
	}
	for _, r := range rows {
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{r.Model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecentOutputTokens(ctx, "m", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("应只返回 3 条有效样本，得到 %d: %v", len(got), got)
	}
	// 升序：1000, 2000, 3000。
	for i, want := range []int64{1000, 2000, 3000} {
		if got[i] != want {
			t.Fatalf("样本[%d] = %d want %d（应升序）", i, got[i], want)
		}
	}
	// limit 生效：取最近 2 条（3000 与 2000）。
	limited, err := s.RecentOutputTokens(ctx, "m", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0] != 2000 || limited[1] != 3000 {
		t.Fatalf("limit=2 应取最近两条升序: %v", limited)
	}
}

// TestRecentDensities 锁输入密度学习的样本查询：分母是完整上下文
// （input + cache_read + cache_creation——Claude 口径 input 不含缓存读写），
// 只取该模型成功、body_len>0 且上下文>0 的行，body_len<1024 的短体噪声
// 剔除，返回毫密度（body_len÷上下文×1000）升序。
func TestRecentDensities(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "recent-density")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	mk := func(id, model, result string, bodyLen, inTok, cacheRead, cacheCreate int64, at time.Time) Request {
		return Request{ID: id, TS: at, Model: model, Result: result,
			BodyLen: bodyLen, InputTokens: inTok, CacheReadTokens: cacheRead,
			CacheCreationTokens: cacheCreate, TotalTokens: inTok + cacheRead + cacheCreate}
	}
	rows := []Request{
		mk("d-1", "m", ResultOK, 730_000, 100_000, 0, 0, base.Add(1*time.Minute)), // 7300
		mk("d-2", "m", ResultOK, 400_000, 50_000, 0, 0, base.Add(2*time.Minute)),  // 8000
		// Claude 口径：input=2000 但缓存读 98000——上下文 100K，
		// 密度 7300（与 d-1 同），分母不补会错算成 365000。
		mk("d-claude", "m", ResultOK, 730_000, 2_000, 98_000, 0, base.Add(3*time.Minute)),
		mk("d-err", "m", ResultError, 700_000, 100_000, 0, 0, base.Add(4*time.Minute)), // 失败行剔除
		mk("d-nolen", "m", ResultOK, 0, 90_000, 0, 0, base.Add(5*time.Minute)),         // 无 body_len 剔除
		mk("d-noctx", "m", ResultOK, 500_000, 0, 0, 0, base.Add(6*time.Minute)),        // 上下文全零剔除
		mk("d-short", "m", ResultOK, 512, 80, 0, 0, base.Add(7*time.Minute)),           // 短体噪声剔除
		mk("d-3", "m", ResultOK, 210_000, 30_000, 0, 0, base.Add(8*time.Minute)),       // 7000
	}
	for _, r := range rows {
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{r.Model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecentDensities(ctx, "m", 100, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// d-claude 的缓存读补进分母后与 d-1 同为 7300，有效样本 4 条。
	if len(got) != 4 {
		t.Fatalf("应只返回 4 条有效密度样本，得到 %d: %v", len(got), got)
	}
	// 新样本在前（时间倒序）：d-3(7000) → d-claude(7300) → d-2(8000) → d-1(7300)。
	for i, want := range []int64{7000, 7300, 8000, 7300} {
		if got[i] != want {
			t.Fatalf("密度样本[%d] = %d want %d（应新样本在前）", i, got[i], want)
		}
	}
}

// TestRecentDensitiesMirrorNotDoubled 锁生产实锤的口径缺陷修复：宿主回填会把
// OpenAI inclusive 行的 cache_read_tokens 填成 cached_tokens 的镜像值，而 input
// 本身已含缓存命中——旧口径在 SQL 端拼 input+cache_read 让分母翻倍、密度学成
// 一半（实测 glm-5.3 分母 1.989×、deepseek-v4-flash 1.923×，预占虚高一倍、
// 「最近预占」实际占比恒 52%）。改为读落库时归一的 context_tokens 后，镜像行
// 与干净 inclusive 行得到同一密度。
func TestRecentDensitiesMirrorNotDoubled(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "density-mirror")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	// 生产行形状：input 225177、cached=cache_read=224896（镜像）、total≈input+output。
	mirror := Request{ID: "mirror-1", TS: base.Add(time.Minute), Model: "m", Result: ResultOK,
		BodyLen: 2_043_000, InputTokens: 225_177, CachedTokens: 224_896,
		CacheReadTokens: 224_896, TotalTokens: 225_401}
	// 干净 inclusive 行：宿主没回填镜像（cache_read 列为 0），同密度。
	clean := Request{ID: "clean-1", TS: base.Add(2 * time.Minute), Model: "m", Result: ResultOK,
		BodyLen: 2_043_000, InputTokens: 225_177, CachedTokens: 224_896, TotalTokens: 225_401}
	for _, r := range []Request{mirror, clean} {
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{"m"}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecentDensities(ctx, "m", 100, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 分母都是 225177（不是 450073）：2043000×1000÷225177 = 9072。
	if len(got) != 2 {
		t.Fatalf("应返回 2 条样本，得到 %d: %v", len(got), got)
	}
	want := int64(2_043_000 * 1000 / 225_177)
	for i, v := range got {
		if v != want {
			t.Fatalf("密度样本[%d] = %d want %d（镜像行分母被双计会得到 %d）",
				i, v, want, 2_043_000*1000/(225_177+224_896))
		}
	}
	// 缓存份额同源：镜像行的分子取 MAX(cached, cache_read)=224896、分母 225177，
	// 读份额接近 100%；旧口径分母翻倍会算成约 50%。
	cs, ok, err := s.RecentCacheShares(ctx, "m", 100, time.Time{})
	if err != nil || !ok {
		t.Fatalf("应有份额样本: ok=%v err=%v", ok, err)
	}
	if wantBP := int64(224_896 * 2 * 10000 / (225_177 * 2)); cs.ReadBP != wantBP {
		t.Fatalf("缓存读份额异常: got %d want %d（双计分母会得到约一半）", cs.ReadBP, wantBP)
	}
}

// TestEstimateDensityDrift 锁漂移适应：全窗 7300 毫密度的模型在 compact
// 后（近期 ~10400 毫密度，实测占比 100%→50% 场景的反推值），
// 全窗中位数要新样本过半才能拉动；近期窗口中位数漂出带即改跟，
// 10 条新流量先部分恢复（窗口半新半旧）、20 条完全接管。
func TestEstimateDensityDrift(t *testing.T) {
	// 30 条旧构成 7300 + 10 条新构成 10430（新样本在前）。
	var samples []int64
	for i := 0; i < 10; i++ {
		samples = append(samples, 10430)
	}
	for i := 0; i < 30; i++ {
		samples = append(samples, 7300)
	}
	est, ok := EstimateDensity(samples)
	if !ok {
		t.Fatal("样本量足够应可用")
	}
	if !est.Drifted {
		t.Fatalf("近期密度明显漂出全窗带应判定漂移: %+v", est)
	}
	// 近期窗口 20 条 = 10 新 + 10 旧 → 中位数 (7300+10430)/2 = 8865，
	// 比全窗 7300 已向新构成移近一半（对应用户实测 70% → ~85% 占比）。
	if est.Milli != 8865 {
		t.Fatalf("漂移后应改跟近期窗口中位数: got %d want 8865", est.Milli)
	}
	if est.Samples != 40 {
		t.Fatalf("样本数应为全窗 40: %d", est.Samples)
	}

	// 20 条新流量（窗口完全换血）→ 完全接管新密度。
	samples = samples[:0]
	for i := 0; i < 20; i++ {
		samples = append(samples, 10430)
	}
	for i := 0; i < 20; i++ {
		samples = append(samples, 7300)
	}
	est, ok = EstimateDensity(samples)
	if !ok || !est.Drifted || est.Milli != 10430 {
		t.Fatalf("窗口完全换血应完全接管: %+v ok=%v", est, ok)
	}

	// 未漂移：近期窗口与全窗一致 → 全窗中位数、不标漂移。
	samples = nil
	for i := 0; i < 20; i++ {
		samples = append(samples, 7300)
	}
	for i := 0; i < 20; i++ {
		samples = append(samples, 7300)
	}
	est, ok = EstimateDensity(samples)
	if !ok || est.Drifted || est.Milli != 7300 {
		t.Fatalf("一致样本不应漂移: %+v ok=%v", est, ok)
	}

	// 漂移幅度在带内（15%）→ 不切换：近期 8000 vs 全窗 7300 = +9.6%。
	samples = nil
	for i := 0; i < 20; i++ {
		samples = append(samples, 8000)
	}
	for i := 0; i < 20; i++ {
		samples = append(samples, 7300)
	}
	est, ok = EstimateDensity(samples)
	if !ok || est.Drifted {
		t.Fatalf("带内偏移不应判漂移: %+v ok=%v", est, ok)
	}

	// 样本不足窗口（n <= driftWindow）没有「基准 vs 近期」区分度，
	// 直接全窗中位数、不标漂移。
	est, ok = EstimateDensity([]int64{10430, 10430, 7300, 7300, 7300, 7300, 7300, 7300, 7300, 7300})
	if !ok || est.Drifted {
		t.Fatalf("n<=driftWindow 不做漂移探测: %+v ok=%v", est, ok)
	}

	// 偶发异质请求不触发误切换：近期窗口 20 条里只有 5 条漂移值，
	// 中位数仍是 7300。
	samples = nil
	for i := 0; i < 5; i++ {
		samples = append(samples, 15000)
	}
	for i := 0; i < 15; i++ {
		samples = append(samples, 7300)
	}
	for i := 0; i < 20; i++ {
		samples = append(samples, 7300)
	}
	est, ok = EstimateDensity(samples)
	if !ok || est.Drifted {
		t.Fatalf("少数异质请求不应触发漂移（近期中位数仍是 7300）: %+v ok=%v", est, ok)
	}

	// 样本 <3 条不可用。
	if _, ok := EstimateDensity([]int64{7300, 7300}); ok {
		t.Fatal("样本 <3 应不可用")
	}
}

// TestRecentCacheShares 锁缓存份额学习的数据源查询：分子取
// MAX(cache_read_tokens, cached_tokens) 归一两种口径（OpenAI 记 cached 含于
// input、cache_read 列为 0；Claude 记 cache_read、input 不含），分母为完整
// 上下文 input+cache_read+cache_creation，token 加权合计（大请求权重更大），
// 只取成功且上下文>0 的行。
func TestRecentCacheShares(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "recent-shares")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	mk := func(id, model, result string, in, cached, cacheR, cacheW int64, at time.Time) Request {
		return Request{ID: id, TS: at, Model: model, Result: result,
			InputTokens: in, CachedTokens: cached, CacheReadTokens: cacheR,
			CacheCreationTokens: cacheW, TotalTokens: in + cacheR + cacheW}
	}
	rows := []Request{
		// Claude 口径：input 不含缓存。读 8000、写 2000、新鲜 2000 → 上下文 12000。
		mk("s-claude", "m", ResultOK, 2000, 0, 8000, 2000, base.Add(1*time.Minute)),
		// OpenAI 口径：input 已含缓存命中 3000（cached 列）、cache_read 列为 0
		// → 上下文 5000、分子 3000。
		mk("s-openai", "m", ResultOK, 5000, 3000, 0, 0, base.Add(2*time.Minute)),
		// 失败行剔除（上下文再大也不进样本）。
		mk("s-err", "m", ResultError, 100000, 90000, 0, 0, base.Add(3*time.Minute)),
		// 其他模型剔除。
		mk("s-other", "m2", ResultOK, 1000, 0, 900, 0, base.Add(4*time.Minute)),
		// 零上下文剔除。
		mk("s-zero", "m", ResultOK, 0, 0, 0, 0, base.Add(5*time.Minute)),
	}
	for _, r := range rows {
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{r.Model}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}

	cs, ok, err := s.RecentCacheShares(ctx, "m", 100, time.Time{})
	if err != nil || !ok {
		t.Fatalf("模型 m 应有份额样本: ok=%v err=%v", ok, err)
	}
	// 分子合计 = Claude 读 8000 + OpenAI 命中 3000 = 11000；写 2000；
	// 分母 = 12000 + 5000 = 17000。
	// 读份额 = 11000/17000 ≈ 64.7% → 6470 万分比；写份额 = 2000/17000 ≈ 11.8% → 1176。
	if want := int64(11000 * 10000 / 17000); cs.ReadBP != want {
		t.Fatalf("缓存读份额异常: got %d want %d", cs.ReadBP, want)
	}
	if want := int64(2000 * 10000 / 17000); cs.CreateBP != want {
		t.Fatalf("缓存写份额异常: got %d want %d", cs.CreateBP, want)
	}
	if cs.Samples != 2 || cs.ContextTx != 17000 {
		t.Fatalf("样本规模异常: %+v", cs)
	}

	// 无样本模型返回 ok=false。
	if _, ok, err := s.RecentCacheShares(ctx, "none", 100, time.Time{}); err != nil || ok {
		t.Fatalf("无样本模型应 ok=false: ok=%v err=%v", ok, err)
	}
	// 全失败模型同样 ok=false。
	if _, ok, err := s.RecentCacheShares(ctx, "m-err", 100, time.Time{}); err != nil || ok {
		t.Fatalf("全失败行模型应 ok=false: ok=%v err=%v", ok, err)
	}
}

// TestRecentCacheSharesWeighting 锁 token 加权语义：份额按 token 合计
// 而非逐条中位数——两条读占比 90% 的小请求 + 一条读占比 10% 的大请求，
// token 加权后整体读份额被大请求拉低（中位数口径会给 90%）。
func TestRecentCacheSharesWeighting(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "recent-shares-w")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	rows := []Request{
		// 两条 1000 token 上下文、读 900（90%）。
		{ID: "w-1", TS: base, Model: "m", Result: ResultOK, InputTokens: 1000, CacheReadTokens: 900, TotalTokens: 1900},
		{ID: "w-2", TS: base.Add(time.Minute), Model: "m", Result: ResultOK, InputTokens: 1000, CacheReadTokens: 900, TotalTokens: 1900},
		// 一条 100000 token 上下文、读 10000（10%）。
		{ID: "w-3", TS: base.Add(2 * time.Minute), Model: "m", Result: ResultOK, InputTokens: 90000, CacheReadTokens: 10000, TotalTokens: 100000},
	}
	for _, r := range rows {
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{"m"}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	cs, ok, err := s.RecentCacheShares(ctx, "m", 100, time.Time{})
	if err != nil || !ok {
		t.Fatalf("应有样本: ok=%v err=%v", ok, err)
	}
	// 加权：读 (900+900+10000) / 上下文 (1900+1900+100000) ≈ 12.06%。
	if want := int64(11800 * 10000 / 103800); cs.ReadBP != want {
		t.Fatalf("token 加权份额异常: got %d want %d（中位数口径会是 9000）", cs.ReadBP, want)
	}
	if want := int64(11800 * 10000 / 103800); want > 2000 {
		// 计算意图保底：加权结果应明显低于 90% 的中位数口径。
		t.Fatalf("加权份额应被大请求拉低: %d", cs.ReadBP)
	}
}

// TestModelDensities 锁密度读数接口：每模型返回中位数/MAD/样本数，
// 只列有样本的模型，按模型名排序。
func TestModelDensities(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "model-densities")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	// 模型 a：8 条一致的 7300 毫密度 → 中位 7300、MAD 0；其中 4 条带
	// 缓存读 50000（body_len 同步放大保持密度一致），份额读数应随之出现。
	for i := 0; i < 8; i++ {
		cacheR, bodyLen := int64(0), int64(730_000)
		if i < 4 {
			cacheR, bodyLen = 50_000, 1_095_000 // 上下文 150K，密度同为 7300
		}
		r := Request{ID: "a-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: "a", Result: ResultOK, BodyLen: bodyLen, InputTokens: 100_000,
			CacheReadTokens: cacheR, TotalTokens: 100_000 + cacheR}
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{"a"}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	// 模型 b：3 条 4000 毫密度（低密度模型）。
	for i := 0; i < 3; i++ {
		r := Request{ID: "b-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: "b", Result: ResultOK, BodyLen: 400_000, InputTokens: 100_000, TotalTokens: 100_000}
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{"b"}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	// 模型 c：只有失败行，不出现。

	got, err := s.ModelDensities(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Model != "a" || got[1].Model != "b" {
		t.Fatalf("应返回 a、b 两个模型（按名排序）: %+v", got)
	}
	if got[0].MilliDensity != 7300 || got[0].MilliMAD != 0 || got[0].Samples != 8 {
		t.Fatalf("模型 a 读数异常: %+v", got[0])
	}
	// 模型 a 缓存份额：4 条带读 50000、上下文 150000，4 条无缓存、上下文
	// 100000 → 读合计 200000 / 上下文合计 1000000 = 20%。
	if got[0].CacheReadBP != 2000 || got[0].CacheCreateBP != 0 {
		t.Fatalf("模型 a 缓存份额读数异常: %+v", got[0])
	}
	// 模型 b 无缓存流量：份额字段为 0。
	if got[1].CacheReadBP != 0 || got[1].CacheCreateBP != 0 {
		t.Fatalf("模型 b 缓存份额应为 0: %+v", got[1])
	}
	if got[1].MilliDensity != 4000 || got[1].Samples != 3 {
		t.Fatalf("模型 b 读数异常: %+v", got[1])
	}
}

// TestDensityEpochFilter 锁学习基线：基线（面板「重置」写入）之前的样本
// 不再参与密度与缓存构成学习；基线之后不足 3 条时不可用（回退固定估算）。
func TestDensityEpochFilter(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "density-epoch")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	// 三条旧构成样本（7300 毫密度）+ 一条旧缓存构成。
	for i := 0; i < 3; i++ {
		r := Request{ID: "old-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: "m", Result: ResultOK, BodyLen: 730_000, InputTokens: 100_000,
			CacheReadTokens: 50_000, TotalTokens: 150_000}
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{"m"}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	// 重置基线：设在旧样本之后、新样本之前。
	cut := base.Add(10 * time.Minute)
	if err := s.SetDensityEpoch(ctx, "m", cut); err != nil {
		t.Fatal(err)
	}
	// 基线拉到之后：旧样本被排除，读数不可用。
	if samples, err := s.RecentDensities(ctx, "m", 100, cut); err != nil || len(samples) != 0 {
		t.Fatalf("基线后无样本应返回空: %v %v", samples, err)
	}
	if _, ok, err := s.RecentCacheShares(ctx, "m", 100, cut); err != nil || ok {
		t.Fatalf("基线后无样本份额应 ok=false: ok=%v err=%v", ok, err)
	}
	// 零值基线退化为全部样本（未重置路径）。
	if samples, err := s.RecentDensities(ctx, "m", 100, time.Time{}); err != nil || len(samples) != 3 {
		t.Fatalf("零值基线应取全部样本: %v %v", samples, err)
	}

	// 新构成样本（10430 毫密度：同体量请求真实 token 少三成）落基线之后。
	for i := 0; i < 3; i++ {
		r := Request{ID: "new-" + time.Duration(i).String(), TS: base.Add(time.Duration(20+i) * time.Minute),
			Model: "m", Result: ResultOK, BodyLen: 730_000, InputTokens: 70_000, TotalTokens: 70_000}
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{"m"}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	samples, err := s.RecentDensities(ctx, "m", 100, cut)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("基线后应有 3 条新样本，得到 %d: %v", len(samples), samples)
	}
	est, ok := EstimateDensity(samples)
	if !ok || est.Milli < 10_000 {
		t.Fatalf("基线后应按新构成学习（10430 附近）: %+v ok=%v", est, ok)
	}

	// 读数行：重置过的模型即使样本不足也要留在列表（面板显示待学习态）。
	if err := s.SetDensityEpoch(ctx, "fresh-model", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ModelDensities(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	var found *ModelDensity
	for i := range rows {
		if rows[i].Model == "fresh-model" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("有基线的模型应保留在读数列表: %+v", rows)
	}
	if found.Samples != 0 || found.ResetAt == nil {
		t.Fatalf("无新样本的重置行应显示 Samples=0 且带 ResetAt: %+v", *found)
	}
}
