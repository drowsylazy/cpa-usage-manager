// Package routelang 实现模型路由的规则脚本语言。
//
// 语言形态（从上到下第一条命中生效）：
//
//	# 注释
//	when input_tokens <= 8000
//	  -> weighted { "gpt-4o-mini": 3, "deepseek-chat": 1 }   # 加权随机：选中者排首，其余按权重降序跟随
//	when ai_judge(["simple", "hard"]) == "hard"
//	  -> priority ["claude-opus-4", "gemini-2.5-pro"]         # 声明序即回退链
//	-> "claude-sonnet-4"                                      # 无条件兜底；必填且只能是最后一条
//
// 设计约束：无循环、无赋值、无用户函数定义——求值天然终止。
// 函数调用语法位保留给内置（本期仅 ai_judge），未知函数在编译期报错，
// 后续加入新内置不会破坏已有脚本。
package routelang

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
)

// Program 是一次编译通过的路由规则。
type Program struct {
	root   astProg
	usesAI bool
	refs   []string
}

// SyntaxError 是编译期错误，带行列号供面板定位。
type SyntaxError struct {
	Msg  string
	Line int // 1 起
	Col  int // 1 起
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("第 %d 行 %d 列: %s", e.Line, e.Col, e.Msg)
}

// Compile 解析并静态校验规则脚本。
// 校验包含：语法合法、存在无条件兜底分支且位于末尾、候选链构造器合法、
// 未知函数报错、weighted 权重为正数。
func Compile(src string) (*Program, error) {
	p, err := parseProgram(src)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ReferencedModels 返回规则引用的全部目标模型名（去重、保序副本）。
// 静态提取自候选链位置，ai_judge 的选项标签不属于目标模型。
func (p *Program) ReferencedModels() []string {
	out := make([]string, len(p.refs))
	copy(out, p.refs)
	return out
}

// UsesAI 报告规则是否调用了 ai_judge（保存期据此要求先配置 judge 模型）。
func (p *Program) UsesAI() bool { return p.usesAI }

// Env 是一次求值的运行时环境。
type Env struct {
	// Vars 是开放给脚本的变量集：
	// input_tokens/body_len(int64)、model/thinking_effort/source(string)、stream(bool)。
	Vars map[string]any
	// Rand 用于 weighted 加权随机；nil 时使用包级随机源。
	Rand *rand.Rand
	// AI 是 ai_judge 的宿主实现（service 层注入 judge 调用）；nil 时调用即运行期错误。
	AI func(ctx context.Context, options []string) (string, error)
}

// AIFallbackError 表示 ai_judge 执行失败且已自动回落到无条件兜底分支。
// 调用方据此写 route.ai_fallback 审计；链本身可用。
type AIFallbackError struct {
	Err error
}

func (e *AIFallbackError) Error() string {
	return "ai_judge 求值失败，已回落兜底分支: " + e.Err.Error()
}

func (e *AIFallbackError) Unwrap() error { return e.Err }

// Eval 求值路由规则，返回有序候选链。
// fellBack 为真表示某个 when 条件里的 ai_judge 失败，结果来自兜底分支。
// 除 AIFallbackError 外的错误（未知变量、类型不匹配等）原样返回且无链。
func (p *Program) Eval(ctx context.Context, env *Env) (chain []string, fellBack bool, err error) {
	for _, b := range p.root.branches {
		ok, err := evalCond(ctx, b.cond, env)
		if err != nil {
			var aiErr *AIError
			if errors.As(err, &aiErr) {
				// AI 失败：回落兜底分支（编译期已保证存在）。
				fb, ferr := evalChain(p.root.fallback.chain, env)
				if ferr != nil {
					return nil, false, ferr
				}
				return fb, true, &AIFallbackError{Err: aiErr}
			}
			return nil, false, err
		}
		if ok {
			chain, err2 := evalChain(b.chain, env)
			return chain, false, err2
		}
	}
	fb, ferr := evalChain(p.root.fallback.chain, env)
	return fb, false, ferr
}

// AIError 包装 ai_judge 的底层失败原因。
type AIError struct{ Err error }

func (e *AIError) Error() string { return "ai_judge: " + e.Err.Error() }
func (e *AIError) Unwrap() error { return e.Err }

// normalizeKey 统一模型名比较口径：小写+去首尾空白。与 store 层判重的
// 渠道前缀剥离不同——这里只做别名匹配所需的最小归一。
func normalizeKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
