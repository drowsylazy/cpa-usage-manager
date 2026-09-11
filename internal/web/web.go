package web

import (
	"bytes"
	"compress/gzip"
	"embed"
	"sync"
)

//go:embed console.html
var consoleHTML []byte

//go:embed console.css
var consoleCSS []byte

//go:embed js
var jsFS embed.FS

// jsParts 是面板脚本的组成部分，按拼接顺序列出。脚本历史上是单文件
// console.js（单 IIFE），v0.8.11 起按既有分节标记拆分：文件名前缀即拼接
// 顺序，组件定义段（20-*）必须先于实例化与事件绑定段（30-* 及以后）——
// 顶层立即执行的代码撞上后段 const 的暂时性死区会让整个脚本加载即崩
// （v0.7.4 白屏实锤）。新增段落时在正确位置登记，web 测试会拒绝孤儿文件。
// scripts/js-tests.mjs 里有一份同步的清单（node 侧语法/行为测试用）。
var jsParts = []string{
	"00-head.js",       // 文件头注释 + IIFE 开启 + 'use strict'
	"10-utils.js",      // 工具 + 显示币种
	"20-components.js", // 下拉组件、会话与 API、主题、时间范围、弹层
	"30-overview.js",   // 页签调度、概览、趋势图
	"40-keys.js",       // 密钥、详情 dialog
	"50-usage.js",      // 用量
	"60-pricing.js",    // 价格、模型集合、规则干跑
	"70-system.js",     // 系统、实时、通知、定期报告
	"80-misc.js",       // 徽标、登录门、偏好同步、计价试算器
	"99-boot.js",       // 启动（组件实例化与首屏渲染收尾）+ IIFE 关闭
}

// assembleJS 按 jsParts 顺序拼接面板脚本。
func assembleJS() []byte {
	var buf bytes.Buffer
	for _, name := range jsParts {
		b, err := jsFS.ReadFile("js/" + name)
		if err != nil {
			// jsParts 与 embed 目录不一致是编程错误，panic 暴露（init 阶段
			// 首次调用即触发，不会带病上线）。
			panic("web: 缺少脚本段 " + name + ": " + err.Error())
		}
		buf.Write(b)
	}
	return buf.Bytes()
}

var (
	plainOnce sync.Once
	plain     []byte
	gzOnce    sync.Once
	gzipped   []byte
)

// assemble 把 CSS / JS 注入 HTML 壳的占位符，产出自包含的单文件面板。
// 占位符缺失时原样保留，便于在浏览器里直接调试源文件。
func assemble() []byte {
	b := bytes.ReplaceAll(consoleHTML, []byte("/*@console.css*/"), consoleCSS)
	return bytes.ReplaceAll(b, []byte("/*@console.js*/"), assembleJS())
}

// ConsoleJS 返回拼接后的面板脚本全文，供 scripts/assemble-js.go 等外部
// 工具（node --check、js 单测）消费。
func ConsoleJS() []byte { return assembleJS() }

// ConsoleHTML 返回不含数据的单一管理面板壳；所有数据只经管理 API 加载。
//
// 锁定决策：HTML 壳内不嵌入任何业务数据；登录密钥仅存 sessionStorage（当前会话）。
// 界面语言为简体中文（应需求取消多语言）。图表使用内联 SVG 渲染，无外部依赖，
// 保证 /console 在隔离环境亦可运行。
//
// 组装结果首次调用后常驻：构建期注入的内容进程内不变，缓存与 gzip 路径对称。
func ConsoleHTML() []byte {
	plainOnce.Do(func() { plain = assemble() })
	return plain
}

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
