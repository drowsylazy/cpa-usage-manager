package service

import (
	"context"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestMatchPricingCrossKindPriority 钉住 exact 分桶索引下的优先级裁决语义：
// matchPricing 改为 exact O(1) 命中 + glob/regexp 线性扫描后，命中候选必须
// 仍按全局优先序（priority DESC, id ASC）取最前者——高优先级 glob 压过低
// 优先级 exact，反之亦然。
func TestMatchPricingCrossKindPriority(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()

	mk := func(kind, pattern string, priority int, price money.Price) {
		if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
			MatchKind: kind, Pattern: pattern, Priority: priority, Enabled: true,
			PriceInput: price, PriceOutput: price, Source: store.PricingSourceManual,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 场景一：glob(10) 压 exact(1)，同模型两个族都命中。
	mk(store.MatchExact, "n-a", 1, 111)
	mk(store.MatchGlob, "n-*", 10, 222)
	rule, priced, err := s.matchPricing(ctx, "n-a")
	if err != nil || !priced || rule.PriceInput != 222 {
		t.Fatalf("glob(10) 应压过 exact(1): rule=%+v priced=%v err=%v", rule, priced, err)
	}
	// 场景二：exact(10) 压 glob(1)（优先级翻转，模式双向重叠；独立前缀 p-
	// 避开场景一的 n-*）。
	mk(store.MatchExact, "p-b", 10, 333)
	mk(store.MatchGlob, "p-b*", 1, 444)
	if rule, priced, _ = s.matchPricing(ctx, "p-b"); !priced || rule.PriceInput != 333 {
		t.Fatalf("exact(10) 应压过 glob(1): rule=%+v priced=%v", rule, priced)
	}
	// 场景三：无任何命中 → 未计价。
	if rule, priced, _ = s.matchPricing(ctx, "no-such-model"); priced {
		t.Fatalf("无命中规则不应 priced: %+v", rule)
	}
	// 场景四：exact 匹配大小写与空白不敏感（与 MatchRule 的 EqualFold/TrimSpace 同口径）。
	if rule, priced, _ = s.matchPricing(ctx, " P-B "); !priced || rule.PriceInput != 333 {
		t.Fatalf("exact 匹配应大小写/空白不敏感: rule=%+v priced=%v", rule, priced)
	}
}
