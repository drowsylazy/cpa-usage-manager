package httpapi

import (
	"encoding/json"
	"testing"
)

// TestJSONCoerceContract 把 jsonStr / jsonInt64 / jsonBool 三个手工解析
// 辅助的输入契约钉成表格。这组函数是 httpapi 双口径（裸数字 vs 字符串）
// 兼容层的唯一收口，历史上两次真实 bug（金额限额编辑恒失败、并发字段
// 恒报「必须是整数」）都源于「错误被忽略后拿到空串/零值」——任何改动
// 都必须让这张表先过。
func TestJSONCoerceContract(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }

	t.Run("jsonStr 只接受 JSON 字符串，其余静默为空串", func(t *testing.T) {
		cases := []struct {
			in   string
			want string
		}{
			{`"hello"`, "hello"},
			{`""`, ""},
			{`"123"`, "123"},
			{`123`, ""},    // 裸数字 → 空串（不是 "123"！调用方必须自己选 jsonInt64）
			{`true`, ""},   // 类型不匹配 → 空串
			{`null`, ""},   // null → 空串
			{`{"a":1}`, ""}, // 对象 → 空串
		}
		for _, c := range cases {
			if got := jsonStr(raw(c.in)); got != c.want {
				t.Errorf("jsonStr(%s) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("jsonInt64 数字优先、字符串回退，两者都失败才报错", func(t *testing.T) {
		ok := []struct {
			in   string
			want int64
		}{
			{`500000`, 500000},   // 裸数字（面板口径）
			{`"500000"`, 500000}, // 带引号（历史金额口径）
			{` 42 `, 42},         // 字符串带空白归一
			{`-7`, -7},           // 负数是合法语义（限额 -1=不限）
			{`0`, 0},
		}
		for _, c := range ok {
			got, err := jsonInt64(raw(c.in))
			if err != nil || got != c.want {
				t.Errorf("jsonInt64(%s) = %d, %v; want %d, nil", c.in, got, err, c.want)
			}
		}
		bad := []string{`"abc"`, `null`, `true`, `{"a":1}`, `1.5`, `"1.5"`, ``}
		for _, in := range bad {
			if _, err := jsonInt64(raw(in)); err == nil {
				t.Errorf("jsonInt64(%s) 应报错（浮点/非数不是合法整数语义）", in)
			}
		}
	})

	t.Run("jsonBool 只接受 JSON 布尔，其余静默为 false", func(t *testing.T) {
		if !jsonBool(raw(`true`)) || jsonBool(raw(`false`)) {
			t.Error("布尔字面量应原样解析")
		}
		for _, in := range []string{`"true"`, `1`, `null`, ``, `{}`} {
			if jsonBool(raw(in)) {
				t.Errorf("jsonBool(%s) 应为 false（字符串/数字不是布尔）", in)
			}
		}
	})
}
