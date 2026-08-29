package service

import (
	"context"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

// CNY 计价规则：价格四档以 micro-CNY 存储，结算时按保存时锁定的汇率
// 折算成 micro-USD 入账（账本恒为 USD，不随行情漂移）。
func TestCostForRuleCNYConversion(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()

	// ¥2 / 1M input tokens，锁定汇率 7.16。
	rule, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "cn-model", Priority: 10, Enabled: true,
		PriceInput: 2_000_000, Currency: store.PricingCurrencyCNY, RateMilli: 7160,
		Source: store.PricingSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.Currency != store.PricingCurrencyCNY || rule.RateMilli != 7160 {
		t.Fatalf("币种/汇率应原样落库: %+v", rule)
	}

	// 1M input tokens = ¥2 = ceil(2_000_000×1000/7160) = 279330 micro-USD。
	u := usageparse.Usage{InputTokens: 1_000_000}
	cost, priced, err := s.Price("cn-model", u)
	if err != nil || !priced {
		t.Fatalf("计价失败: cost=%v priced=%v err=%v", cost, priced, err)
	}
	if want := int64(279330); int64(cost) != want {
		t.Fatalf("1M token 应折算 %d micro-USD，得到 %d", want, cost)
	}

	// 小额：1000 token = 2000 micro-CNY → ceil(2000×1000/7160)=280。
	cost, _, err = s.Price("cn-model", usageparse.Usage{InputTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(280); int64(cost) != want {
		t.Fatalf("1000 token 应折算 %d micro-USD，得到 %d", want, cost)
	}

	// 原生币种入账口径：PriceNative 返回 micro-CNY 原生金额与币种。
	_, native, cur, _, err := s.PriceNative("cn-model", usageparse.Usage{InputTokens: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if cur != store.PricingCurrencyCNY || int64(native) != 2_000_000 {
		t.Fatalf("原生入账应为 CNY 2000000 micro，得到 %s %d", cur, native)
	}

	// 汇率越界拒绝。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "bad-rate", Priority: 10, Enabled: true,
		Currency: store.PricingCurrencyCNY, RateMilli: 100, Source: store.PricingSourceManual,
	}); err == nil {
		t.Fatal("CNY 汇率 0.1 应被拒绝")
	}
	// USD 规则带非法币种拒绝。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "bad-cur", Priority: 10, Enabled: true,
		Currency: "JPY", Source: store.PricingSourceManual,
	}); err == nil {
		t.Fatal("JPY 应被拒绝")
	}
}
