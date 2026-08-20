package usageparse

import (
	"math"
	"strings"
	"testing"
)

func TestOpenAIChatNonStream(t *testing.T) {
	body := []byte(`{
	  "id": "chatcmpl-123",
	  "object": "chat.completion",
	  "choices": [{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
	  "usage": {
	    "prompt_tokens": 1200,
	    "completion_tokens": 350,
	    "total_tokens": 1550,
	    "prompt_tokens_details": {"cached_tokens": 900, "audio_tokens": 0},
	    "completion_tokens_details": {"reasoning_tokens": 120, "audio_tokens": 0}
	  }
	}`)
	u, ok := ParseJSON(body)
	if !ok {
		t.Fatal("应解析出用量")
	}
	if u.Protocol != ProtocolOpenAIChat {
		t.Errorf("Protocol = %q, 期望 openai_chat", u.Protocol)
	}
	if u.InputTokens != 1200 || u.OutputTokens != 350 || u.TotalTokens != 1550 {
		t.Errorf("原样字段错误: %+v", u)
	}
	if u.CachedTokens != 900 {
		t.Errorf("CachedTokens = %d, 期望 900", u.CachedTokens)
	}
	if u.ReasoningTokens != 120 {
		t.Errorf("ReasoningTokens = %d, 期望 120", u.ReasoningTokens)
	}
	if !u.InputIncludesCache {
		t.Error("OpenAI 口径 prompt_tokens 含缓存，InputIncludesCache 应为 true")
	}
	// 计费口径：输入须扣除缓存命中，避免按输入价重复收费。
	b := u.Billable()
	if b.Input != 300 {
		t.Errorf("Billable.Input = %d, 期望 300（1200-900）", b.Input)
	}
	if b.CacheRead != 900 {
		t.Errorf("Billable.CacheRead = %d, 期望 900", b.CacheRead)
	}
	if b.Output != 350 {
		t.Errorf("Billable.Output = %d, 期望 350（reasoning 已含在内，不重复加）", b.Output)
	}
	if b.CacheCreation != 0 {
		t.Errorf("Billable.CacheCreation = %d, 期望 0", b.CacheCreation)
	}
}

func TestOpenAIResponsesNonStream(t *testing.T) {
	body := []byte(`{
	  "id": "resp_1",
	  "object": "response",
	  "usage": {
	    "input_tokens": 500,
	    "output_tokens": 200,
	    "total_tokens": 700,
	    "input_tokens_details": {"cached_tokens": 128},
	    "output_tokens_details": {"reasoning_tokens": 64}
	  }
	}`)
	u, ok := ParseJSON(body)
	if !ok {
		t.Fatal("应解析出用量")
	}
	if u.Protocol != ProtocolOpenAIResponses {
		t.Errorf("Protocol = %q, 期望 openai_responses", u.Protocol)
	}
	b := u.Billable()
	if b.Input != 372 || b.CacheRead != 128 || b.Output != 200 {
		t.Errorf("Billable = %+v, 期望 Input=372 CacheRead=128 Output=200", b)
	}
}

func TestClaudeNonStream(t *testing.T) {
	body := []byte(`{
	  "id": "msg_1",
	  "type": "message",
	  "role": "assistant",
	  "usage": {
	    "input_tokens": 100,
	    "output_tokens": 250,
	    "cache_creation_input_tokens": 2048,
	    "cache_read_input_tokens": 4096
	  }
	}`)
	u, ok := ParseJSON(body)
	if !ok {
		t.Fatal("应解析出用量")
	}
	if u.Protocol != ProtocolClaude {
		t.Errorf("Protocol = %q, 期望 claude", u.Protocol)
	}
	if u.InputIncludesCache {
		t.Error("Claude 口径 input_tokens 不含缓存，InputIncludesCache 应为 false")
	}
	b := u.Billable()
	// Claude 的输入不含缓存，不得扣减。
	if b.Input != 100 {
		t.Errorf("Billable.Input = %d, 期望 100（不扣减）", b.Input)
	}
	if b.CacheRead != 4096 {
		t.Errorf("Billable.CacheRead = %d, 期望 4096", b.CacheRead)
	}
	if b.CacheCreation != 2048 {
		t.Errorf("Billable.CacheCreation = %d, 期望 2048", b.CacheCreation)
	}
	if b.Output != 250 {
		t.Errorf("Billable.Output = %d, 期望 250", b.Output)
	}
}

func TestClaudeCacheCreationDetailObject(t *testing.T) {
	// 新版 Claude 用 cache_creation 明细对象表示分级 TTL 缓存写入。
	body := []byte(`{"usage":{
	  "input_tokens": 10,
	  "output_tokens": 20,
	  "cache_read_input_tokens": 0,
	  "cache_creation": {"ephemeral_5m_input_tokens": 1000, "ephemeral_1h_input_tokens": 500}
	}}`)
	u, ok := ParseJSON(body)
	if !ok {
		t.Fatal("应解析出用量")
	}
	if u.Protocol != ProtocolClaude {
		t.Errorf("Protocol = %q, 期望 claude", u.Protocol)
	}
	if u.CacheCreationTokens != 1500 {
		t.Errorf("CacheCreationTokens = %d, 期望 1500（明细求和）", u.CacheCreationTokens)
	}
}

func TestClaudeCacheCreationScalarWinsOverDetail(t *testing.T) {
	// 标量与明细同时出现时是冗余表示，必须只计一次。
	body := []byte(`{"usage":{
	  "input_tokens": 10,
	  "output_tokens": 20,
	  "cache_creation_input_tokens": 1500,
	  "cache_creation": {"ephemeral_5m_input_tokens": 1000, "ephemeral_1h_input_tokens": 500}
	}}`)
	u, ok := ParseJSON(body)
	if !ok {
		t.Fatal("应解析出用量")
	}
	if u.CacheCreationTokens != 1500 {
		t.Errorf("CacheCreationTokens = %d, 期望 1500（不得重复计为 3000）", u.CacheCreationTokens)
	}
}

func TestGeminiNonStream(t *testing.T) {
	body := []byte(`{
	  "candidates": [{"content":{"parts":[{"text":"hi"}]}}],
	  "usageMetadata": {
	    "promptTokenCount": 800,
	    "candidatesTokenCount": 150,
	    "cachedContentTokenCount": 600,
	    "thoughtsTokenCount": 90,
	    "totalTokenCount": 1040
	  }
	}`)
	u, ok := ParseJSON(body)
	if !ok {
		t.Fatal("应解析出用量")
	}
	if u.Protocol != ProtocolGemini {
		t.Errorf("Protocol = %q, 期望 gemini", u.Protocol)
	}
	if u.ReasoningInOutput {
		t.Error("Gemini 的 thoughtsTokenCount 与 candidatesTokenCount 并列，ReasoningInOutput 应为 false")
	}
	b := u.Billable()
	if b.Input != 200 {
		t.Errorf("Billable.Input = %d, 期望 200（800-600）", b.Input)
	}
	if b.CacheRead != 600 {
		t.Errorf("Billable.CacheRead = %d, 期望 600", b.CacheRead)
	}
	// 关键差异：Gemini 的思考 token 未计入 candidates，必须并入计费输出。
	if b.Output != 240 {
		t.Errorf("Billable.Output = %d, 期望 240（150+90）", b.Output)
	}
}

func TestGeminiSnakeCaseKeys(t *testing.T) {
	body := []byte(`{"usage_metadata":{
	  "prompt_token_count": 300,
	  "candidates_token_count": 50,
	  "cached_content_token_count": 100,
	  "thoughts_token_count": 25,
	  "total_token_count": 375
	}}`)
	u, ok := ParseJSON(body)
	if !ok {
		t.Fatal("应解析 snake_case 拼写")
	}
	if u.Protocol != ProtocolGemini {
		t.Errorf("Protocol = %q", u.Protocol)
	}
	if u.InputTokens != 300 || u.OutputTokens != 50 || u.CachedTokens != 100 || u.ReasoningTokens != 25 {
		t.Errorf("字段解析错误: %+v", u)
	}
}

func TestNestedUsageContainers(t *testing.T) {
	cases := []struct {
		name string
		body string
		in   int64
		out  int64
	}{
		{
			// Claude 流式首帧把 usage 包在 message 里。
			name: "claude message_start",
			body: `{"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":77,"output_tokens":1,"cache_read_input_tokens":5}}}`,
			in:   77, out: 1,
		},
		{
			// Responses 流式完成帧把 usage 包在 response 里。
			name: "response.completed",
			body: `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":42,"output_tokens":8,"total_tokens":50}}}`,
			in:   42, out: 8,
		},
		{
			name: "深层包裹",
			body: `{"a":{"b":{"c":{"usage":{"prompt_tokens":9,"completion_tokens":3}}}}}`,
			in:   9, out: 3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, ok := ParseJSON([]byte(c.body))
			if !ok {
				t.Fatal("应在嵌套结构中找到 usage")
			}
			if u.InputTokens != c.in || u.OutputTokens != c.out {
				t.Errorf("得到 in=%d out=%d, 期望 in=%d out=%d", u.InputTokens, u.OutputTokens, c.in, c.out)
			}
		})
	}
}

func TestDeepNestingBeyondLimitIsIgnored(t *testing.T) {
	// 超过搜索深度上限的 usage 不再下探（防御恶意深嵌套）。
	body := "{" + strings.Repeat(`"a":{`, 20) + `"usage":{"prompt_tokens":1}` + strings.Repeat("}", 20) + "}"
	if _, ok := ParseJSON([]byte(body)); ok {
		t.Error("超深嵌套应被忽略而非无限下探")
	}
}

func TestNoUsage(t *testing.T) {
	cases := []string{
		`{}`,
		`{"choices":[]}`,
		`{"error":{"message":"bad request"}}`,
		`{"usage":{}}`,
		`{"usage":null}`,
		``,
		`not json`,
		`[1,2,3]`,
	}
	for _, c := range cases {
		if u, ok := ParseJSON([]byte(c)); ok {
			t.Errorf("%q 不应解析出用量，得到 %+v", c, u)
		}
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	big := make([]byte, MaxBodyBytes+1)
	big[0] = '{'
	for i := 1; i < len(big); i++ {
		big[i] = ' '
	}
	if _, ok := ParseJSON(big); ok {
		t.Error("超限载荷应被拒绝")
	}
}

func TestNegativeTokensClampedToZero(t *testing.T) {
	// 上游偶发负值没有业务含义，不得污染费用。
	u, ok := ParseJSON([]byte(`{"usage":{"prompt_tokens":-5,"completion_tokens":-1,"total_tokens":-6}}`))
	if !ok {
		t.Fatal("应解析出用量容器")
	}
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("负值应归零，得到 %+v", u)
	}
	b := u.Billable()
	if b.Input < 0 || b.Output < 0 || b.CacheRead < 0 || b.CacheCreation < 0 {
		t.Errorf("计费口径不得为负: %+v", b)
	}
}

func TestCachedExceedingPromptClampsInsteadOfGoingNegative(t *testing.T) {
	// 上游自相矛盾（cached > prompt）时输入归零，而不是产生负费用。
	u, ok := ParseJSON([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":500}}}`))
	if !ok {
		t.Fatal("应解析出用量")
	}
	b := u.Billable()
	if b.Input != 0 {
		t.Errorf("Billable.Input = %d, 期望 0", b.Input)
	}
	if b.CacheRead != 500 {
		t.Errorf("Billable.CacheRead = %d, 期望 500", b.CacheRead)
	}
}

func TestExplicitZeroIsDistinguishedFromAbsent(t *testing.T) {
	// 显式 0 也算「出现过 usage」，不能被当作未找到。
	u, ok := ParseJSON([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`))
	if !ok {
		t.Fatal("显式 0 的 usage 应被识别")
	}
	if u.Protocol != ProtocolOpenAIChat {
		t.Errorf("Protocol = %q, 期望 openai_chat", u.Protocol)
	}
	if !u.IsZero() {
		t.Errorf("IsZero 应为 true, 得到 %+v", u)
	}
}

func TestSSEPayloads(t *testing.T) {
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\"}\n" +
		"\n" +
		": 这是注释\n" +
		"data: {\"type\":\"ping\"}\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n"
	got := SSEPayloads([]byte(stream))
	if len(got) != 2 {
		t.Fatalf("应提取 2 个载荷（跳过注释与 [DONE]），得到 %d: %q", len(got), got)
	}
	if string(got[0]) != `{"type":"message_start"}` {
		t.Errorf("载荷 0 = %q", got[0])
	}
}

func TestSSEPayloadsMultiLineData(t *testing.T) {
	// SSE 规范：同一事件的多行 data 以换行拼接。
	stream := "data: {\"usage\":\ndata: {\"prompt_tokens\":5}}\n\n"
	got := SSEPayloads([]byte(stream))
	if len(got) != 1 {
		t.Fatalf("应合并为 1 个载荷，得到 %d", len(got))
	}
	u, ok := ParseJSON(got[0])
	if !ok {
		t.Fatalf("拼接后的载荷应可解析: %q", got[0])
	}
	if u.InputTokens != 5 {
		t.Errorf("InputTokens = %d, 期望 5", u.InputTokens)
	}
}

func TestSSEPayloadsCRLF(t *testing.T) {
	stream := "data: {\"usage\":{\"prompt_tokens\":7}}\r\n\r\n"
	u, ok := ParseSSE([]byte(stream))
	if !ok {
		t.Fatal("CRLF 换行应被正确处理")
	}
	if u.InputTokens != 7 {
		t.Errorf("InputTokens = %d, 期望 7", u.InputTokens)
	}
}

func TestSSEPayloadsNoTrailingBlankLine(t *testing.T) {
	// 流被截断、末尾缺空行时最后一个事件也不能丢。
	stream := "data: {\"usage\":{\"prompt_tokens\":11}}"
	u, ok := ParseSSE([]byte(stream))
	if !ok {
		t.Fatal("末帧无空行时也应解析")
	}
	if u.InputTokens != 11 {
		t.Errorf("InputTokens = %d, 期望 11", u.InputTokens)
	}
}

func TestClaudeStreamAccumulation(t *testing.T) {
	// Claude 流式：输入在 message_start，累计输出在 message_delta。
	stream := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1500,"output_tokens":1,"cache_creation_input_tokens":800,"cache_read_input_tokens":2400}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"你"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":137}}

event: message_stop
data: {"type":"message_stop"}

`
	u, ok := ParseSSE([]byte(stream))
	if !ok {
		t.Fatal("应从流中解析出用量")
	}
	if u.InputTokens != 1500 {
		t.Errorf("InputTokens = %d, 期望 1500", u.InputTokens)
	}
	// message_delta 的 137 必须胜过 message_start 的占位 1。
	if u.OutputTokens != 137 {
		t.Errorf("OutputTokens = %d, 期望 137", u.OutputTokens)
	}
	if u.CacheCreationTokens != 800 || u.CacheReadTokens != 2400 {
		t.Errorf("缓存字段错误: %+v", u)
	}
	if u.Protocol != ProtocolClaude {
		t.Errorf("Protocol = %q, 期望 claude", u.Protocol)
	}
	b := u.Billable()
	if b.Input != 1500 || b.Output != 137 || b.CacheRead != 2400 || b.CacheCreation != 800 {
		t.Errorf("Billable = %+v", b)
	}
}

func TestOpenAIStreamAccumulation(t *testing.T) {
	// OpenAI 流式：中间帧 usage 为 null，末帧给出完整 usage。
	stream := `data: {"id":"1","choices":[{"delta":{"content":"你"}}],"usage":null}

data: {"id":"1","choices":[{"delta":{"content":"好"}}],"usage":null}

data: {"id":"1","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,"prompt_tokens_details":{"cached_tokens":16}}}

data: [DONE]

`
	u, ok := ParseSSE([]byte(stream))
	if !ok {
		t.Fatal("应从流中解析出用量")
	}
	if u.InputTokens != 20 || u.OutputTokens != 5 || u.TotalTokens != 25 {
		t.Errorf("字段错误: %+v", u)
	}
	b := u.Billable()
	if b.Input != 4 || b.CacheRead != 16 {
		t.Errorf("Billable = %+v, 期望 Input=4 CacheRead=16", b)
	}
}

func TestGeminiStreamAccumulation(t *testing.T) {
	// Gemini 流式：每帧都带累计 usageMetadata，取最大值即为最终值。
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"你"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11}}

data: {"candidates":[{"content":{"parts":[{"text":"好"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}}

data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":42,"thoughtsTokenCount":7,"totalTokenCount":59}}

`
	u, ok := ParseSSE([]byte(stream))
	if !ok {
		t.Fatal("应从流中解析出用量")
	}
	if u.InputTokens != 10 {
		t.Errorf("InputTokens = %d, 期望 10", u.InputTokens)
	}
	if u.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, 期望 42（取累计最大值）", u.OutputTokens)
	}
	if u.ReasoningTokens != 7 {
		t.Errorf("ReasoningTokens = %d, 期望 7", u.ReasoningTokens)
	}
	if b := u.Billable(); b.Output != 49 {
		t.Errorf("Billable.Output = %d, 期望 49（42+7）", b.Output)
	}
}

func TestAccumulatorIncremental(t *testing.T) {
	// 分块喂入（模拟真实流式读取，块边界不对齐事件边界的情况除外）。
	var acc Accumulator
	if _, ok := acc.Result(); ok {
		t.Error("未喂入任何数据时应返回 ok=false")
	}
	acc.FeedSSE([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":90,\"output_tokens\":1}}}\n\n"))
	if u, ok := acc.Result(); !ok || u.InputTokens != 90 {
		t.Errorf("首块后 = %+v, ok=%v", u, ok)
	}
	acc.FeedSSE([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":33}}\n\n"))
	u, ok := acc.Result()
	if !ok {
		t.Fatal("应有结果")
	}
	if u.InputTokens != 90 || u.OutputTokens != 33 {
		t.Errorf("累积结果 = %+v, 期望 in=90 out=33", u)
	}

	acc.Reset()
	if _, ok := acc.Result(); ok {
		t.Error("Reset 后应回到空状态")
	}
}

func TestAccumulatorMergeNeverDecreases(t *testing.T) {
	// 后到的较小值不得覆盖已知较大值（防止末帧占位值抹掉真实用量）。
	var acc Accumulator
	acc.Feed([]byte(`{"usage":{"input_tokens":100,"output_tokens":50}}`))
	acc.Feed([]byte(`{"usage":{"input_tokens":0,"output_tokens":1}}`))
	u, _ := acc.Result()
	if u.InputTokens != 100 || u.OutputTokens != 50 {
		t.Errorf("合并后 = %+v, 期望 in=100 out=50", u)
	}
}

func TestParseAutoDetect(t *testing.T) {
	// JSON 形态
	if u, ok := Parse([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4}}`)); !ok || u.InputTokens != 3 {
		t.Errorf("JSON 自动识别失败: %+v ok=%v", u, ok)
	}
	// SSE 形态
	if u, ok := Parse([]byte("data: {\"usage\":{\"prompt_tokens\":8}}\n\n")); !ok || u.InputTokens != 8 {
		t.Errorf("SSE 自动识别失败: %+v ok=%v", u, ok)
	}
	// 都不是
	if _, ok := Parse([]byte("garbage")); ok {
		t.Error("无用量载荷应返回 false")
	}
	if _, ok := Parse(nil); ok {
		t.Error("空载荷应返回 false")
	}
}

func TestEffectiveTotal(t *testing.T) {
	// 上游给了 total 就用它。
	u := Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, ReasoningInOutput: true}
	if got := u.EffectiveTotal(); got != 15 {
		t.Errorf("EffectiveTotal = %d, 期望 15", got)
	}
	// 缺失 total 时按计费口径求和。
	u2 := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheCreationTokens: 3, ReasoningInOutput: true}
	if got := u2.EffectiveTotal(); got != 20 {
		t.Errorf("EffectiveTotal = %d, 期望 20", got)
	}
}

func TestBillableSum(t *testing.T) {
	b := Billable{Input: 1, Output: 2, CacheRead: 3, CacheCreation: 4}
	if b.Sum() != 10 {
		t.Errorf("Sum = %d, 期望 10", b.Sum())
	}
}

func TestUsageArithmeticSaturatesInsteadOfWrapping(t *testing.T) {
	// 恶意或损坏的上游 usage 不得通过 int64 回绕污染后续计费。
	u, ok := ParseJSON([]byte(`{"usageMetadata":{"candidatesTokenCount":9223372036854775807,"thoughtsTokenCount":1}}`))
	if !ok {
		t.Fatal("应识别出 usage")
	}
	if got := u.Billable().Output; got != math.MaxInt64 {
		t.Errorf("输出 token 溢出应钳位到 MaxInt64，得到 %d", got)
	}
	if got := u.EffectiveTotal(); got != math.MaxInt64 {
		t.Errorf("合计 token 溢出应钳位到 MaxInt64，得到 %d", got)
	}

	u, ok = ParseJSON([]byte(`{"usage":{"input_tokens":1,"output_tokens":2,"cache_creation":{"a":9223372036854775807,"b":1}}}`))
	if !ok {
		t.Fatal("应识别出 Claude usage")
	}
	if got := u.CacheCreationTokens; got != math.MaxInt64 {
		t.Errorf("缓存写入 token 溢出应钳位到 MaxInt64，得到 %d", got)
	}
}

func TestBillableCategoriesDoNotOverlap(t *testing.T) {
	// 归一化后四类之和不得超过上游报告的 token 总量（否则说明重复计数）。
	cases := []string{
		`{"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"prompt_tokens_details":{"cached_tokens":800}}}`,
		`{"usage":{"input_tokens":500,"output_tokens":60,"total_tokens":560,"input_tokens_details":{"cached_tokens":400}}}`,
		`{"usageMetadata":{"promptTokenCount":900,"candidatesTokenCount":80,"cachedContentTokenCount":700,"totalTokenCount":980}}`,
	}
	for _, c := range cases {
		u, ok := ParseJSON([]byte(c))
		if !ok {
			t.Fatalf("解析失败: %s", c)
		}
		b := u.Billable()
		if b.Sum() > u.TotalTokens {
			t.Errorf("计费四类合计 %d 超过上游 total %d，存在重复计数: %s", b.Sum(), u.TotalTokens, c)
		}
	}
}

func TestClaudeWithoutCacheFieldsStillBillsCorrectly(t *testing.T) {
	// 无缓存字段的 Claude 响应会被归类为 openai_responses（形状相同），
	// 但由于 CachedTokens 为 0，计费结果必须完全一致。
	u, ok := ParseJSON([]byte(`{"usage":{"input_tokens":123,"output_tokens":45}}`))
	if !ok {
		t.Fatal("应解析出用量")
	}
	b := u.Billable()
	if b.Input != 123 || b.Output != 45 || b.CacheRead != 0 || b.CacheCreation != 0 {
		t.Errorf("Billable = %+v, 期望 Input=123 Output=45", b)
	}
}

func TestMalformedUsageContainerIsSkipped(t *testing.T) {
	// usage 是标量/数组而非对象时应跳过，不 panic。
	for _, c := range []string{
		`{"usage":123}`,
		`{"usage":"abc"}`,
		`{"usage":[1,2]}`,
		`{"usageMetadata":"nope"}`,
	} {
		if _, ok := ParseJSON([]byte(c)); ok {
			t.Errorf("%q 不应解析成功", c)
		}
	}
}
