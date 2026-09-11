//go:build ignore

// 把面板脚本按 web.go 的 jsParts 顺序拼接到 stdout。
// 供 node --check 与 scripts/js-tests.mjs 消费；源文件已拆分为
// internal/web/js/*.js，仓库里不再有整份 console.js。
package main

import (
	"os"

	"github.com/drowsylazy/cpa-usage-manager/internal/web"
)

func main() {
	if _, err := os.Stdout.Write(web.ConsoleJS()); err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
}
