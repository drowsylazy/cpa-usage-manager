package web

import _ "embed"

//go:embed console.html
var consoleHTML []byte

// ConsoleHTML 返回不含数据的单一管理面板壳；所有数据只经管理 API 加载。
//
// 锁定决策：HTML 壳内不嵌入任何业务数据；登录密钥仅存 sessionStorage（当前会话）。
// 主题/语言跟随（简中/繁中/英文/俄文）。图表使用内联 canvas 渲染，无外部依赖，
// 保证 /console 在隔离环境亦可运行。
func ConsoleHTML() []byte { return consoleHTML }
