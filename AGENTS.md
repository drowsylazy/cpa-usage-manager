# AGENTS.md

## 项目状态

实施中（v0.0.1 尚未发布）。`DESIGN.md` 是本插件的权威设计规范，所有实现必须遵守其中的锁定决策。仓库已有可构建代码：`internal/{config, service, store, money, usageparse, fx, httpapi, web}` 与入口 `main.go`（内联 C ABI）。请求路径实时额度强制（model_router/executor/request_interceptor/request_lifecycle_plugin）与被动统计模式均已在 `main.go` + `internal/service/requestpath.go` 实现；发布 v0.0.1 前需跑通全量回归（`go test ./...`、`scripts/smoke.go`、`scripts/abi-smoke.c`、`scripts/devserver.go`）。

## 这是什么

面向 CLIProxyAPI 的 **Go `c-shared` 插件**，插件 ID：`cpa-usage-manager`（宿主按动态库文件名派生 ID）。定位：插件 Key（`cum-...`）额度管理为核心 + 单一管理面板展示用量/额度/审计。

- Go module：`github.com/drowsylazy/cpa-usage-manager`
- Go 1.26+，`CGO_ENABLED=1` 才能产出 `.so`/`.dll`/`.dylib`
- 这是**重写**，不是合并：不搬运两插件源码，上游仓库只作实现参考与测试基准
  - 参考源：`AITNR/cap-token-usage-tracker`（统计）、`yuluo688/credit-manager`（Key 额度）

## 已锁定的设计决策（不得违反）

- **存储**：单一 SQLite（`modernc.org/sqlite`，纯 Go），不用 bbolt；单写者 + WAL + 跨进程锁 + handover 租约
- **Key 格式**：`cum-<kid>-<secret>`（不是 `tk-`）；明文仅签发时返回一次，库中只存 HMAC 哈希 + AES-GCM 密文 + pepper
- **金额**：整数 micro-USD，无浮点；只对 Input/Output/Cache Read/Cache Creation 计价，各类别向上取整后相加
- **默认行为**：`quota.enabled=true` 接管前端鉴权；置 `false` 退回纯统计（被动 usage 记录）
- **usage 写入**：单一路径一次入库（逐请求记录 + 分钟聚合 + 账本 + 审计），无双写
- **无公开/自助页面、无会话令牌**：唯一读取面是 `/console` 管理面板，全部数据经宿主管理密钥鉴权的 `/v0/management/plugins/cpa-usage-manager/*`；HTML 壳不含数据；不保留 tracker 的「普通/完整模式」双前端与 `X-Full-Mode-Session`
- **不存上游 API Key 密文**：认证字段只保存清洗后的展示信息，不提供上游 Key 明文回显/标签
- 计价统一为一张 `pricing_rules` 表（match_kind exact/glob/regexp + priority），同时服务额度结算与面板费用展示

## 架构分层

`internal/{config, service, store, money, usageparse, fx, httpapi, web}`，入口为 `main.go`（内联 C ABI，无 cgo 构建标签）。

## 约定

- 仓库文档与用户沟通使用**中文**
- 空数据库默认自动建 `default` caller、全模型免费计价规则（`unknown_policy: allow`），但不自动签发任何 Key
- pepper 只在环境变量或 `data_dir/key-peppers`（0600），绝不入库/入日志/入 API 响应
- `data_dir`（0700）备份时须连同 `key-peppers` 一起备份
- 构建产物名必须是 `cpa-usage-manager.{so,dll,dylib}`；本机替换 DLL 后须重启宿主
- 敏感接口（签发/解密/备份/恢复/重置）响应须 `Cache-Control: no-store`