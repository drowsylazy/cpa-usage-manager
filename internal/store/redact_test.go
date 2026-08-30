package store

import "testing"

func TestRedactSource(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"openai", "openai"},
		{"openai-compatible-stepfun", "openai-compatible-stepfun"},
		{"webui", "webui"},
		{"", ""},
		{"sk-A69OqIScxVgXsKZJUXwpBCUJuUKGOxu2J9OiJJQ0ccTr2l8i", ""},
		{"2i2Pv5n8VgUMNszTwc6xbterMbbB8OgpaIR2uCBenLqMf0yKkOqWg1AJHnaScjrJP", ""},
		{"shortkey1234567890123456789012345678", ""}, // 32 位纯字母数字
		{"api.openai.com/v1", "api.openai.com/v1"},
		// 带分隔符的凭据（≥20 位字母数字混合，宿主回落填 api_key 的实测形态）
		{"fk-a1b2c3d4e5f6g7h8i9j0", ""},
		{"AIzaSyA1bC2dE3fG4hI5jK6lM7nO8pQ9rS0tU", ""},
		{"gsk_9f8e7d6c5b4a3210fedcba9876543210", ""},
		{"proj-key-4f1e8d7c-2b3a-49c0", ""}, // 含 '-'，旧规则漏放
		// JWT（三点分段）
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV", ""},
		// 混合文本中的凭据 token
		{"Bearer ey_a1b2c3d4e5f6g7h8i9j0", ""},
		{"key sk-A69OqIScxVgXsKZJUXwpBCUJuUKGOxu2J9OiJJQ0ccTr2l8i", ""},
		// 正常来源保留
		{"user@example.com", "user@example.com"},
		{"my-gcp-project", "my-gcp-project"},
		{"openai-compatible", "openai-compatible"},
		{"vertex ai-gcp-87654321", "vertex ai-gcp-87654321"}, // 短项目 ID 无凭据长度
		{"openai gemini claude", "openai gemini claude"},
	}
	for _, c := range cases {
		if got := RedactSource(c.in); got != c.want {
			t.Fatalf("RedactSource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
