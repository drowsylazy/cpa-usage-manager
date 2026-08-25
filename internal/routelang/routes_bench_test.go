package routelang

// 路由规则编译/求值微基准：量化快照重载时的 Compile 成本与每请求 Eval 成本。
// 运行：go test ./internal/routelang/ -bench BenchmarkRoutelang -benchmem -run '^$'
import (
	"context"
	"testing"
)

const benchRule = `# 分级路由示例
when input_tokens <= 8000 && source == "openai"
  -> weighted { "gpt-4o-mini": 5, "deepseek-chat": 2, "qwen-max": 1 }
when ai_judge(["simple", "hard"]) == "hard"
  -> priority ["claude-opus-4", "gemini-2.5-pro", "gpt-4o"]
-> "claude-sonnet-4"
`

func BenchmarkRoutelangCompileTypical(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Compile(benchRule); err != nil {
			b.Fatal(err)
		}
	}
}

func benchEnv() *Env {
	return &Env{
		Vars: map[string]any{
			"input_tokens":    int64(12000),
			"body_len":        int64(24000),
			"model":           "auto",
			"stream":          true,
			"thinking_effort": "medium",
			"source":          "openai",
		},
	}
}

func BenchmarkRoutelangEvalPriority(b *testing.B) {
	p, err := Compile(`-> priority ["claude-opus-4", "gemini-2.5-pro", "gpt-4o"]`)
	if err != nil {
		b.Fatal(err)
	}
	env := benchEnv()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := p.Eval(ctx, env); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRoutelangEvalWeighted(b *testing.B) {
	p, err := Compile(`when input_tokens <= 8000
  -> weighted { "gpt-4o-mini": 5, "deepseek-chat": 2 }
-> weighted { "claude-opus-4": 1, "gpt-4o": 1 }`)
	if err != nil {
		b.Fatal(err)
	}
	env := benchEnv()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := p.Eval(ctx, env); err != nil {
			b.Fatal(err)
		}
	}
}
