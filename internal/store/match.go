package store

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// 计价规则的模型匹配。
//
// 三种匹配方式的语义：
//   - exact：模型名完全相等
//   - glob：支持 `*`（任意长度，可跨 `/`）与 `?`（单个字符）
//   - regexp：Go RE2 语法，非锚定（部分匹配即命中）
//
// 三者一律**大小写不敏感**：上游模型名的大小写写法不统一，
// 大小写敏感会导致规则静默失配、进而落到 unknown_policy 分支，
// 宽松匹配在这里比严格匹配安全。

// MatchGlobPattern 判断 s 是否匹配 glob 模式 pattern。
//
// 与 path.Match 不同：`*` 可以跨越 `/`，因为模型名常含斜杠
// （如 anthropic/claude-sonnet-4），要求 `*` 不跨 `/` 会违反直觉。
func MatchGlobPattern(pattern, s string) bool {
	p := strings.ToLower(pattern)
	v := strings.ToLower(s)

	// 双指针 + 回溯，线性时间，不会因病态模式产生指数级回溯。
	var (
		pi, vi     int
		starPi     = -1
		starVi     int
		hasStarPos bool
	)
	for vi < len(v) {
		switch {
		case pi < len(p) && (p[pi] == v[vi] || p[pi] == '?'):
			pi++
			vi++
		case pi < len(p) && p[pi] == '*':
			starPi = pi
			hasStarPos = true
			pi++
			starVi = vi
		case hasStarPos:
			// 回溯：让上一个 `*` 多吞掉一个字符。
			pi = starPi + 1
			starVi++
			vi = starVi
		default:
			return false
		}
	}
	// 模式剩余部分只能是 `*`。
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// regexpCache 缓存已编译的正则，避免每次匹配重复编译。
var regexpCache = struct {
	sync.RWMutex
	m map[string]*regexp.Regexp
}{m: make(map[string]*regexp.Regexp)}

// maxRegexpCache 是正则缓存上限。规则由管理员维护、数量有限，
// 超过上限说明存在异常写入，整体清空即可（重新编译代价可接受）。
const maxRegexpCache = 512

// compileRegexp 编译（并缓存）一条大小写不敏感的正则。
func compileRegexp(pattern string) (*regexp.Regexp, error) {
	regexpCache.RLock()
	re, ok := regexpCache.m[pattern]
	regexpCache.RUnlock()
	if ok {
		return re, nil
	}

	// (?i) 前缀实现大小写不敏感，与 glob/exact 口径一致。
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("正则 %q 编译失败: %w", pattern, err)
	}

	regexpCache.Lock()
	if len(regexpCache.m) >= maxRegexpCache {
		regexpCache.m = make(map[string]*regexp.Regexp, maxRegexpCache)
	}
	regexpCache.m[pattern] = re
	regexpCache.Unlock()
	return re, nil
}

// MatchRule 判断 model 是否命中给定的匹配方式与模式。
// 非法正则视为不匹配（写入时已由 PricingRule.Validate 拦截）。
func MatchRule(matchKind, pattern, model string) bool {
	switch matchKind {
	case MatchExact:
		return strings.EqualFold(strings.TrimSpace(pattern), strings.TrimSpace(model))
	case MatchGlob:
		return MatchGlobPattern(pattern, model)
	case MatchRegexp:
		re, err := compileRegexp(pattern)
		if err != nil {
			return false
		}
		return re.MatchString(model)
	default:
		return false
	}
}

// Matches 判断该规则是否命中给定模型。
func (r *PricingRule) Matches(model string) bool {
	if !r.Enabled {
		return false
	}
	return MatchRule(r.MatchKind, r.Pattern, model)
}
