package web

import (
	"regexp"
	"strings"
	"testing"
)

// thRe 匹配表头单元格标签（<th> 或 <th ...>），排除 <thead>。
var thRe = regexp.MustCompile(`<th[ >]`)

// TestLiveTabTableArity 锁实时页三张表的表头列数。改表头却忘改行渲染
// （或反之）会让整行错位——列数不一致在浏览器里表现为数据串列，静态
// 检查与 node --check 都看不出来，用本测试在读 HTML 的层面钉住表头。
func TestLiveTabTableArity(t *testing.T) {
	html := string(ConsoleHTML())
	for _, c := range []struct {
		id  string
		col int
	}{
		{"held-table", 7},
		{"recent-table", 7},
		{"accuracy-table", 4},
		{"densities-table", 6},
	} {
		block := tableHeadBlock(t, html, c.id)
		// 只数 <th 标签（<thead 以 <th 开头，须排除）。
		got := thRe.FindAllStringIndex(block, -1)
		if len(got) != c.col {
			t.Errorf("%s 表头列数 = %d，期望 %d（行渲染必须同步调整）", c.id, len(got), c.col)
		}
	}
}

// tableHeadBlock 取出某个 table 的 thead 片段。
func tableHeadBlock(t *testing.T, html, id string) string {
	t.Helper()
	marker := `id="` + id + `"`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("HTML 中找不到表格 %s", id)
	}
	rest := html[i:]
	j := strings.Index(rest, "</thead>")
	if j < 0 {
		t.Fatalf("表格 %s 缺少 </thead>", id)
	}
	return rest[:j]
}

// TestLiveTabMergedCells 锁实时页的复合单元格与状态列已就位：预估→实际、
// 预占→实扣、密度±MAD、读/写 各自合并为一格（改动前是 10 列/8 列，
// 强关联读数被拆散到整行两端）。
func TestLiveTabMergedCells(t *testing.T) {
	js := string(ConsoleJS())
	for _, want := range []string{"cell-pair", "Token 预估 → 实际", "金额 预占 → 实扣", "缓存构成（读 / 写）"} {
		// 标签在 HTML、类在 JS，分别断言。
		if !strings.Contains(string(consoleHTML), want) && !strings.Contains(js, want) {
			t.Errorf("实时页缺少 %q", want)
		}
	}
	// 已移除的无意义口径列不得复现（「% of 4B」是把学习密度比作常识值的
	// 自造指标，对判断预占准不准没有信息量）。
	if regexp.MustCompile(`of 4B`).MatchString(js) {
		t.Error("不应再出现自造的「of 4B」口径列")
	}
}
