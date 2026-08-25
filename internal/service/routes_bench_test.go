package service

// 路由热路径微基准：量化 ROUTE-OPTIMIZATION.md 中的三个关键疑点。
// 运行：go test ./internal/service/ -bench BenchmarkRouteHotPath -benchmem -run '^$'
//
//	1) RequestDigest（整包 map 解析 + 递归采集）在无 ai_judge 规则上的浪费；
//	2) ParseRequestMeta 类型化解析的基线成本；
//	3) preparePayload 的整包 Unmarshal+Marshal 往返成本。
import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// benchBody 构造一个接近真实形态的 OpenAI chat-completions 请求体：
// msgs 条消息、每条约 msgChars 字节，总量约 targetBytes。
func benchBody(targetBytes, msgChars int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"auto","stream":true,"max_tokens":4096,"service_tier":"default","reasoning":{"effort":"medium"},"messages":[`)
	first := true
	written := 0
	i := 0
	for written < targetBytes {
		if !first {
			sb.WriteByte(',')
		}
		first = false
		msg := fmt.Sprintf(`{"role":"user","content":"消息 %d：%s"}`, i, strings.Repeat("内容样例文本", msgChars/16+1))
		sb.WriteString(msg)
		written += len(msg)
		i++
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func benchmarkBodies() (small, large []byte) {
	return benchBody(100*1024, 400), benchBody(1024*1024, 4000)
}

func benchmarkRequestDigest(b *testing.B, body []byte) {
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if d := RequestDigest(body); d == "" {
			b.Fatal("空摘要")
		}
	}
}

func BenchmarkRouteHotPathDigest100KB(b *testing.B) {
	small, _ := benchmarkBodies()
	benchmarkRequestDigest(b, small)
}

func BenchmarkRouteHotPathDigest1MB(b *testing.B) {
	_, large := benchmarkBodies()
	benchmarkRequestDigest(b, large)
}

func benchmarkParseMeta(b *testing.B, body []byte) {
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m := ParseRequestMeta(body)
		if m.Model == "" {
			b.Fatal("model 为空")
		}
	}
}

func BenchmarkRouteHotPathParseMeta100KB(b *testing.B) {
	small, _ := benchmarkBodies()
	benchmarkParseMeta(b, small)
}

func BenchmarkRouteHotPathParseMeta1MB(b *testing.B) {
	_, large := benchmarkBodies()
	benchmarkParseMeta(b, large)
}

// benchmarkPayloadRoundTrip 是 preparePayload 的核心开销：
// 整包 Unmarshal 到 map[string]any → 改 model → 整包 Marshal 回来。
func benchmarkPayloadRoundTrip(b *testing.B, body []byte) {
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			b.Fatal("unmarshal 失败")
		}
		payload["model"] = "target-x"
		if _, err := json.Marshal(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRouteHotPathRoundTrip100KB(b *testing.B) {
	small, _ := benchmarkBodies()
	benchmarkPayloadRoundTrip(b, small)
}

func BenchmarkRouteHotPathRoundTrip1MB(b *testing.B) {
	_, large := benchmarkBodies()
	benchmarkPayloadRoundTrip(b, large)
}

// BenchmarkRouteHotPathRandSource 量化 BuildRouteEnv 每请求
// rand.New(rand.NewSource(...)) 的构造成本（routelang weighted 用一次 Float64）。
func BenchmarkRouteHotPathRandSource(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		r := rand.New(rand.NewSource(12345))
		_ = r.Float64()
	}
}

func BenchmarkRouteHotPathRandGlobal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = rand.Float64()
	}
}
