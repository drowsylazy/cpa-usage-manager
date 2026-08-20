// Package money 提供整数 micro-USD 算术。
//
// 锁定决策：全流程金额一律为整数 micro-USD，不出现任何浮点运算。
// 价格以「每百万 token 的 micro-USD」表示（PricePerMillion），
// 单类别费用 = ceil(tokens × price / 1e6)，各类别分别向上取整后再相加。
package money

import (
	"errors"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// Micro 是以 micro-USD（1e-6 USD）为单位的金额。可为负（额度允许透支）。
type Micro int64

// Price 是「每百万 token」的单价，单位同样是 micro-USD。
// 例如 $3.00 / 1M tokens 记作 Price(3_000_000)。
type Price int64

const (
	// MicroPerUSD 是 1 USD 含有的 micro-USD 数。
	MicroPerUSD = 1_000_000
	// TokensPerPriceUnit 是价格的计价基数：单价按每百万 token 表示。
	TokensPerPriceUnit = 1_000_000
	// maxFracDigits 是解析/格式化 USD 字符串时的小数位数。
	maxFracDigits = 6
)

var (
	// ErrOverflow 表示运算结果超出 int64 可表示范围。
	ErrOverflow = errors.New("money: 数值溢出")
	// ErrNegative 表示不接受负数的场合收到了负数。
	ErrNegative = errors.New("money: 不允许负数")
	// ErrSyntax 表示金额字符串格式非法。
	ErrSyntax = errors.New("money: 金额格式非法")
)

// Zero 是零金额。
const Zero Micro = 0

// FromUSD 由整数美元构造金额。
func FromUSD(usd int64) (Micro, error) {
	m, ok := mulS(usd, MicroPerUSD)
	if !ok {
		return 0, ErrOverflow
	}
	return Micro(m), nil
}

// Add 返回 m+n，溢出时报错。
func (m Micro) Add(n Micro) (Micro, error) {
	s := m + n
	// 同号相加后符号翻转即溢出。
	if (m > 0 && n > 0 && s < 0) || (m < 0 && n < 0 && s >= 0) {
		return 0, ErrOverflow
	}
	return s, nil
}

// Sub 返回 m-n，溢出时报错。
func (m Micro) Sub(n Micro) (Micro, error) {
	if n == math.MinInt64 {
		return 0, ErrOverflow
	}
	return m.Add(-n)
}

// AddSat 返回 m+n，溢出时饱和到 int64 边界。用于统计求和等不应因溢出中断的场合。
func (m Micro) AddSat(n Micro) Micro {
	s, err := m.Add(n)
	if err == nil {
		return s
	}
	if m > 0 {
		return math.MaxInt64
	}
	return math.MinInt64
}

// IsNegative 报告金额是否为负。
func (m Micro) IsNegative() bool { return m < 0 }

// USDString 以定长 6 位小数渲染金额，例如 Micro(1234567) → "1.234567"。
// 不做浮点转换，纯字符串拼装。
func (m Micro) USDString() string {
	neg := m < 0
	// 取绝对值时对 MinInt64 特殊处理。
	var abs uint64
	if neg {
		abs = uint64(-(m + 1)) + 1
	} else {
		abs = uint64(m)
	}
	whole := abs / MicroPerUSD
	frac := abs % MicroPerUSD
	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	sb.WriteString(strconv.FormatUint(whole, 10))
	sb.WriteByte('.')
	fs := strconv.FormatUint(frac, 10)
	for i := len(fs); i < maxFracDigits; i++ {
		sb.WriteByte('0')
	}
	sb.WriteString(fs)
	return sb.String()
}

// String 返回带 $ 前缀、去掉尾随零的可读形式。
func (m Micro) String() string {
	s := m.USDString()
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		s = "0"
	}
	return "$" + s
}

// ParseUSD 将十进制美元字符串解析为 micro-USD，最多 6 位小数，超出即报错。
// 纯整数解析，不经过 float。
func ParseUSD(s string) (Micro, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, ErrSyntax
	}
	neg := false
	switch t[0] {
	case '+':
		t = t[1:]
	case '-':
		neg = true
		t = t[1:]
	}
	t = strings.TrimPrefix(t, "$")
	if t == "" {
		return 0, ErrSyntax
	}
	intPart, fracPart := t, ""
	if i := strings.IndexByte(t, '.'); i >= 0 {
		intPart, fracPart = t[:i], t[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return 0, ErrSyntax
	}
	if len(fracPart) > maxFracDigits {
		return 0, ErrSyntax
	}
	var whole uint64
	if intPart != "" {
		v, err := strconv.ParseUint(intPart, 10, 64)
		if err != nil {
			return 0, ErrSyntax
		}
		whole = v
	}
	var frac uint64
	if fracPart != "" {
		if strings.IndexFunc(fracPart, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, ErrSyntax
		}
		v, err := strconv.ParseUint(fracPart, 10, 64)
		if err != nil {
			return 0, ErrSyntax
		}
		frac = v
		// 右侧补零到 6 位，把 ".5" 视为 500000 micro。
		for i := len(fracPart); i < maxFracDigits; i++ {
			frac *= 10
		}
	}
	hi, lo := bits.Mul64(whole, MicroPerUSD)
	if hi != 0 {
		return 0, ErrOverflow
	}
	total := lo + frac
	if total < lo {
		return 0, ErrOverflow
	}
	limit := uint64(math.MaxInt64)
	if neg {
		limit = uint64(math.MaxInt64) + 1
	}
	if total > limit {
		return 0, ErrOverflow
	}
	if neg {
		if total == uint64(math.MaxInt64)+1 {
			return math.MinInt64, nil
		}
		return -Micro(total), nil
	}
	return Micro(total), nil
}

// CostForTokens 计算单个 token 类别的费用：ceil(tokens × price / 1e6)。
// tokens 与 price 均须非负；结果向上取整，符合「各类别向上取整后相加」的锁定口径。
func CostForTokens(tokens int64, price Price) (Micro, error) {
	if tokens < 0 || price < 0 {
		return 0, ErrNegative
	}
	if tokens == 0 || price == 0 {
		return 0, nil
	}
	q, ok := mulDivCeilU(uint64(tokens), uint64(price), TokensPerPriceUnit)
	if !ok || q > uint64(math.MaxInt64) {
		return 0, ErrOverflow
	}
	return Micro(q), nil
}

// PriceFromUSDPerMillion 由「每百万 token 多少美元」的十进制字符串构造单价。
func PriceFromUSDPerMillion(s string) (Price, error) {
	m, err := ParseUSD(s)
	if err != nil {
		return 0, err
	}
	if m < 0 {
		return 0, ErrNegative
	}
	return Price(m), nil
}

// USDPerMillionString 渲染单价为每百万 token 的美元字符串。
func (p Price) USDPerMillionString() string { return Micro(p).USDString() }

// SumCeil 依次累加各类别已向上取整的费用，任一步溢出即报错。
func SumCeil(parts ...Micro) (Micro, error) {
	var total Micro
	for _, p := range parts {
		var err error
		total, err = total.Add(p)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

// mulDivCeilU 计算 ceil(a*b/d)，使用 128 位中间结果避免溢出。d 必须非零。
func mulDivCeilU(a, b, d uint64) (uint64, bool) {
	if d == 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(a, b)
	if hi >= d {
		// 商无法用 64 位表示（bits.Div64 会 panic）。
		return 0, false
	}
	q, r := bits.Div64(hi, lo, d)
	if r != 0 {
		if q == math.MaxUint64 {
			return 0, false
		}
		q++
	}
	return q, true
}

// mulS 是带溢出检测的有符号乘法。
func mulS(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	// a==-1 && b==MinInt64 这类情况上面的除法检测无法覆盖，单独判定。
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	return p, true
}
