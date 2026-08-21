# AGENTS.md

## 项目状态

实施中（已发布 v0.0.1–v0.2.2）。`DESIGN.md` 是本插件的权威设计规范，所有实现必须遵守其中的锁定决策。仓库已有可构建代码：`internal/{config, service, store, money, usageparse, fx, httpapi, web}` 与入口 `main.go`（内联 C ABI）。请求路径实时额度强制（model_router/executor/request_interceptor/request_lifecycle_plugin）与被动统计模式均已在 `main.go` + `internal/service/requestpath.go` 实现；每次发布前需跑通全量回归（`go test ./...`、`scripts/smoke.go`、`scripts/abi-smoke.c`、`scripts/devserver.go`）。v0.0.1 Windows 构建失败原因：egor-tensin/setup-mingw 依赖的 chocolatey mingw 包安装脚本故障（libpthread.dll.a 缺失），已改为 CI 直接下载 winlibs 便携 MinGW（16.1.0posix-14.0.0-ucrt-r1）。v0.0.3 发布资产格式与宿主 pluginstore 不符（宿主要求 `<id>_<version>_<goos>_<goarch>.zip` + `checksums.txt`），v0.0.4 已按此规范重写打包。v0.1.0 管理面板完全重写为「仪表盘」风格三文件结构（console.html/css/js，构建期注入占位符合成单文件壳），界面语言应需求收敛为仅简体中文（取消四语言 i18n）；httpapi 的 /requests /trends /costs 经 parseFilter 统一支持 from/to/result 等过滤。v0.1.1：移除面板审计页；models.dev 计价改为搜索引入（10 分钟缓存）替代整本同步；宿主 usage.handle 作为权威用量口径回填零用量记录，TPS 缺失时自算；趋势图重做为堆叠柱状图（像素级尺寸 + 过密降采样）；延迟统一秒显示并拆首字/总延迟；顶栏工具组靠左避开宿主按钮区；深色模式跟随宿主主题。v0.1.2：新增计价清空（/pricing/reset，保留 glob:* 兜底）；修复宿主路由声明表漏 /pricing/search 导致面板搜索 404；趋势 tooltip 改顶部下挂修复不可见；柱状图间距收紧。v0.2.0：修复 quota 模式重复统计（usage.handle 的 APIKey 字段在部分兼容渠道是上游凭据，改为按 时间±15s+延迟±150ms+模型候选 关联执行器记录判重，命中仅回填缺失 token/首字延迟）；新增 store.RedactSource 清洗疑似上游凭据的 source 字段（写入+读取双端）；概览第四卡改缓存命中率（兼容 OpenAI cached_tokens 与 Claude cache_read 口径）；模型占比/密钥消耗支持 费用|Token|请求 指标切换；移除面板主题手动切换按钮（保留自动跟随宿主）。v0.2.1：修复双写竞态导致的重复统计——执行器结算与宿主 usage.handle 几乎同时落库，先查后插判重存在窗口；改为落库后对账（ReconcileRequestDuplicates：key_id 空否相反+模型候选+延迟±150ms+时间±15s 关联），保留执行器行、合并被动行零值字段并删除之，分钟聚合同步扣减/回补。v0.2.2：重复统计改为进程内「宿主用量认领」根治——宿主 usage.handle 不带请求 ID，任何库内启发式判重都会漏判/误判；执行器与拦截器在调用宿主上游前登记认领（归一化裸模型名匹配，同 kid 优先、否则 FIFO），命中的宿主回调交给该请求消费而不入库，库内判重（DedupeRequests/ReconcileRequestDuplicates）降级为跨进程与迟到回调的兜底；`host_usage_wait` 只用于流式兜底且改为「先关客户端流再等」（非流式不等待，宿主是在执行器返回之后才记账的）；宿主展示字段只在落库前合并（provider/source/auth_type/tier 属 usage_rollups 主键），落库后仅回填 token 与首字延迟；新增 `POST /dedupe` 与面板「对账去重」按钮，Maintain 每次自动按保留期对账；面板存储错误提示按真实成因分支（不再固定追加杀毒/同步盘那一句），系统页新增「存储重试」读数（`/health` 的 io_retries）；httpapi 路径改由 `route()` 登记并经 `Paths()` 与 `managementRegistration()` 交叉校验（拦住 v0.1.2 那类漏声明 404）；abi-smoke 不再卸载动态库（Go 运行时不支持卸载 c-shared，FreeLibrary 后偶发退出段错误，v0.2.1 及更早同样存在）。

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
- 仓库文档与用户沟通使用**中文**
- 空数据库默认自动建 `default` caller、全模型免费计价规则（`unknown_policy: allow`），但不自动签发任何 Key
- pepper 只在环境变量或 `data_dir/key-peppers`（0600），绝不入库/入日志/入 API 响应
- `data_dir`（0700）备份时须连同 `key-peppers` 一起备份
- 构建产物名必须是 `cpa-usage-manager.{so,dll,dylib}`；本机替换 DLL 后须重启宿主
- 敏感接口（签发/解密/备份/恢复/重置）响应须 `Cache-Control: no-store`