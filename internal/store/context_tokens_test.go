package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestDeriveContextTokens 锁 context_tokens 的兜底推导：判据（total 优先、
// 缺失时按形状）正确，且返回值恒为输入侧——早先版本误把带 output 的判据
// 值直接返回，分母被抬高了一个输出量。
func TestDeriveContextTokens(t *testing.T) {
	cases := []struct {
		name string
		r    Request
		want int64
	}{
		{"宿主镜像行（cached==cache_read，total=input+output）",
			Request{InputTokens: 225_177, CachedTokens: 224_896, CacheReadTokens: 224_896,
				OutputTokens: 224, TotalTokens: 225_401}, 225_177},
		{"干净 inclusive 行（只有 cached）",
			Request{InputTokens: 5_000, CachedTokens: 3_000, OutputTokens: 200, TotalTokens: 5_200}, 5_000},
		{"Claude exclusive 行（total 需加缓存读写）",
			Request{InputTokens: 405, CacheReadTokens: 242, CacheCreationTokens: 100,
				OutputTokens: 603, TotalTokens: 1_350}, 747},
		{"total 缺失的 Claude 形状",
			Request{InputTokens: 2_000, CacheReadTokens: 8_000, CacheCreationTokens: 2_000}, 12_000},
		{"total 缺失的 inclusive 形状",
			Request{InputTokens: 5_000, CachedTokens: 3_000}, 5_000},
		{"无输入无缓存归零",
			Request{OutputTokens: 100, TotalTokens: 100}, 0},
	}
	for _, c := range cases {
		if got := deriveContextTokens(c.r); got != c.want {
			t.Fatalf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

// TestMigrationBackfillContextTokens 用迁移 v18 的真实回填语句验证历史行：
// 升级后（不依赖新流量）镜像行与 Claude 行都得到正确的输入侧分母。
func TestMigrationBackfillContextTokens(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "ctx-backfill")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	rows := []Request{
		// 宿主镜像行：分母应为 input（不是 input+cache_read）。
		{ID: "m-mirror", TS: base, Model: "m", Result: ResultOK, BodyLen: 2_043_000,
			InputTokens: 225_177, CachedTokens: 224_896, CacheReadTokens: 224_896,
			OutputTokens: 224, TotalTokens: 225_401},
		// Claude 行：分母应为 input+cache_read+cache_creation。
		{ID: "m-claude", TS: base.Add(time.Minute), Model: "m", Result: ResultOK, BodyLen: 100_000,
			InputTokens: 405, CacheReadTokens: 242, CacheCreationTokens: 100,
			OutputTokens: 603, TotalTokens: 1_350},
	}
	for _, r := range rows {
		if err := s.RecordPassiveUsage(ctx, r, PassiveDedupeHint{Models: []string{"m"}, Near: r.TS}); err != nil {
			t.Fatal(err)
		}
	}
	// 模拟 v17 老库：清空该列后重跑迁移 v18 的回填语句。
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET context_tokens = 0`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, migration18Backfill)
		return err
	}); err != nil {
		t.Fatalf("回填语句执行失败: %v", err)
	}

	got := map[string]int64{}
	for _, id := range []string{"m-mirror", "m-claude"} {
		var v int64
		if err := s.Read(ctx, func(q Querier) error {
			return q.QueryRowContext(ctx, `SELECT context_tokens FROM requests WHERE id = ?`, id).Scan(&v)
		}); err != nil {
			t.Fatal(err)
		}
		got[id] = v
	}
	if got["m-mirror"] != 225_177 {
		t.Fatalf("镜像行回填分母应为 input（225177），得到 %d（双计会是 450073）", got["m-mirror"])
	}
	if got["m-claude"] != 747 {
		t.Fatalf("Claude 行回填分母应为 input+读+写（747），得到 %d", got["m-claude"])
	}
}
