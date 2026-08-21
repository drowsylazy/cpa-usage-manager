package web

import (
	"bytes"
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
	once      sync.Once
	assembled []byte
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
func ConsoleHTML() []byte {
	once.Do(func() { assembled = assemble() })
	return assembled
}
