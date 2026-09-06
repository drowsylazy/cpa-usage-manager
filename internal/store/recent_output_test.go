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

	got, err := s.RecentDensities(ctx, "m", 100)
	if err != nil {
		t.Fatal(err)
	}
	// d-claude 的缓存读补进分母后与 d-1 同为 7300，有效样本 4 条。
	if len(got) != 4 {
		t.Fatalf("应只返回 4 条有效密度样本，得到 %d: %v", len(got), got)
	}
	for i, want := range []int64{7000, 7300, 7300, 8000} {
		if got[i] != want {
			t.Fatalf("密度样本[%d] = %d want %d（应升序）", i, got[i], want)
		}
	}
}

// TestModelDensities 锁密度读数接口：每模型返回中位数/MAD/样本数，
// 只列有样本的模型，按模型名排序。
func TestModelDensities(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "model-densities")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	// 模型 a：8 条一致的 7300 毫密度 → 中位 7300、MAD 0。
	for i := 0; i < 8; i++ {
		r := Request{ID: "a-" + time.Duration(i).String(), TS: base.Add(time.Duration(i) * time.Minute),
			Model: "a", Result: ResultOK, BodyLen: 730_000, InputTokens: 100_000, TotalTokens: 100_000}
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
	if got[1].MilliDensity != 4000 || got[1].Samples != 3 {
		t.Fatalf("模型 b 读数异常: %+v", got[1])
	}
}
