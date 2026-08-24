package web

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"sync"
)

//go:embed console.html
var consoleHTML []byte

//go:embed console.css
var consoleCSS []byte

//go:embed console.js
var consoleJS []byte

var (
	gzOnce  sync.Once
	gzipped []byte
)

// assemble 把 CSS / JS 注入 HTML 壳的占位符，产出自包含的单文件面板。
// 占位符缺失时原样保留，便于在浏览器里直接调试源文件。
func assemble() []byte {
	b := bytes.ReplaceAll(consoleHTML, []byte("/*@console.css*/"), consoleCSS)
	return bytes.ReplaceAll(b, []byte("/*@console.js*/"), consoleJS)
}

// ConsoleHTML 返回不含数据的单一管理面板壳；所有数据只经管理 API 加载。
//
// 锁定决策：HTML 壳内不嵌入任何业务数据；登录密钥仅存 sessionStorage（当前会话）。
// 界面语言为简体中文（应需求取消多语言）。图表使用内联 SVG 渲染，无外部依赖，
// 保证 /console 在隔离环境亦可运行。
//
// 组装结果不常驻：现代客户端都走 ConsoleHTMLGzip 的预压缩路径，
// 这条明文路径只在极端环境被触发，不值得为此多驻留一份 ~224KB。
func ConsoleHTML() []byte { return assemble() }

// ConsoleHTMLGzip 返回预压缩的面板字节，供支持 gzip 的客户端直接输出。
// 压缩在首次调用时做一次并常驻；调用方须自行设置 Content-Encoding: gzip。
func ConsoleHTMLGzip() []byte {
	gzOnce.Do(func() {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(assemble())
		_ = zw.Close()
		gzipped = buf.Bytes()
	})
	return gzipped
}
