package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestGroupByDimensionSourceRedacted 锁住 source 维度读侧清洗：历史脏行
// （写入收口生效前落库、source 列带着上游 API Key 的 usage_rollups）在
// /usage/dimension?dimension=source 输出前必须清洗，随保留期老化。
func TestGroupByDimensionSourceRedacted(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()

	// 直接种脏 rollup 行，绕过写入收口（模拟历史数据）。
	leaked := "fk-a1b2c3d4e5f6g7h8i9j0"
	ts := time.Date(2026, 8, 20, 10, 3, 0, 0, time.UTC)
	err := st.Write(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx,
			`INSERT INTO usage_rollups (bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result, req_count, input_tokens, output_tokens, total_tokens, cost_micro_usd)
			 VALUES (?,?,?,?,?,?,?,?,?,1,?,?,?,?)`,
			ts.UTC().Unix()/60, "gpt-5", "", "default", "", leaked, "", "", store.ResultOK, 10, 20, 30, 0)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := s.GroupByDimension(ctx, UsageFilter{}, "source", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("应只有 1 个 source 分组，得到 %d", len(rep.Rows))
	}
	if rep.Rows[0].Value != "" {
		t.Fatalf("source 维度分组值应被清洗，得到 %q", rep.Rows[0].Value)
	}
}
