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
	}
	for _, c := range cases {
		if got := RedactSource(c.in); got != c.want {
			t.Fatalf("RedactSource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
