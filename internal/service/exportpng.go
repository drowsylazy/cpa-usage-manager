package service

// PNG 导出全部用标准库渲染：不引入绘图依赖，也不依赖系统字体。
// 字形取自内置 5x7 点阵表（覆盖可打印 ASCII，其余字符渲染为 '?'），
// 因此在任何平台上导出的图片都完全一致。

import (
	"context"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

const (
	glyphW     = 5
	glyphH     = 7
	glyphFirst = ' '
	glyphLast  = '~'
)

// glyph5x7 是从 0x20 起每字符 5 列的点阵数据（每列自低位向上对应第 0..6 行）。
var glyph5x7 = mustHex(
	"0000000000" + "00005F0000" + "0007000700" + "147F147F14" + "242A7F2A12" + "2313086462" + "3649552250" + "0005030000" +
		"001C224100" + "0041221C00" + "14083E0814" + "08083E0808" + "0050300000" + "0808080808" + "0060600000" + "2010080402" +
		"3E5149453E" + "00427F4000" + "4261514946" + "2141454B31" + "1814127F10" + "2745454539" + "3C4A494930" + "0171090503" +
		"3649494936" + "064949291E" + "0036360000" + "0056360000" + "0814224100" + "1414141414" + "0041221408" + "0201510906" +
		"324979413E" + "7E1111117E" + "7F49494936" + "3E41414122" + "7F4141221C" + "7F49494941" + "7F09090101" + "3E41415132" +
		"7F0808087F" + "00417F4100" + "2040413F01" + "7F08142241" + "7F40404040" + "7F0204027F" + "7F0408107F" + "3E4141413E" +
		"7F09090906" + "3E4151215E" + "7F09192946" + "2649494932" + "03017F0103" + "3F4040403F" + "1F2040201F" + "3F4038403F" +
		"6314081463" + "0304780403" + "6151494543" + "00007F4141" + "0204081020" + "41417F0000" + "0402010204" + "4040404040" +
		"0001020400" + "2054545478" + "7F48444438" + "3844444420" + "384444487F" + "3854545418" + "087E090102" + "5864642438" +
		"7F08040478" + "00447D4000" + "2040443D00" + "7F10284400" + "00417F4000" + "7C04780478" + "7C08040478" + "3844444438" +
		"7C24242418" + "182424247C" + "7C08040408" + "4854545420" + "043F444020" + "3C4040201C" + "1C2040201C" + "3C4030403C" +
		"4428102844" + "0C5050503C" + "4464544C44" + "0008364100" + "00007F0000" + "0041360800" + "0804081008")

// mustHex 解析内置点阵表；表是常量，解析失败属于代码缺陷。
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("service: 内置点阵表损坏: " + err.Error())
	}
	return b
}

// glyphFor 返回一个字符的 5 列点阵。
func glyphFor(r rune) []byte {
	if r < glyphFirst || r > glyphLast {
		r = '?'
	}
	i := (int(r) - int(glyphFirst)) * glyphW
	if i+glyphW > len(glyph5x7) {
		return glyph5x7[:glyphW]
	}
	return glyph5x7[i : i+glyphW]
}

// canvas 是一块可绘制的 RGBA 位图，只提供本文件需要的几种图元。
type canvas struct{ img *image.RGBA }

// newCanvas 创建填充背景色的画布。
func newCanvas(w, h int, bg color.RGBA) *canvas {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	c := &canvas{img: img}
	c.rect(0, 0, w, h, bg)
	return c
}

// rect 填充 [x0,x1) × [y0,y1) 的矩形，超出画布的部分自动裁剪。
func (c *canvas) rect(x0, y0, x1, y1 int, col color.RGBA) {
	b := c.img.Bounds()
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	x0, y0 = max(x0, b.Min.X), max(y0, b.Min.Y)
	x1, y1 = min(x1, b.Max.X), min(y1, b.Max.Y)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			c.img.SetRGBA(x, y, col)
		}
	}
}

// line 画一条任意方向的直线（Bresenham）。
func (c *canvas) line(x0, y0, x1, y1 int, col color.RGBA) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		c.rect(x0, y0, x0+1, y0+1, col)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

// text 在 (x, y) 处绘制文本（y 为文字顶端），scale 为整数放大倍数。
func (c *canvas) text(x, y int, s string, col color.RGBA, scale int) {
	if scale < 1 {
		scale = 1
	}
	cx := x
	for _, r := range s {
		g := glyphFor(r)
		for i := 0; i < glyphW; i++ {
			for j := 0; j < glyphH; j++ {
				if g[i]>>uint(j)&1 == 1 {
					px, py := cx+i*scale, y+j*scale
					c.rect(px, py, px+scale, py+scale, col)
				}
			}
		}
		cx += (glyphW + 1) * scale
	}
}

// textRight 让文本右端对齐到 x。
func (c *canvas) textRight(x, y int, s string, col color.RGBA, scale int) {
	c.text(x-textWidth(s, scale), y, s, col, scale)
}

// textCenter 让文本水平居中于 x。
func (c *canvas) textCenter(x, y int, s string, col color.RGBA, scale int) {
	c.text(x-textWidth(s, scale)/2, y, s, col, scale)
}

// textWidth 返回文本渲染宽度（末字符后不留间距）。
func textWidth(s string, scale int) int {
	if scale < 1 {
		scale = 1
	}
	n := len([]rune(s))
	if n == 0 {
		return 0
	}
	return n*(glyphW+1)*scale - scale
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// 图表配色：浅底深墨，单色柱，弱化网格线，保证在深浅主题下都可读。
var (
	colBG   = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	colInk  = color.RGBA{0x1F, 0x23, 0x28, 0xFF}
	colMute = color.RGBA{0x6B, 0x72, 0x80, 0xFF}
	colGrid = color.RGBA{0xE3, 0xE7, 0xEC, 0xFF}
	colBar  = color.RGBA{0x2F, 0x6F, 0xED, 0xFF}
	colBarT = color.RGBA{0x8F, 0xB4, 0xF7, 0xFF}
)

// chartSpec 描述一张要渲染的图。
type chartSpec struct {
	Title    string
	Subtitle string
	Labels   []string
	Values   []int64
	// Format 把数值渲染为坐标轴与柱顶文本。
	Format func(int64) string
	// Horizontal 为 true 时画横向条形图（适合模型名等长标签）。
	Horizontal bool
}

// niceCeil 把上界抬到 1/2/5×10^k 形式，让刻度取整好看。
func niceCeil(v int64) int64 {
	if v <= 0 {
		return 1
	}
	step := int64(1)
	for step*10 <= v {
		step *= 10
	}
	for _, m := range []int64{1, 2, 5, 10} {
		if v <= step*m {
			return step * m
		}
	}
	return step * 10
}

// scaleTo 把 v 按 max 映射到 [0, span] 的整数长度（纯整数运算）。
func scaleTo(v, max int64, span int) int {
	if max <= 0 || v <= 0 {
		return 0
	}
	if v > max {
		v = max
	}
	return int(int64(span) * v / max)
}

// renderChart 渲染一张图并返回位图。空数据时输出一张说明图。
func renderChart(spec chartSpec, w, h int) *image.RGBA {
	if spec.Format == nil {
		spec.Format = func(v int64) string { return itoa(v) }
	}
	c := newCanvas(w, h, colBG)
	c.text(24, 22, spec.Title, colInk, 2)
	if spec.Subtitle != "" {
		c.text(24, 44, spec.Subtitle, colMute, 1)
	}
	if len(spec.Values) == 0 {
		c.textCenter(w/2, h/2, "no data / 无数据", colMute, 2)
		return c.img
	}

	var maxV int64
	for _, v := range spec.Values {
		if v > maxV {
			maxV = v
		}
	}
	top := niceCeil(maxV)

	if spec.Horizontal {
		renderHBars(c, spec, w, h, top)
	} else {
		renderVBars(c, spec, w, h, top)
	}
	return c.img
}

// renderVBars 画纵向柱状图（时间序列）。
func renderVBars(c *canvas, spec chartSpec, w, h int, top int64) {
	left, right, up, down := 96, 24, 64, 52
	plotW, plotH := w-left-right, h-up-down
	baseY := up + plotH

	// 网格与 Y 轴刻度。
	for i := 0; i <= 4; i++ {
		y := up + plotH*i/4
		c.rect(left, y, left+plotW, y+1, colGrid)
		c.textRight(left-8, y-3, spec.Format(top*int64(4-i)/4), colMute, 1)
	}
	c.rect(left, up, left+1, baseY+1, colGrid)

	n := len(spec.Values)
	slot := plotW / n
	if slot < 1 {
		slot = 1
	}
	barW := max(1, slot*3/4)
	for i, v := range spec.Values {
		x := left + i*plotW/n + (slot-barW)/2
		bh := scaleTo(v, top, plotH)
		c.rect(x, baseY-bh, x+barW, baseY, colBar)
	}
	// X 轴标签最多 8 个，均匀取样，避免重叠。
	stepLabel := max(1, (n+7)/8)
	for i := 0; i < n; i += stepLabel {
		if i < len(spec.Labels) {
			c.textCenter(left+i*plotW/n+slot/2, baseY+10, spec.Labels[i], colMute, 1)
		}
	}
	c.textRight(w-right, h-14, fmt.Sprintf("max %s", spec.Format(top)), colMute, 1)
}

// renderHBars 画横向条形图（维度排行）。
func renderHBars(c *canvas, spec chartSpec, w, h int, top int64) {
	left, right, up, down := 200, 120, 64, 28
	plotW, plotH := w-left-right, h-up-down

	n := len(spec.Values)
	slot := plotH / n
	if slot < 3 {
		slot = 3
	}
	barH := max(2, slot*2/3)
	for i, v := range spec.Values {
		y := up + i*slot
		bw := scaleTo(v, top, plotW)
		c.rect(left, y, left+plotW, y+barH, colGrid)
		col := colBar
		if i >= 10 {
			col = colBarT
		}
		c.rect(left, y, left+bw, y+barH, col)
		label := ""
		if i < len(spec.Labels) {
			label = truncateLabel(spec.Labels[i], 30)
		}
		c.textRight(left-8, y+barH/2-3, label, colInk, 1)
		c.text(left+plotW+8, y+barH/2-3, spec.Format(v), colMute, 1)
	}
	c.rect(left, up, left+1, up+plotH, colGrid)
}

// truncateLabel 把过长标签截断为「前缀…后缀」，保留尾部以区分同族模型名。
func truncateLabel(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 6 {
		return s
	}
	head := (n - 1) * 2 / 3
	tail := n - 1 - head
	return string(r[:head]) + "~" + string(r[len(r)-tail:])
}

// compactInt 把大整数渲染为 1.2k / 3.4M 形式（整数运算，一位小数）。
func compactInt(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	var s string
	switch {
	case v >= 1_000_000_000:
		s = fmt.Sprintf("%d.%dG", v/1_000_000_000, v%1_000_000_000/100_000_000)
	case v >= 1_000_000:
		s = fmt.Sprintf("%d.%dM", v/1_000_000, v%1_000_000/100_000)
	case v >= 1_000:
		s = fmt.Sprintf("%d.%dk", v/1_000, v%1_000/100)
	default:
		s = itoa(v)
	}
	if neg {
		return "-" + s
	}
	return s
}

// PNGKinds 是 PNG 导出支持的图表类型。
var PNGKinds = []string{"trends", "dimension", "keys"}

// PNGMetrics 是可作为纵轴的指标。
var PNGMetrics = []string{"cost", "requests", "failures", "tokens", "input_tokens", "output_tokens"}

// ExportPNG 服务端渲染一张 PNG 图表并写入 w，返回建议的文件名。
//
// 服务端渲染而非依赖前端截图：导出结果与面板主题、字体、缩放无关，
// 也让宿主侧的定时任务能直接产出可归档的图片。
func (s *Service) ExportPNG(ctx context.Context, w io.Writer, req ExportRequest) (string, error) {
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "trends"
	}
	metric := strings.TrimSpace(req.Metric)
	if metric == "" {
		metric = "cost"
	}
	limit := req.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	f := req.Filter.Usage()
	usd := func(v int64) string { return "$" + money.Micro(v).USDString() }

	spec := chartSpec{Format: compactInt}
	switch kind {
	case "trends":
		grain := req.Grain
		if grain == "" {
			grain = "day"
		}
		points, err := s.Trends(ctx, f, grain)
		if err != nil {
			return "", err
		}
		for _, p := range points {
			v, ok := trendMetric(p, metric)
			if !ok {
				return "", fmt.Errorf("不支持的指标 %q，可选：%s", metric, strings.Join(PNGMetrics, ", "))
			}
			spec.Values = append(spec.Values, v)
			spec.Labels = append(spec.Labels, trendLabel(p.Bucket, grain))
		}
		spec.Title = fmt.Sprintf("Usage trends (%s / %s)", grain, metric)
	case "dimension", "keys":
		dim := req.Dimension
		if kind == "keys" {
			dim = "key_id"
		}
		rep, err := s.GroupByDimension(ctx, f, dim, limit)
		if err != nil {
			return "", err
		}
		for _, r := range rep.Rows {
			v, ok := dimensionMetric(r, metric)
			if !ok {
				return "", fmt.Errorf("不支持的指标 %q，可选：%s", metric, strings.Join(PNGMetrics, ", "))
			}
			spec.Values = append(spec.Values, v)
			spec.Labels = append(spec.Labels, r.Value)
		}
		spec.Horizontal = true
		spec.Title = fmt.Sprintf("Top %s by %s", rep.Dimension, metric)
	default:
		return "", fmt.Errorf("不支持的图表类型 %q，可选：%s", kind, strings.Join(PNGKinds, ", "))
	}
	if metric == "cost" {
		spec.Format = usd
	}
	spec.Subtitle = chartSubtitle(f, len(spec.Values))

	name := fmt.Sprintf("cpa-usage-manager_%s_%s.png", kind, time.Now().UTC().Format("20060102-150405"))
	img := renderChart(spec, 1280, 720)
	if err := png.Encode(w, img); err != nil {
		return "", err
	}
	return name, nil
}

// trendMetric 取趋势点上的指标值。
func trendMetric(p TrendPoint, metric string) (int64, bool) {
	switch metric {
	case "cost":
		return p.CostMicroUSD, true
	case "requests":
		return p.Requests, true
	case "failures":
		return p.Failures, true
	case "tokens":
		return p.TotalTokens, true
	case "input_tokens":
		return p.InputTokens, true
	case "output_tokens":
		return p.OutputTokens, true
	}
	return 0, false
}

// dimensionMetric 取维度行上的指标值。
func dimensionMetric(r DimensionRow, metric string) (int64, bool) {
	switch metric {
	case "cost":
		return int64(r.CostMicroUSD), true
	case "requests":
		return r.Requests, true
	case "failures":
		return r.Failures, true
	case "tokens":
		return r.TotalTokens, true
	case "input_tokens":
		return r.InputTokens, true
	case "output_tokens":
		return r.OutputTokens, true
	}
	return 0, false
}

// trendLabel 按粒度选择合适的横轴标签格式。
func trendLabel(t time.Time, grain string) string {
	switch grain {
	case "minute", "hour":
		return t.Format("01-02 15:04")
	case "month":
		return t.Format("2006-01")
	default:
		return t.Format("01-02")
	}
}

// chartSubtitle 把筛选条件写进副标题，让导出的图自带上下文。
func chartSubtitle(f UsageFilter, n int) string {
	parts := []string{fmt.Sprintf("%d points", n)}
	if !f.From.IsZero() || !f.To.IsZero() {
		from, to := "*", "*"
		if !f.From.IsZero() {
			from = f.From.Format("2006-01-02 15:04")
		}
		if !f.To.IsZero() {
			to = f.To.Format("2006-01-02 15:04")
		}
		parts = append(parts, from+" ~ "+to+" UTC")
	}
	for _, kv := range []struct{ k, v string }{
		{"key", f.KeyID}, {"caller", f.CallerID}, {"model", f.Model},
		{"provider", f.Provider}, {"result", f.Result},
	} {
		if kv.v != "" {
			parts = append(parts, kv.k+"="+kv.v)
		}
	}
	parts = append(parts, "generated "+time.Now().UTC().Format("2006-01-02 15:04:05")+"Z")
	return strings.Join(parts, "  |  ")
}
