package web

import (
	"strings"
	"testing"
)

func TestConsoleHTML(t *testing.T) {
	html := string(ConsoleHTML())

	// 组装后必须是自包含单文件：CSS / JS 已注入占位符。
	if strings.Contains(html, "/*@console.css*/") || strings.Contains(html, "/*@console.js*/") {
		t.Error("CSS/JS 占位符未被注入")
	}
	if !strings.Contains(html, "--signal") || !strings.Contains(html, "sessionStorage") {
		t.Error("组装结果缺少样式或脚本内容")
	}

	// 必须包含各页签与关键能力元素。
	expected := []string{
		"CPA 用量管理", "概览", "密钥", "用量", "价格", "系统",
		"cpa-management-key", "gate-key", "trend-chart",
		"key-rows", "dim-body", "pricing-rows",
		"backup-btn", "restore-btn", "reset-btn",
		"/v0/management/plugins/cpa-usage-manager",
	}
	for _, e := range expected {
		if !strings.Contains(html, e) {
			t.Errorf("HTML 缺少 %q", e)
		}
	}

	// HTML 壳不得内嵌业务数据（绝无明文 Key、金额或 SQL）。
	for _, leak := range []string{"cum-", "INSERT INTO", "quota_micro_usd\":100"} {
		if strings.Contains(html, leak) {
			t.Errorf("HTML 壳不应包含数据泄漏标记 %q", leak)
		}
	}
}
