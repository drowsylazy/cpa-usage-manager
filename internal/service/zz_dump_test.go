package service

// 临时调试文件：CPA_DUMP=1 时把点阵表与示例图渲染到仓库根目录，供人工核对。
// 核对完成后删除本文件。

import (
	"image/color"
	"image/png"
	"os"
	"testing"
)

func TestDumpGlyphSheet(t *testing.T) {
	if os.Getenv("CPA_DUMP") == "" {
		t.Skip("set CPA_DUMP=1")
	}
	const scale = 4
	rows := []string{
		" !\"#$%&'()*+,-./",
		"0123456789:;<=>?",
		"@ABCDEFGHIJKLMNO",
		"PQRSTUVWXYZ[\\]^_",
		"`abcdefghijklmno",
		"pqrstuvwxyz{|}~",
	}
	c := newCanvas(16*(glyphW+1)*scale+40, len(rows)*(glyphH+3)*scale+40, colBG)
	for i, r := range rows {
		c.text(20, 20+i*(glyphH+3)*scale, r, color.RGBA{0, 0, 0, 0xFF}, scale)
	}
	f, err := os.Create("../../.tmp-glyphs.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, c.img); err != nil {
		t.Fatal(err)
	}
}

func TestDumpChart(t *testing.T) {
	if os.Getenv("CPA_DUMP") == "" {
		t.Skip("set CPA_DUMP=1")
	}
	h := chartSpec{
		Title:      "Top model by cost",
		Subtitle:   "5 points  |  provider=anthropic  |  generated 2026-08-20",
		Labels:     []string{"claude-opus-5", "claude-sonnet-5", "gpt-5.2-codex", "qwen3-coder-plus", "gemini-2.5-pro-preview-latest-long"},
		Values:     []int64{1_200_000, 845_000, 402_100, 90_500, 12_000},
		Horizontal: true,
		Format:     compactInt,
	}
	v := chartSpec{
		Title:  "Usage trends (day / requests)",
		Labels: []string{"08-01", "08-02", "08-03", "08-04", "08-05", "08-06", "08-07", "08-08", "08-09", "08-10", "08-11", "08-12"},
		Values: []int64{120, 340, 90, 520, 610, 480, 150, 730, 690, 410, 260, 880},
		Format: compactInt,
	}
	for name, spec := range map[string]chartSpec{"../../.tmp-h.png": h, "../../.tmp-v.png": v} {
		f, err := os.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, renderChart(spec, 1280, 720)); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}
