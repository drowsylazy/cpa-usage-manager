package web

import (
	"bytes"
	"io/fs"
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

// TestConsoleJSAssembly 钉住多文件拼接的顺序与完整性：
// ①js 目录下的每个文件都必须登记进 jsParts（拒绝孤儿段）；
// ②拼接结果首尾正确（IIFE 开合）、分节标记齐全且「启动」段在最末——
// 顶层执行代码撞上后段 const 的 TDZ 是 v0.7.4 白屏的根因，顺序是硬约束。
func TestConsoleJSAssembly(t *testing.T) {
	entries, err := fs.ReadDir(jsFS, "js")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !seenValidName(e.Name()) {
			t.Errorf("js 目录出现不认识的段文件 %q：请登记进 web.go 的 jsParts", e.Name())
		}
		seen[e.Name()] = true
	}
	for _, name := range jsParts {
		if !seen[name] {
			t.Errorf("jsParts 登记了 %q 但 js 目录里没有该文件", name)
		}
	}

	js := assembleJS()
	// 文件头是版权注释，IIFE 开启紧跟其后；末尾是 IIFE 关闭。
	if !bytes.HasPrefix(js, []byte("/* CPA 用量管理")) || !bytes.Contains(js[:200], []byte("(function () {")) {
		t.Error("拼接结果应以文件头注释 + IIFE 开启开头")
	}
	if !bytes.HasSuffix(bytes.TrimRight(js, "\n"), []byte("})();")) {
		t.Error("拼接结果应以 IIFE 关闭结尾")
	}

	boot := bytes.Index(js, []byte("// ---------- 启动 ----------"))
	if boot < 0 {
		t.Fatal("缺少「启动」段标记")
	}
	for _, banner := range []string{"// ---------- 下拉组件 ----------", "// ---------- 密钥 ----------", "// ---------- 实时（进行中请求） ----------"} {
		i := bytes.Index(js, []byte(banner))
		if i < 0 {
			t.Fatalf("缺少分节标记 %q", banner)
		}
		if i > boot {
			t.Errorf("分节 %q 出现在「启动」段之后，执行顺序违规", banner)
		}
	}
}

func seenValidName(name string) bool {
	for _, n := range jsParts {
		if n == name {
			return true
		}
	}
	return false
}
