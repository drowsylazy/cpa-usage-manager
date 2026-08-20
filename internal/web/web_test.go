package web

import (
	"strings"
	"testing"
)

func TestConsoleHTML(t *testing.T) {
	html := string(ConsoleHTML())

	// 必须包含各页签与关键能力元素。
	expected := []string{
		"CPA 用量管理", "概览", "密钥", "用量", "价格", "认证额度", "审计", "系统",
		"cpa-management-key", "sessionStorage", "mgmt-key", "ov-chart",
		"issue-form", "key-rows", "dim-table", "pricing-rows", "auth-rows",
		"audit-rows", "backup-btn", "restore-btn", "reset-btn",
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
