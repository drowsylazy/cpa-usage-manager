//go:build !cgo

package main

// 非 cgo 环境仅用于运行 Go 单元测试；动态库发布必须启用 CGO_ENABLED=1。
func main() {}
