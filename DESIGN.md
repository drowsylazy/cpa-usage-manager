# cpa-usage-manager 设计文档

面向 CLIProxyAPI 的**全新插件**，吸取 `cap-token-usage-tracker`（token 用量统计）与 `credit-manager`（插件 Key 额度管理）两插件的能力与经验，**从头重新编写**为一个内聚的单一插件，不做代码合并，也不保留其「普通模式/完整模式」双页面与会话令牌体系。

- 插件 ID：`cpa-usage-manager`（宿主按动态库文件名派生 ID）
- Go module：`github.com/drowsylazy/cpa-usage-manager`
- 定位：**以插件 Key（`cum-...`）额度管理为核心**，单一管理面板（`/console`）展示用量/额度/价格/审计
- 默认行为：`quota.enabled=true`，接管前端鉴权；可关闭退回纯统计模式
- **无公开页面、无会话令牌**：全部数据仅通过宿主管理密钥访问

---

## 1. 能力清单

### 1.1 额度管理（核心）

- 签发/管理 `cum-<kid>-<secret>` 插件 Key：总/日/周/月额度（micro-USD）、最大并发、可用模型、标签、归属（caller）、禁用/启用/撤销/轮换/删除
- 请求前**严格预占**（保守 Token 上限 × 计价 → 原子扣占），余额不足 fail-closed 拒绝
- 按实际 usage **结算**（流式/非流式、多协议：OpenAI/Claude/Gemini/Responses 等），缺失 usage 有兜底策略
- 周期额度按 UTC 自然周期（日/周一起点周一/月）；并发按在途未结请求计数
- 明文仅签发时返回一次；数据库只存 HMAC 哈希 + 可恢复的 AES-GCM 密文 + pepper ID + 指纹
- pepper 体系（环境变量 / `key-peppers` 文件 / 自动生成，支持轮换）
- 审计事件、OAuth 认证额度快照 + 本地用量预测

### 1.2 用量统计

- 逐请求元数据 + 分钟级聚合（趋势、维度分组、请求明细）
- 维度：模型/提供方/来源/认证账号/认证类型/服务层级/推理强度/失败状态（认证字段做凭据清洗后保存）
- 指标：请求数、失败数、输入/输出/推理/缓存 Token、延迟、TTFT、生成时长、TPS、缓存命中率
- 时间范围：今天/近5小时/近7天/近30天/本月/自定义；趋势粒度 分钟/时/日/周/月 + 缩放平移
- 模型占比、费用趋势、模型效率、按 Key 筛选的用量视图
- 表格分页/排序/列偏好持久化、USD/CNY 汇率、全值/k/m 单位切换
- CSV/PNG 导出、数据库备份恢复、模型价格簿 + models.dev 同步、gzip 压缩、多语言/主题跟随

### 1.3 明确不做的事（简化边界）

- **无公开只读仪表盘**、**无 Key 自助查询页**：全部数据必须经宿主管理密钥
- **无「普通/完整模式」双前端**、**无 `X-Full-Mode-Session` 会话体系**：面板直接用宿主管理密钥调用管理接口
- **不存上游 API Key 密文**：不提供上游 Key 的明文回显/标签功能；只保存清洗后的认证账号展示信息用于分组

---

## 2. 总体架构

```
┌─ main package ──────────────────────────────────────────────────────┐
│  ABI 导出（cliproxy_plugin_init / PluginCall / Free / Shutdown）     │
│  handleMethod 调度：usage.handle / management.handle / auth / exec   │
├─────────────────────────────────────────────────────────────────────┤
│  internal/config   统一配置解析（内联 YAML + config_file + 环境变量） │
├─────────────────────────────────────────────────────────────────────┤
│  internal/service  核心服务层（运行时持有，可安全重配/热切换）          │
│   ├─ keys/pepper 签发·校验·加密        ├─ quota 预占/结算/审计        │
│   ├─ usage 记录入库 + 聚合             ├─ pricing 计价/规则/models.dev │
│   ├─ analytics 统计查询（趋势/维度/明细）└─ backup/restore/export      │
├─────────────────────────────────────────────────────────────────────┤
│  internal/store    单一 SQLite（WAL + 单写者 + 跨进程锁 + handover）  │
│  internal/money    整数 micro-USD 算术                               │
│  internal/usageparse  多协议 usage 解析                              │
│  internal/fx       USD/CNY 汇率                                       │
├─────────────────────────────────────────────────────────────────────┤
│  internal/httpapi  路由注册（management + resource）+ 管理密钥鉴权    │
├─────────────────────────────────────────────────────────────────────┤
│  internal/web     单一管理 SPA（嵌入 HTML）                           │
└─────────────────────────────────────────────────────────────────────┘
```

### 2.1 请求路径（quota.enabled=true）

```
客户端  Authorization: Bearer cum-…
  → frontend_auth.authenticate  校验 Key（pepper HMAC，常量时间比较）
  → model.route / executor.execute(_stream)
      预占（保守上限 × 计价，周期/并发额度原子校验，fail-closed）
      → host.model.execute(_stream) 透明转发（宿主跳过自身避免递归）
      → 解析 usage → 结算 + 写逐请求记录 + 写聚合 + 审计
```

### 2.2 被动统计路径（quota.enabled=false）

不注册鉴权/执行器能力；仅 `usage.handle` 接收宿主用量记录入库统计。

### 2.3 宿主用量认领（单一写入路径的实现）

`quota.enabled=true` 时，同一个真实请求会被两条链路看到：插件执行器（结算时写行）与宿主
`usage.handle` 回调（被动统计写行）。宿主回调载荷**不含请求 ID**，早期版本靠
「时间窗 + 延迟 + 模型」在库里猜关联，既会漏判（重复统计）也会误判（合并掉不同请求）。

现行做法是**进程内认领登记表（rendezvous）**：

```
executor.execute(_stream) / request_interceptor
  → registerUsageClaim(kid, 归一化模型名…)     登记一个认领位
  → host.model.execute(_stream)
usage.handle(record)
  → claimHostUsage(record)                     按模型名匹配（同 kid 优先，其次 FIFO）
      命中 → 交给该请求，**不写行**；若该请求已落库则回填 token/首字延迟
      未命中 → 走被动写入（跨进程/迟到回调的兜底），事务内先探测执行器行，
                命中即合并、不再插行（入库时防重）
结算
  → 落库前合并宿主展示字段（provider/auth_type/tier，属聚合主键，只能落库前写）
  → 落库后仅回填 token 与首字延迟
  → claim.release(8s 宽限)                     容忍宿主稍晚回调
```

写行的路径始终只有一条：认领命中即被动侧不写。双写残余竞态由**入库时防重**
消除——单写者串行化保证两侧事务一先一后提交，后者的事务内探测必然看到前者的行：
被动侧 `RecordPassiveUsage` 与执行器侧 `SettleReservation` 都在同一写事务内
探测并合并另一口径的行，任何时刻库内不可见重复行，事后对账因此取消。
`DedupeRequests` 仅在 Maintain 中按保留期批量清理历史遗留对。

### 2.4 读路径

唯一入口 `/console`（管理面板，HTML 壳不含数据）；所有数据经 `/v0/management/plugins/cpa-usage-manager/*`（宿主管理密钥鉴权）。无其他公开读取面。

---

## 3. 单一数据模型（SQLite）

单一数据库 `cpa-usage-manager.db`（默认 `<data_dir>/cpa-usage-manager.db`），迁移版本化（`schema_migrations`）。

### 3.1 核心表

| 表 | 关键列 | 说明 |
|---|---|---|
| `callers` | id, display_name, enabled | 归属记录（组织/团队），不承载额度 |
| `plugin_keys` | kid, key_hash, encrypted_material, pepper_id, fingerprint, principal, caller_scope, caller_id, label, enabled, revoked_at, expires_at, quota/daily/weekly/monthly_micro_usd, **token_limit / daily_token_limit / weekly_token_limit / monthly_token_limit**, max_concurrent_requests, allowed_models_json, 累计器（spent_micro_usd + \*_cycle_key + \*_spent_micro_usd、**tokens_used + \*_tokens_used**）, last_used_at | 签发策略与安全材料 |
| `pricing_rules` | id, match_kind(exact/glob/regexp), pattern, priority, enabled, price_input/output/cache_read/cache_creation（四档，见下方锁定决策）, accounting_mode, billing_mode, per_image_micro_usd, source(manual/models_dev), models_dev_id | 统一计价表，结算与展示共用 |
| `reservations` | id, key_id, caller_id, model, idempotency_key, status(held/settled/released), held/settled_micro_usd, reserved_tokens, expires_at, heartbeat_at | 预占与在途 |
| `requests` | id, key_id, caller_id, model, provider, source, auth_*, tier, result, ts, input/output/reasoning/cached/cache_read/cache_creation_tokens, total_tokens, latency_ms, ttft_ms, generation_ms, tps, thinking_intensity, cost_micro_usd, reservation_id | 逐请求记录 |
| `usage_rollups` | bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result, req_count, fail_count, in/out/reasoning/cached/cache_read/cache_creation_tokens, latency_sum, ttft_sum, generation_sum, tps_sum, cost_micro_usd | 分钟聚合，面板快速加载 |
| `audit_events` | ts, actor, action, entity_type, entity_id, detail_json | 审计 |
| `auth_quota_snapshots` | provider, auth_id, snapshot_json, fetched_at, status | OAuth 额度快照 |
| `auth_quota_window_baselines` | provider, auth_id, window_id, cycle_key, observed/baseline | 用量预测基线 |
| `preferences` | k, v | 面板偏好 |
| `meta` | k, v | 汇率缓存、models.dev 同步元数据 |

### 3.2 关键语义

- **金额**：整数 micro-USD，各 Token 类别费用向上取整后相加；只对 Input/Output/Cache Read/Cache Creation 计价
- **周期额度**：已结算 + 当期在途预占；日/周（周一）/月按 UTC；超额结算允许（余额可为负），后续请求 fail-closed
- **并发额度**：未结算/未释放的在途请求数，预占事务内原子校验
- **保留策略**：`requests` 与 `usage_rollups` 按 `retention_days` 清理；`plugin_keys`/`callers`/`pricing_rules`/`audit_events` 长期保留
- **认证字段**：来源/auth 字段做凭据清洗（疑似 Key/Bearer 的值不原样保存），只保留可用于分组的展示信息
- **备份/恢复**：整个数据库单文件备份；恢复需 `X-Confirm-Restore: replace`

### 3.3 旧数据迁移（不做）

**锁定决策：不提供从旧 credit-manager / tracker 导入数据的迁移工具。** 本插件是全新产品，不承诺与上游两插件的历史数据兼容。以下内容已明确排除在范围之外，后续任何阶段都不得实现：

- 不提供 `scripts/migrate_legacy.go` 或任何等价的旧数据导入脚本
- 不解析旧 credit-manager SQLite（keys/ledger/callers/pricing）
- 不解析旧 tracker bbolt（聚合/价格/偏好）
- 不迁移旧 pepper 文件，也不兼容旧 key_hash/密文格式
- 旧数据需用户自行按新库格式重建（重新签发 Key、重配计价、重新接入后自然积累用量）

> 注意：本节「不做迁移」仅针对**旧插件数据导入**。`schema_migrations` 的**版本化 schema 升级**属于存储层核心能力（见 §3.1 与 §9），始终保留。

---

## 4. 插件 ABI 与能力

单文件 `main.go` 内联实现 C ABI（沿用两插件已验证的 ABI 结构，无 cgo 构建标签）。

能力注册按 `quota.enabled` 动态计算（`reconfigure` 时重算）：

| 能力 | 默认（on） | quota.enabled=false |
|---|---|---|
| `usage_plugin` | ✔ | ✔ |
| `management_api` | ✔ | ✔ |
| `frontend_auth_provider` (exclusive) | ✔ | ✘ |
| `model_router` / `executor`（scope both，含 image/video 格式） | ✔ | ✘ |
| `request_interceptor` / `request_lifecycle_plugin` | ✔ | ✘ |

RPC schema 1–3 + 原生 ABI 1，schema 协商取 min(host, 自身)。

---

## 5. API 设计

统一前缀 `cpa-usage-manager`。**鉴权仅一层：所有数据接口走宿主管理密钥。**

- 管理路由 `/v0/management/plugins/cpa-usage-manager/*` → 宿主管理密钥
- 资源路由 `/v0/resource/plugins/cpa-usage-manager/*` → 仅 `/console`（SPA HTML 壳，不含数据）

### 5.1 管理路由

```
health                  GET   状态/版本
overview                GET   面板总览（keys/pricing/usage 摘要）
callers                 POST/GET  创建/列出归属
callers/enabled         POST  启停归属
keys                    POST  签发（明文仅此一次，Cache-Control: no-store）
keys                    GET   列出（不含明文）
keys/update             POST  更新标签/状态/额度/并发/可用模型
keys/rotate             POST  轮换（旧 Key 原子失效）
keys/revoke             POST  撤销
keys/reveal             POST  解密可恢复密文（no-store）
keys/delete             POST  永久删除（保留历史）
pricing                 POST/GET  新增/更新/列出计价规则
pricing/delete          POST  删除规则
pricing/sync            POST  models.dev 同步（优先级/忽略后缀/显式映射）
balance                 GET   查询 Key 剩余额度
usage                   GET   用量流水（按 key_id/model 过滤，分页）
usage/summary           GET   按 Key/模型汇总
requests                GET   逐请求明细（分页/排序/筛选）
trends                  GET   聚合趋势（按 分钟/时/日/周/月）
costs                   GET   费用统计与价格覆盖率
audit                   GET   审计事件
auth-quotas             GET   OAuth 认证额度快照 + 本地预测（no-store）
preferences             GET/POST  面板偏好
exchange-rate           GET   USD/CNY 汇率（缓存）
export/csv              POST  导出当前筛选为 CSV
export/png              POST  导出当前面板为 PNG
backup                  GET   下载数据库备份（≤64 MiB）
restore                 POST  分段上传恢复（X-Confirm-Restore: replace）
reset                   POST  重置统计（body {"confirm":"reset"}）
```

### 5.2 资源路由

```
/console   统一管理面板（SPA HTML 壳，侧栏菜单「CPA 用量管理」）
```

路由表在 `internal/httpapi` 集中声明 + 注册期校验唯一性。

---

### 5.3 双口径额度（金额 + Token）

每枚 Key 有两组互相独立的上限，**任一触顶即拒绝请求**：

| 口径 | 字段 | 单位 | 用途 |
|---|---|---|---|
| 金额 | `quota/daily/weekly/monthly_micro_usd` | 整数 micro-USD | 控制成本 |
| Token | `token_limit` / `daily/weekly/monthly_token_limit` | 整数 token | 控制用量 |

- 两者均为 `NULL` 表示该档不限；可只配一种
- **Token 口径 = 计费四类合计**（Input＋Output＋Cache Read＋Cache Creation，即 `Billable().Sum()`），与费用同一口径：inclusive/exclusive 已归一、`cached` 不重复计、推理已并入 Output
- **判定时机与金额对称**：预占期按估算 token 判定（`reservations.reserved_tokens`，在途预占计入用量），结算时按真实 token 回写累计器；上游未回用量时退回预占估算值
- Token 累计器**复用金额那套 `*_cycle_key`** 归零机制，并在同一条 `UPDATE` 内推进，保证两种口径的跨期点严格一致（不会出现金额已跨期而 token 未跨期）
- 存在的理由：混合模型下单价差可达数十倍（haiku vs opus），同一笔预算对应的 token 量相差极大，只用金额无法精确约束用量

---

## 6. 配置设计

统一 YAML：

```yaml
data_dir: ./data/cpa-usage-manager   # 库 + pepper 文件所在目录（0700）
database_file: cpa-usage-manager.db  # SQLite 文件名
busy_timeout: 5s
retention_days: 365                  # 逐请求/聚合保留天数（1-3650）

quota:                               # 额度子系统（默认开启）
  enabled: true
  keys:
    pepper_env: CPA_USAGE_MANAGER_KEY_PEPPERS
    pepper_file: key-peppers
    active_pepper_id: active
  limits:
    max_token_estimate: 1000000      # 单请求预占 Token 上限
    default_output_reserve: 4096     # 无 max_tokens 时的输出预占
    require_estimate: false
  settlement:
    missing_usage: settle_reserved   # settle_reserved | release
    host_usage_wait: 1500ms          # 流式兜底：关流后等宿主 usage 回调的窗口（非流式不等待）
  stream:
    stale_reservation_timeout: 2h    # 无心跳在途预占自动释放

pricing:
  unknown_policy: allow              # deny | allow | default
  models_dev_sync:
    enabled: true
    provider_priority: []            # 提供方优先级
    ignore_suffixes: []
    model_mappings: {}

response_compression: true
response_compression_min_bytes: 1024
```

- 配置注入：宿主 `plugins.items.cpa-usage-manager.config`（内联 YAML）→ 可选 `config_file` 或环境变量 `CPA_USAGE_MANAGER_CONFIG_FILE`；内联覆盖文件，禁止嵌套递归
- pepper 解析：环境变量优先 → 文件 → 首次启动自动生成（0600）

---

## 7. 前端设计（单一管理 SPA）

单文件 HTML（内联 CSS/JS + echarts CDN，无前端构建链），主题/语言跟随（简中/繁中/英文/俄文）。**登录宿主管理密钥后（sessionStorage，仅当前会话），全部数据调用 `/v0/management/plugins/cpa-usage-manager/*`。**

```
┌ 顶部：品牌 · 主题/语言 · 时间范围 · 刷新 · [登录管理密钥] ┐
├ 页签（路由 hash）─────────────────────────────────────────┤
│ 概览      总 token/请求/费用（按 Key、模型、时间筛选）；     │
│           趋势图（缩放平移）、模型占比、效率指标            │
│ 密钥      签发 cum- Key（额度/并发/模型/标签/caller）；     │
│           列表（余额/周期用量/并发占用/状态）筛选分页排序；  │
│           更新/禁用/撤销/轮换/删除/解密查看                │
│ 用量      维度表（模型/提供方/来源/认证账号/结果）、         │
│           逐请求明细（分页/列偏好）、费用与价格覆盖率       │
│ 价格      计价规则管理（exact/glob/regexp+优先级）+          │
│           models.dev 同步 + 汇率                           │
│ 认证额度  OAuth 快照 + 本地预测（no-store）                 │
│ 审计      事件流                                           │
│ 系统      备份/恢复/重置统计/导出 CSV/PNG                  │
└───────────────────────────────────────────────────────────┘
```

- 未登录时仅显示登录界面，页面壳不含任何数据
- 所有敏感操作（签发/解密/备份/恢复/重置）均为管理密钥鉴权，响应 `Cache-Control: no-store`
- 无公开页、无自助页、无会话令牌

---

## 8. 构建与发布

| 项 | 说明 |
|---|---|
| Go | 1.26.x，`CGO_ENABLED=1`（SQLite 用 modernc 纯 Go，CGO 仅用于 c-shared） |
| 平台 | Linux amd64/arm64、Windows amd64、macOS arm64 |
| 构建 | `go build -buildmode=c-shared -trimpath -buildvcs=false -ldflags="-s -w -X main.version=<ver>" -o cpa-usage-manager.{so,dll,dylib} .` |
| 脚本 | `scripts/build.ps1`/`build.sh`/`deploy.ps1`（部署到 `plugins/<goos>/<goarch>/`） |
| CI | `release.yml` 五平台矩阵，`v*` 标签正式版 / 分支推送 alpha；产物 `cpa-usage-manager_<ver>_<goos>_<goarch>.zip` + `checksums.txt` |
| 本地验证 | `gofmt -w .`、`go vet ./...`、`go test ./...`、`go run ./scripts/smoke.go`、`scripts/abi-smoke.c` |

---

## 9. 测试策略

| 层 | 用例 |
|---|---|
| store | 版本化 schema 迁移（非旧数据导入）、CRUD、配额原子性、单写者锁 + handover、保留清理 |
| service | 签发/鉴权/轮换/撤销、预占边界（周期/并发/模型）、结算（各协议解析、缺失 usage 兜底）、审计 |
| pricing | 规则匹配优先级、micro-USD 取整、accounting/billing 模式、models.dev 同步合并语义 |
| analytics | 聚合正确性、趋势下采样、维度分组、逐请求分页排序 |
| httpapi | 路由表唯一性、管理密钥鉴权矩阵、gzip、备份恢复、no-store 语义 |
| web | `/console` 可达、主题/语言、关键交互冒烟 |
| e2e | `smoke.go`：签发→鉴权→预占→结算→统计入库→审计 全链路；`abi-smoke.c` 校验导出符号 |

---

## 10. 实施阶段

| 阶段 | 内容 | 出口标准 |
|---|---|---|
| P0 | 模块初始化、目录骨架、单一 SQLite schema + 迁移、config、money、fx、usageparse | store/config/money 测试绿 |
| P1 | keys/pepper 服务 + quota 预占/结算 + 审计 + pricing 规则 | service 层测试绿（smoke.go 验证） |
| P2 | usage 入库（逐请求 + 聚合 + 账本一次写入）+ analytics 查询 | 统计查询正确性测试绿 |
| P3 | main.go ABI + 能力注册 + httpapi 全部路由 + 管理密钥鉴权 | 路由/鉴权测试绿，可被宿主加载注册 |
| P4 | 单一管理 SPA（/console） | 手工冒烟 + web 测试 |
| P5 | 构建脚本、CI、README、四平台产物 | 四平台可构建，CI 通过 |

评审本设计后从 P0 开始。实施时不搬运两插件源码文件，按本设计重新编写；两仓库实现细节仅作参考（已克隆在本地临时目录）。

---

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 重写工作量大、回归风险 | 用两插件现有测试用例作为行为基准逐项移植为新测试 |
| 单一 SQLite 承载高吞吐流式结算 + 聚合 | 单写者 + WAL + 批量 flush + 分钟聚合与逐请求分离，聚合查询不阻塞写入 |
| 无公开/自助读取面可能影响部分使用场景 | 明确为设计决策；所有查询统一经管理面板 |
| 计价模型统一引入行为差异 | 结算与展示共用一张表、一套取整规则；不提供旧数据迁移，历史账本一致性由用户自行重建 |
| 独占鉴权影响存量宿主 | 文档明示；`quota.enabled=false` 提供纯统计模式 |
| 前端单文件体积 | 沿用两插件做法，echarts CDN + 多语言按需内联 |

---

## 12. 模型路由（集合别名 + 规则语言，v0.5.0）

### 12.1 数据与语言

- SQLite `model_routes` 表（schema v8）：`alias` NOCASE 唯一、`rule` 脚本文本、`cooldown_seconds`（默认 60）、`pricing_mode`（target|alias）、`enabled`。写后回调失效服务层内存快照（atomic.Pointer + 60s TTL 兜底跨进程改写）。
- 规则脚本由 `internal/routelang` 解释（纯 Go 手写 lexer/parser/eval，零依赖）：`when <条件> -> <候选链>` 分支式；候选链构造器 `"模型"` / `priority [...]`（声明序即回退链）/ `weighted {...}`（加权随机，选中者排首其余按权重降序跟随）；无条件兜底分支必填且只能是最后一条；无循环无赋值 ⇒ 求值天然终止。语法错误带行列号供面板定位。

### 12.2 运行时行为

```
执行器入口：MatchRoute(剥思考后缀 + EqualFold) → BuildRouteEnv → ResolveChain
ResolveChain = Eval（ai_judge 失败自动回落兜底分支并记审计）→ 冷却过滤（保序摘除）
全冷却 → upstream_error 信封；命中 → 预占一次 + 登记认领（别名+全部引用目标含后缀形态的超集）
逐目标尝试：成功/终局失败 → 单行结算（model=别名, upstream_model=成功目标）
            可转移失败 → MarkRouteFail + 审计 route.failover → 换下一目标
流式仅在首字节前可切换（dialHostStream 窗口）；routeFailureEligible 判定可转移性
```

- **可转移失败**：401/402/403/408/429、5xx、连接类文本错误；404 除 Responses 存储引用（previous_response_id 等）外可转；400/422、context 取消/超时、emit 失败不可转。
- **计价**：`target` 模式预占按首选目标规则（PricingOverride），结算按实际成功目标（剥后缀）匹配；`alias` 模式全程按别名声价。维度统计恒记别名。
- **认领防双计**：宿主 usage.handle 上报的目标真名（可能带思考后缀）命中认领则并入业务请求行；judge 调用不登记认领，自然被动入账到评判模型名下。

### 12.3 ai_judge

- 评判模型/超时存偏好 KV（`routing_judge_model` / `routing_judge_timeout_ms`，默认 8000ms，500~120000 校验）。
- 发送脱敏摘要：结构化指标 + 对话文本前 2000 字符（messages/contents/system/input 容器内 content/text 字段，map 键排序保证确定性）；绝不发送原始 body 全文。
- 进程内 LRU 缓存（512 条 × 10min TTL，key=judge_model+变量快照+options 的 SHA-256）+ single-flight 合并同 key 并发。
- 失败/超时/越界输出 → 整条规则回落无条件兜底分支 + 审计 `route.ai_fallback`；保存期强制：使用 ai_judge 的规则必须已配置评判模型。
- judge 调用经 main.configure 注入的钩子走 `host.model.execute`（服务层不直接持有 C ABI 回调）。

### 12.4 model_registrar

- 能力位 `model_registrar`（quota.enabled 且存在启用路由时声明）；dispatch 新增 `model.register` 方法返回 `{provider, models}`，ModelInfo 字段 PascalCase 无 tag 与宿主对齐（ID/Name/DisplayName=别名原文、Object=model、OwnedBy=cpa-usage-manager、Description=`集合别名 · N 个目标`、UserDefined=true）。旧宿主忽略能力位，静默降级。

### 12.5 已知限制

- 冷却进程内保存：reconfigure/重启丢失、多实例各自独立——failover 链本身兜底。
- `input_tokens` 为 len(body)/2+1 封顶粗估（CJK 偏低 1.5~2x），阈值条件需留余量。
- ai_judge 同步阻塞热路径 ≤ timeout_ms；摘要文本发往 judge 目标模型（隐私边界见 README）。
- 别名流量仅面向插件 Key（cum-/caller_scope）；原生 Key 打别名走宿主原生路径报未知模型，预期共存行为。
- 引用模型禁命中任何启用别名（含自引用）：路由不嵌套，编译期校验拒绝。