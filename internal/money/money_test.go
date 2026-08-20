package money

import (
	"math"
	"testing"
)

func TestParseUSD(t *testing.T) {
	cases := []struct {
		in   string
		want Micro
	}{
		{"0", 0},
		{"1", 1_000_000},
		{"1.5", 1_500_000},
		{"0.000001", 1},
		{"3.000000", 3_000_000},
		{"$2.25", 2_250_000},
		{" 12.345678 ", 12_345_678},
		{"-0.5", -500_000},
		{"+7", 7_000_000},
		{".5", 500_000},
		{"15.", 15_000_000},
	}
	for _, c := range cases {
		got, err := ParseUSD(c.in)
		if err != nil {
			t.Errorf("ParseUSD(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseUSD(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

func TestParseUSDErrors(t *testing.T) {
	bad := []string{"", "  ", "abc", "1.2345678", "1.2.3", "-", "$", "1e6", "0x10", "1 000"}
	for _, s := range bad {
		if _, err := ParseUSD(s); err == nil {
			t.Errorf("ParseUSD(%q) 期望报错，却成功了", s)
		}
	}
}

func TestParseUSDRoundTrip(t *testing.T) {
	vals := []Micro{0, 1, 999_999, 1_000_000, 12_345_678, -1, -1_500_000, math.MaxInt64, math.MinInt64}
	for _, v := range vals {
		s := v.USDString()
		got, err := ParseUSD(s)
		if err != nil {
			t.Fatalf("ParseUSD(%q)（来自 %d）报错: %v", s, v, err)
		}
		if got != v {
			t.Errorf("往返失败: %d → %q → %d", v, s, got)
		}
	}
}

func TestUSDString(t *testing.T) {
	cases := []struct {
		in   Micro
		want string
	}{
		{0, "0.000000"},
		{1, "0.000001"},
		{1_000_000, "1.000000"},
		{1_234_567, "1.234567"},
		{-1_500_000, "-1.500000"},
		{-1, "-0.000001"},
	}
	for _, c := range cases {
		if got := c.in.USDString(); got != c.want {
			t.Errorf("Micro(%d).USDString() = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestString(t *testing.T) {
	cases := []struct {
		in   Micro
		want string
	}{
		{0, "$0"},
		{1_000_000, "$1"},
		{1_500_000, "$1.5"},
		{1, "$0.000001"},
		{-2_250_000, "$-2.25"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Micro(%d).String() = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestCostForTokens(t *testing.T) {
	// $3 / 1M tokens。
	p := Price(3_000_000)
	cases := []struct {
		tokens int64
		price  Price
		want   Micro
	}{
		{0, p, 0},
		{1_000_000, p, 3_000_000},           // 恰好一百万 token → $3
		{500_000, p, 1_500_000},             // 半百万 → $1.5
		{1, p, 3},                           // 1 token → 3 micro
		{1, Price(1), 1},                    // ceil(1×1/1e6) = 1，向上取整不为 0
		{3, Price(1), 1},                    // ceil(3/1e6) = 1
		{1_000_001, Price(1), 2},            // ceil(1000001/1e6) = 2
		{123, Price(0), 0},                  // 单价 0 → 免费
		{0, Price(0), 0},                    //
		{7, Price(15_000_000), 105},         // 7 × 15 = 105 micro
		{999_999, Price(1_000_000), 999999}, //
	}
	for _, c := range cases {
		got, err := CostForTokens(c.tokens, c.price)
		if err != nil {
			t.Errorf("CostForTokens(%d, %d) 报错: %v", c.tokens, c.price, err)
			continue
		}
		if got != c.want {
			t.Errorf("CostForTokens(%d, %d) = %d, 期望 %d", c.tokens, c.price, got, c.want)
		}
	}
}

func TestCostForTokensCeilNeverLosesSubUnit(t *testing.T) {
	// 任意非零 token 与非零单价，费用必须 ≥ 1 micro（向上取整不得吞掉零头）。
	for _, tok := range []int64{1, 2, 17, 999} {
		for _, pr := range []Price{1, 2, 999} {
			got, err := CostForTokens(tok, pr)
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if got < 1 {
				t.Errorf("CostForTokens(%d, %d) = %d，期望 ≥ 1", tok, pr, got)
			}
		}
	}
}

func TestCostForTokensNegative(t *testing.T) {
	if _, err := CostForTokens(-1, Price(1)); err != ErrNegative {
		t.Errorf("负 token 期望 ErrNegative，得到 %v", err)
	}
	if _, err := CostForTokens(1, Price(-1)); err != ErrNegative {
		t.Errorf("负单价期望 ErrNegative，得到 %v", err)
	}
}

func TestCostForTokensOverflow(t *testing.T) {
	if _, err := CostForTokens(math.MaxInt64, Price(math.MaxInt64)); err != ErrOverflow {
		t.Errorf("期望 ErrOverflow，得到 %v", err)
	}
}

func TestCostForTokensNoOverflowAtRealisticScale(t *testing.T) {
	// 10 亿 token × $1000/1M —— 远超真实用量，仍须精确无溢出。
	got, err := CostForTokens(1_000_000_000, Price(1_000_000_000))
	if err != nil {
		t.Fatalf("真实量级不应溢出: %v", err)
	}
	// 1e9 tokens × 1e9 micro / 1e6 = 1e12 micro = $1,000,000
	if got != 1_000_000_000_000 {
		t.Errorf("得到 %d, 期望 1000000000000", got)
	}
}

func TestSumCeilMatchesPerCategoryRounding(t *testing.T) {
	// 锁定口径：各类别分别向上取整再相加，而不是先合计 token 再取整。
	// 3 类各 1 token、单价 1 micro/1M：每类 ceil → 1，合计 3。
	// 若先合计（3 token）再取整只会得到 1，差异必须体现。
	var parts []Micro
	for range 3 {
		c, err := CostForTokens(1, Price(1))
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, c)
	}
	total, err := SumCeil(parts...)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("逐类别取整合计 = %d, 期望 3", total)
	}
	naive, err := CostForTokens(3, Price(1))
	if err != nil {
		t.Fatal(err)
	}
	if naive != 1 {
		t.Errorf("合并后取整 = %d, 期望 1（用于对照）", naive)
	}
}

func TestAddSub(t *testing.T) {
	a, b := Micro(1_500_000), Micro(2_250_000)
	if s, err := a.Add(b); err != nil || s != 3_750_000 {
		t.Errorf("Add = %d, %v", s, err)
	}
	if d, err := a.Sub(b); err != nil || d != -750_000 {
		t.Errorf("Sub = %d, %v", d, err)
	}
	if _, err := Micro(math.MaxInt64).Add(1); err != ErrOverflow {
		t.Errorf("正溢出期望 ErrOverflow，得到 %v", err)
	}
	if _, err := Micro(math.MinInt64).Add(-1); err != ErrOverflow {
		t.Errorf("负溢出期望 ErrOverflow，得到 %v", err)
	}
	if _, err := Micro(0).Sub(math.MinInt64); err != ErrOverflow {
		t.Errorf("减 MinInt64 期望 ErrOverflow，得到 %v", err)
	}
}

func TestAddSat(t *testing.T) {
	if got := Micro(math.MaxInt64).AddSat(1000); got != math.MaxInt64 {
		t.Errorf("AddSat 正向饱和 = %d", got)
	}
	if got := Micro(math.MinInt64).AddSat(-1000); got != math.MinInt64 {
		t.Errorf("AddSat 负向饱和 = %d", got)
	}
	if got := Micro(5).AddSat(6); got != 11 {
		t.Errorf("AddSat 正常路径 = %d", got)
	}
}

func TestFromUSD(t *testing.T) {
	if m, err := FromUSD(12); err != nil || m != 12_000_000 {
		t.Errorf("FromUSD(12) = %d, %v", m, err)
	}
	if _, err := FromUSD(math.MaxInt64); err != ErrOverflow {
		t.Errorf("期望 ErrOverflow，得到 %v", err)
	}
}

func TestPriceFromUSDPerMillion(t *testing.T) {
	p, err := PriceFromUSDPerMillion("3.00")
	if err != nil || p != 3_000_000 {
		t.Errorf("得到 %d, %v", p, err)
	}
	if _, err := PriceFromUSDPerMillion("-1"); err != ErrNegative {
		t.Errorf("负单价期望 ErrNegative，得到 %v", err)
	}
	if got := Price(3_000_000).USDPerMillionString(); got != "3.000000" {
		t.Errorf("USDPerMillionString = %q", got)
	}
}

func TestNoFloatInHotPath(t *testing.T) {
	// 回归守卫：典型 Claude 定价（输入 $3、输出 $15、缓存写 $3.75、缓存读 $0.30 / 1M）
	// 在整数路径下必须得到精确值，浮点实现会在此类分数上产生尾差。
	in, _ := CostForTokens(1_234_567, 3_000_000)
	out, _ := CostForTokens(89_012, 15_000_000)
	cw, _ := CostForTokens(456_789, 3_750_000)
	cr, _ := CostForTokens(2_345_678, 300_000)
	total, err := SumCeil(in, out, cw, cr)
	if err != nil {
		t.Fatal(err)
	}
	want := Micro(3_703_701 + 1_335_180 + 1_712_959 + 703_704)
	if total != want {
		t.Errorf("合计 = %d (%s), 期望 %d", total, total.USDString(), want)
	}
}
