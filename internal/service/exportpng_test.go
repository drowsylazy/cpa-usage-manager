package service

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestGlyphTableComplete(t *testing.T) {
	want := (int(glyphLast) - int(glyphFirst) + 1) * glyphW
	if len(glyph5x7) != want {
		t.Fatalf("点阵表长度 %d，期望 %d", len(glyph5x7), want)
	}
	// 空格必须全空，'?' 必须非空，否则说明表整体错位。
	for _, b := range glyphFor(' ') {
		if b != 0 {
			t.Fatalf("空格字形不应有像素: %v", glyphFor(' '))
		}
	}
	var any byte
	for _, b := range glyphFor('?') {
		any |= b
	}
	if any == 0 {
		t.Fatal("'?' 字形为空，点阵表可能错位")
	}
}

func TestTextWidth(t *testing.T) {
	if got := textWidth("", 1); got != 0 {
		t.Fatalf("空串宽度应为 0，得到 %d", got)
	}
	if got := textWidth("A", 1); got != glyphW {
		t.Fatalf("单字符宽度应为 %d，得到 %d", glyphW, got)
	}
	if got := textWidth("AB", 2); got != 2*(glyphW+1)*2-2 {
		t.Fatalf("双字符宽度不符: %d", got)
	}
}

func TestNiceCeil(t *testing.T) {
	cases := map[int64]int64{0: 1, 1: 1, 3: 5, 7: 10, 12: 20, 45: 50, 99: 100, 1234: 2000}
	for in, want := range cases {
		if got := niceCeil(in); got != want {
			t.Fatalf("niceCeil(%d) = %d，期望 %d", in, got, want)
		}
	}
}

func TestScaleToStaysInRange(t *testing.T) {
	if got := scaleTo(5, 10, 100); got != 50 {
		t.Fatalf("scaleTo 线性映射错误: %d", got)
	}
	if got := scaleTo(-1, 10, 100); got != 0 {
		t.Fatalf("负值应映射为 0，得到 %d", got)
	}
	if got := scaleTo(99, 10, 100); got != 100 {
		t.Fatalf("超上界应截断到 span，得到 %d", got)
	}
	if got := scaleTo(1, 0, 100); got != 0 {
		t.Fatalf("max=0 应返回 0，得到 %d", got)
	}
}

func TestCompactInt(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1500: "1.5k", 2_400_000: "2.4M",
		3_100_000_000: "3.1G", -1500: "-1.5k"}
	for in, want := range cases {
		if got := compactInt(in); got != want {
			t.Fatalf("compactInt(%d) = %q，期望 %q", in, got, want)
		}
	}
}

func TestTruncateLabel(t *testing.T) {
	if got := truncateLabel("short", 30); got != "short" {
		t.Fatalf("短标签不应改动: %q", got)
	}
	got := truncateLabel(strings.Repeat("a", 40)+"tail", 20)
	if len([]rune(got)) != 20 {
		t.Fatalf("截断后长度应为 20，得到 %d (%q)", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "tail") || !strings.Contains(got, "~") {
		t.Fatalf("截断应保留尾部并插入省略号: %q", got)
	}
}

func TestRenderChartVerticalHasInk(t *testing.T) {
	spec := chartSpec{
		Title:  "trend",
		Labels: []string{"01-01", "01-02", "01-03"},
		Values: []int64{10, 40, 25},
	}
	img := renderChart(spec, 640, 360)
	if img.Bounds().Dx() != 640 || img.Bounds().Dy() != 360 {
		t.Fatalf("尺寸不符: %v", img.Bounds())
	}
	if n := countColor(img, colBar); n == 0 {
		t.Fatal("纵向柱状图没有画出任何柱体")
	}
	// 最高柱应当接近绘图区顶部：统计柱色像素的最小 y。
	if topY := firstRowWith(img, colBar); topY > 160 {
		t.Fatalf("最高柱顶部 y=%d，未按比例拉伸", topY)
	}
}

func TestRenderChartHorizontalHasInk(t *testing.T) {
	spec := chartSpec{
		Title:      "top models",
		Labels:     []string{"claude-opus-5", "claude-sonnet-5"},
		Values:     []int64{100, 20},
		Horizontal: true,
	}
	img := renderChart(spec, 800, 400)
	if n := countColor(img, colBar); n == 0 {
		t.Fatal("横向条形图没有画出任何条")
	}
}

func TestRenderChartEmptyStillEncodes(t *testing.T) {
	img := renderChart(chartSpec{Title: "empty"}, 320, 200)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("空数据图编码失败: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("空数据图编码结果为空")
	}
}

// countColor 统计图中等于 want 的像素数。
func countColor(img *image.RGBA, want color.RGBA) int {
	n := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y) == want {
				n++
			}
		}
	}
	return n
}

// firstRowWith 返回首次出现 want 颜色的行号，未出现时返回 -1。
func firstRowWith(img *image.RGBA, want color.RGBA) int {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y) == want {
				return y
			}
		}
	}
	return -1
}
