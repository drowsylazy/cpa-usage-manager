package routelang

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, src string) *Program {
	t.Helper()
	p, err := Compile(src)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	return p
}

func TestCompileRefsAndUsesAI(t *testing.T) {
	p := mustCompile(t, `
when input_tokens <= 8000
  -> weighted { "gpt-4o-mini": 3, "deepseek-chat": 1 }
when ai_judge(["simple", "hard"]) == "hard"
  -> priority ["claude-opus-4", "gemini-2.5-pro"]
-> "claude-sonnet-4"
`)
	if !p.UsesAI() {
		t.Fatal("应检测到 ai_judge")
	}
	got := strings.Join(p.ReferencedModels(), ",")
	want := "gpt-4o-mini,deepseek-chat,claude-opus-4,gemini-2.5-pro,claude-sonnet-4"
	if got != want {
		t.Fatalf("引用集 = %q, 期望 %q", got, want)
	}

	p2 := mustCompile(t, "-> \"a\"\n")
	if p2.UsesAI() {
		t.Fatal("无 ai_judge 不应报 UsesAI")
	}
	if strings.Join(p2.ReferencedModels(), ",") != "a" {
		t.Fatalf("单目标引用集异常: %v", p2.ReferencedModels())
	}
}

func TestCompileErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // 错误信息子串
	}{
		{"缺兜底", "when x == 1\n  -> \"a\"", "缺少无条件兜底分支"},
		{"兜底后还有分支", "-> \"a\"\nwhen x == 1\n  -> \"b\"", "最后一条"},
		{"未知函数", "when foo(1) == 1\n  -> \"a\"\n-> \"b\"", "未知函数"},
		{"权重非正", "when input_tokens < 1\n  -> weighted {\"a\":0}\n-> \"b\"", "正数"},
		{"weighted 重复模型", "when input_tokens < 1\n  -> weighted {\"a\":1,\"A\":2}\n-> \"b\"", "重复"},
		{"priority 空", "when input_tokens < 1\n  -> priority []\n-> \"b\"", "不能为空"},
		{"条件缺箭头", "when input_tokens < 1\n  \"a\"\n-> \"b\"", "->"},
		{"非法字符", "when input_tokens < 1 ; -> \"a\"", "无法识别的字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.src)
			if err == nil {
				t.Fatalf("应编译失败")
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("应为 SyntaxError: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 不含 %q", err.Error(), tc.want)
			}
			if se.Line <= 0 || se.Col <= 0 {
				t.Fatalf("行列号缺失: %+v", se)
			}
		})
	}
}

func evalOf(t *testing.T, src string, env *Env) ([]string, bool, error) {
	t.Helper()
	ctx := context.Background()
	if env == nil {
		env = &Env{Vars: map[string]any{}}
	}
	return mustCompile(t, src).Eval(ctx, env)
}

func TestEvalBranchesAndShortCircuit(t *testing.T) {
	src := `
when input_tokens <= 8000
  -> priority ["small-a", "small-b"]
when input_tokens <= 20000 && ai_judge(["easy", "complex"]) == "complex"
  -> "big-a"
-> "default-m"
`
	aiCalls := 0
	judge := func(_ context.Context, options []string) (string, error) {
		aiCalls++
		return options[1], nil
	}
	vars := func(n int64) map[string]any {
		return map[string]any{"input_tokens": n, "model": "m", "stream": false,
			"body_len": n * 2, "thinking_effort": "", "source": "openai"}
	}

	// 命中第一条：不应触发 judge（&& 短路）。
	chain, fb, err := evalOf(t, src, &Env{Vars: vars(100), AI: judge})
	if err != nil || fb || chain[0] != "small-a" || chain[1] != "small-b" {
		t.Fatalf("首分支异常: chain=%v fb=%v err=%v", chain, fb, err)
	}
	if aiCalls != 0 {
		t.Fatalf("短路与不应调用 judge，实际 %d 次", aiCalls)
	}
	// 第二条：judge 返回 complex。
	chain, fb, err = evalOf(t, src, &Env{Vars: vars(10000), AI: judge})
	if err != nil || fb || len(chain) != 1 || chain[0] != "big-a" {
		t.Fatalf("次分支异常: chain=%v fb=%v err=%v", chain, fb, err)
	}
	if aiCalls != 1 {
		t.Fatalf("judge 应被调用一次，实际 %d", aiCalls)
	}
	// 兜底。
	chain, fb, err = evalOf(t, src, &Env{Vars: vars(999999), AI: judge})
	if err != nil || fb || chain[0] != "default-m" {
		t.Fatalf("兜底异常: chain=%v fb=%v err=%v", chain, fb, err)
	}
	if aiCalls != 1 {
		t.Fatalf("超阈值但短路左侧为假，不应再调 judge: %d", aiCalls)
	}
}

func TestEvalAIFallbackToDefault(t *testing.T) {
	src := `
when ai_judge(["simple", "hard"]) == "hard"
  -> "hard-m"
-> "safe-m"
`
	calls := 0
	fail := func(_ context.Context, _ []string) (string, error) {
		calls++
		return "", errors.New("上游超时")
	}
	chain, fb, err := evalOf(t, src, &Env{Vars: map[string]any{"input_tokens": int64(1)}, AI: fail})
	if err == nil || !fb {
		t.Fatalf("应返回 AIFallbackError 且 fellBack=true: err=%v fb=%v", err, fb)
	}
	var aife *AIFallbackError
	if !errors.As(err, &aife) {
		t.Fatalf("错误类型应为 AIFallbackError: %T", err)
	}
	if chain[0] != "safe-m" {
		t.Fatalf("应回落兜底分支: %v", chain)
	}
	if calls != 1 {
		t.Fatalf("judge 调用次数 %d", calls)
	}
	// judge 返回不在选项中的值同样回落。
	bad := func(_ context.Context, _ []string) (string, error) { return "unknown-label", nil }
	_, fb, err = evalOf(t, src, &Env{Vars: map[string]any{}, AI: bad})
	if err == nil || !fb {
		t.Fatalf("越界返回也应回落: err=%v fb=%v", err, fb)
	}
}

func TestEvalWeightedFollowersByWeightDesc(t *testing.T) {
	src := "when input_tokens >= 0\n  -> weighted { \"a\": 1, \"b\": 5, \"c\": 2 }\n-> \"z\"\n"
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		chain, _, err := evalOf(t, src, &Env{Vars: map[string]any{"input_tokens": int64(1)}})
		if err != nil {
			t.Fatal(err)
		}
		if len(chain) != 3 {
			t.Fatalf("链长应含全部成员: %v", chain)
		}
		counts[chain[0]]++
		if chain[0] == "a" && (chain[1] != "b" || chain[2] != "c") {
			t.Fatalf("落选者应按权重降序跟随: %v", chain)
		}
	}
	// 粗检（全局随机源无确定性，容差须容纳统计涨落：a 的 σ≈13/2000）：
	// 只拦截实现性错误——权重被忽略（均分）、倒置或恒选首项。
	// 期望 b(5) > c(2) > a(1)，且 a ≈ b/5。
	if !(counts["b"] > counts["c"] && counts["c"] > counts["a"]) {
		t.Fatalf("加权分布应保持 b>c>a 的序: %v", counts)
	}
	if counts["b"] <= counts["a"]+counts["c"] {
		t.Fatalf("权重 5 应占多数: %v", counts)
	}
	if counts["a"]*2 >= counts["b"] || counts["a"]*20 <= counts["b"] {
		t.Fatalf("加权分布异常偏离 5:1: %v", counts)
	}
}

func TestEvalStringOpsAndErrors(t *testing.T) {
	src := `when model == "gpt-x" && !(thinking_effort != "")
  -> "t1"
-> "t0"
`
	chain, _, err := evalOf(t, src, &Env{Vars: map[string]any{
		"model": "GPT-X", "thinking_effort": "", "input_tokens": int64(0)}})
	if err != nil {
		t.Fatalf("字符串比较区分大小写属预期，此处应走兜底而非报错: %v", err)
	}
	if chain[0] != "t0" {
		t.Fatalf("大小写敏感匹配应不命中: %v", chain)
	}
	chain, _, err = evalOf(t, src, &Env{Vars: map[string]any{
		"model": "gpt-x", "thinking_effort": ""}})
	if err != nil || chain[0] != "t1" {
		t.Fatalf("命中分支异常: %v %v", chain, err)
	}

	// 未知变量 → 求值期错误。
	_, _, err = evalOf(t, "when nope == 1\n  -> \"a\"\n-> \"b\"", &Env{Vars: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "未知变量") {
		t.Fatalf("未知变量应报错: %v", err)
	}
	// 数字与字符串比较 → 类型错误。
	_, _, err = evalOf(t, "when input_tokens == \"x\"\n  -> \"a\"\n-> \"b\"",
		&Env{Vars: map[string]any{"input_tokens": int64(1)}})
	if err == nil || !strings.Contains(err.Error(), "比较") {
		t.Fatalf("跨类型比较应报错: %v", err)
	}
}
