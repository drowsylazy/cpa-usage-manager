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
