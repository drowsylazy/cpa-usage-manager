package service

import (
	"context"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

// CNY 计价规则：价格四档以 micro-CNY 存储（原生入账恒人民币），结算时按
// **当前实时汇率**（svc.ExchangeRate，测试环境无汇率源走兜底 7.20）折算成
// micro-USD 供额度扣减与跨币种聚合；汇率不随规则保存锁定，行情变化即跟随。
func TestCostForRuleCNYConversion(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()

	// ¥2 / 1M input tokens 的 CNY 规则（不再带 fx_rate_milli，保存也不锁定）。
	rule, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "cn-model", Priority: 10, Enabled: true,
		PriceInput: 2_000_000, Currency: store.PricingCurrencyCNY,
		Source: store.PricingSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.Currency != store.PricingCurrencyCNY {
		t.Fatalf("币种应原样落库: %+v", rule)
	}
	if rule.RateMilli != 1000 {
		t.Fatalf("fx_rate_milli 应恒归一为中性值 1000（遗留列不参与计算），得到 %d", rule.RateMilli)
	}

	// 折算必须与 svc.ExchangeRate 的**当前值**一致（测试机可能真连上汇率源
	// 拿到 6.71 之类，也可能走兜底 7.20——只对账口径、不锁具体数字）。
	rate := s.ExchangeRate(ctx).USDToCNY
	if !rate.Valid() {
		t.Fatalf("汇率应落在理性区间，得到 %v", rate)
	}
	u := usageparse.Usage{InputTokens: 1_000_000}
	cost, priced, err := s.Price("cn-model", u)
	if err != nil || !priced {
		t.Fatalf("计价失败: cost=%v priced=%v err=%v", cost, priced, err)
	}
	want := (2_000_000*1_000_000 + int64(rate) - 1) / int64(rate)
	if int64(cost) != want {
		t.Fatalf("1M token 按当前汇率 %v 应折算 %d micro-USD，得到 %d", rate, want, cost)
	}

	// 小额：1000 token = 2000 micro-CNY，同样按当前汇率 ceil 折算。
	cost, _, err = s.Price("cn-model", usageparse.Usage{InputTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	wantSmall := (2000*1_000_000 + int64(rate) - 1) / int64(rate)
	if int64(cost) != wantSmall {
		t.Fatalf("1000 token 应折算 %d micro-USD，得到 %d", wantSmall, cost)
	}

	// 原生币种入账口径：PriceNative 返回 micro-CNY 原生金额与币种，
	// 与折算汇率无关、不随行情变化。
	_, native, cur, _, err := s.PriceNative("cn-model", usageparse.Usage{InputTokens: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if cur != store.PricingCurrencyCNY || int64(native) != 2_000_000 {
		t.Fatalf("原生入账应为 CNY 2000000 micro，得到 %s %d", cur, native)
	}

	// 遗留兼容：旧库带任意 fx_rate_milli 的 CNY 规则重存后被归一为 1000，
	// 折算仍走当前实时汇率（不读规则上的遗留值）。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "legacy-rate", Priority: 10, Enabled: true,
		PriceInput: 1_000_000, Currency: store.PricingCurrencyCNY, RateMilli: 5000,
		Source: store.PricingSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	cost, _, err = s.Price("legacy-rate", usageparse.Usage{InputTokens: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	// ¥1 按当前汇率折算，与遗留 RateMilli=5.0 无关。
	wantLegacy := (1_000_000*1_000_000 + int64(rate) - 1) / int64(rate)
	if int64(cost) != wantLegacy {
		t.Fatalf("遗留锁定值 5.0 不应参与折算，应按当前汇率 %v 得 %d，得到 %d", rate, wantLegacy, cost)
	}

	// USD 规则原样返回，不触发折算。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "us-model", Priority: 10, Enabled: true,
		PriceInput: 1_000_000, Currency: store.PricingCurrencyUSD,
		Source: store.PricingSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	cost, _, err = s.Price("us-model", usageparse.Usage{InputTokens: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if int64(cost) != 1_000_000 {
		t.Fatalf("USD 规则应原样返回 1000000 micro-USD，得到 %d", cost)
	}

	// 非法币种拒绝。
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
		MatchKind: store.MatchExact, Pattern: "bad-cur", Priority: 10, Enabled: true,
		Currency: "JPY", Source: store.PricingSourceManual,
	}); err == nil {
		t.Fatal("JPY 应被拒绝")
	}
}
