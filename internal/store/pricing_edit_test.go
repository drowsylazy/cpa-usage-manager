package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// 计价规则编辑：带 ID 的 upsert 必须原位更新（键未变）或迁移（键变更），
// 而不是像无 ID 路径那样按 (match_kind, pattern) 另立新条。

func TestUpsertPricingRuleEditKeepsIDWhenKeyUnchanged(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "pricing-edit")
	ctx := context.Background()

	created, err := s.UpsertPricingRule(ctx, PricingRule{
		MatchKind: MatchGlob, Pattern: "gpt-*", Priority: 100, Enabled: true,
		PriceInput: 1_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpsertPricingRule(ctx, PricingRule{
		ID: created.ID, MatchKind: MatchGlob, Pattern: "gpt-*", Priority: 50, Enabled: false,
		PriceInput: 2_500_000, PriceOutput: 7_500_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID {
		t.Fatalf("键未变时编辑应保留 ID：%d → %d", created.ID, updated.ID)
	}
	if updated.Priority != 50 || updated.Enabled || updated.PriceInput != 2_500_000 || updated.PriceOutput != 7_500_000 {
		t.Fatalf("字段未按编辑值更新: %+v", updated)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("键未变时不应改动创建时间: %v → %v", created.CreatedAt, updated.CreatedAt)
	}
	rules, err := s.ListPricingRules(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d, 期望 1（编辑原位更新，不另立新条）", len(rules))
	}
}

func TestUpsertPricingRuleEditMovesKey(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "pricing-move")
	ctx := context.Background()

	created, err := s.UpsertPricingRule(ctx, PricingRule{
		MatchKind: MatchExact, Pattern: "old-model", Priority: 100, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.UpsertPricingRule(ctx, PricingRule{
		ID: created.ID, MatchKind: MatchGlob, Pattern: "new-*", Priority: 100, Enabled: true,
		PriceInput: 3_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPricingRuleByPattern(ctx, MatchExact, "old-model"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧键应已删除，得到 %v", err)
	}
	got, err := s.GetPricingRuleByPattern(ctx, MatchGlob, "new-*")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != moved.ID || got.PriceInput != 3_000_000 {
		t.Fatalf("新键规则不符: %+v", got)
	}
}

func TestUpsertPricingRuleEditMergesIntoExistingKey(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "pricing-merge")
	ctx := context.Background()

	a, err := s.UpsertPricingRule(ctx, PricingRule{
		MatchKind: MatchExact, Pattern: "model-a", Priority: 10, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.UpsertPricingRule(ctx, PricingRule{
		MatchKind: MatchExact, Pattern: "model-b", Priority: 20, Enabled: true,
		PriceInput: 5_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 把 a 改成 b 的键：合并为一条，b 的字段被 a 的编辑值覆盖。
	merged, err := s.UpsertPricingRule(ctx, PricingRule{
		ID: a.ID, MatchKind: MatchExact, Pattern: "model-b", Priority: 30, Enabled: false,
		PriceOutput: 9_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged.ID != b.ID {
		t.Fatalf("合并后应保留已存在键的条目 #%d，得到 #%d", b.ID, merged.ID)
	}
	if merged.Priority != 30 || merged.Enabled || merged.PriceOutput != 9_000_000 {
		t.Fatalf("合并后字段不符: %+v", merged)
	}
	rules, err := s.ListPricingRules(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("合并后规则数 = %d, 期望 1", len(rules))
	}
}

func TestUpsertPricingRuleEditMissingID(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "pricing-missing")
	if _, err := s.UpsertPricingRule(context.Background(), PricingRule{
		ID: 424242, MatchKind: MatchGlob, Pattern: "x*", Priority: 1, Enabled: true,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("编辑不存在的 ID 应返回 ErrNotFound，得到 %v", err)
	}
}
