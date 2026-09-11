# AGENTS.md

## AI Agent 必读

仓库根目录的 **`MEMORY.md`** 是本项目的持久化记忆（架构决策、约定规则、踩坑教训）。任何 AI Agent 在本仓库工作前必须先通读该文件并**以它为准**；产出新的持久结论（架构决策、约定、教训）后应同步更新该文件。本文件只承载设计规范，不重复记忆内容；完整版本历史见 **`CHANGELOG.md`**。

## 项目状态

实施中（已发布 v0.0.1–v0.8.10）。`DESIGN.md` 是本插件的权威设计规范，所有实现必须遵守其中的锁定决策；当前 schema 版本 v18。**完整版本历史已迁至 `CHANGELOG.md`**（逐版本变更详情、踩坑与回归记录），本文件不再承载逐版本细节——发版时在 CHANGELOG.md 顶部新增条目，并同步更新本节版本号范围。

发布前需跑通全量回归：`go test ./...`、`scripts/smoke.go`、`scripts/abi-smoke.c`、`scripts/devserver.go`。历史版本沉淀的关键结论（CI 打包方式、配色校验门槛、面板实测流程、估算精度演进等）见 `MEMORY.md` 与 CHANGELOG.md 对应条目，遇到相关问题先查这两处。

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

- 推送后**不**监控 GitHub Actions workflow 与发布资产，由维护者自行确认
- 仓库文档与用户沟通使用**中文**。
- 空数据库默认自动建 `default` caller、全模型免费计价规则（`unknown_policy: allow`），但不自动签发任何 Key
- pepper 只在环境变量或 `data_dir/key-peppers`（0600），绝不入库/入日志/入 API 响应
- `data_dir`（0700）备份时须连同 `key-peppers` 一起备份
- 构建产物名必须是 `cpa-usage-manager.{so,dll,dylib}`；本机替换 DLL 后须重启宿主
- 敏感接口（签发/解密/备份/恢复/重置）响应须 `Cache-Control: no-store`
- 面板改动请用拟真数据实测，不要凭空调参：`go run scripts/seed.go` 注入种子（写默认 data_dir，可用 `CPA_DEV_DATA_DIR` 改；固定随机种子、可重复运行），再 `go run scripts/devserver.go` 开 <http://127.0.0.1:18080/console>（管理密钥 `dev-secret`）。`scripts/seed.go` 与 `scripts/devserver.go` 都是 `//go:build ignore` 的开发脚本，不参与构建
- 改图表系列色（`--series-1..4`）必须重跑配色校验（明度带 / 彩度下限 / 相邻 CVD ΔE≥8 / 常视觉 ΔE≥15 / 对比≥3:1），浅深两模式都要过；不要把图表系列色换回品牌语义色（实测两模式均不达标，见 CHANGELOG.md v0.3.2）