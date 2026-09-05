# 项目记忆：cpa-usage-manager

> **本文件是仓库内权威项目记忆（2026-02 迁入）**：任何 AI Agent 在本仓库工作前必须先通读本文件并以它为准；产出新的持久结论（架构决策、约定规则、踩坑教训）后应同步更新本文件。AGENTS.md 对此有明确要求。
>
> 仓库：F:\cpa-usage-manager（CLIProxyAPI 的 Go c-shared 插件）。权威设计规范是 DESIGN.md，版本历史在 AGENTS.md 第 5 节。

## Rules

- 发版流程（**v0.6.2 起改为双提交**，用户明确指示）：①代码提交在前，message 不带版本号（前缀按变更类型 `feat:` / `fix:` / `perf:` 等）；②发版单独一个提交，message 固定为 `release: vX.Y.Z`（内容 = registry.json 版本号 + AGENTS.md 版本历史/范围 + 本流程规则），打 `vX.Y.Z` 标签推送 → CI 自动构建四平台并创建 GitHub Release。推送后**不**监控 CI（仓库约定）。
- 仓库文档与用户沟通使用中文。
- **审计功能已整体弃用（2026-08 用户明确指示）**：本项目不需要审计功能。不再新增任何审计向功能（审计面板页、audit 导出/归档/保留期清理等一律不做）；存量 `audit_events` 表仅承载内部机制的既有留痕（route.ai_fallback / route.failover / 密钥操作回执等），维持现状，不在其上扩展。
- **UI 改动禁止跑 playwright/浏览器自动化验证**（用户两次明确叫停：先投诉链式调用卡死，后直接要求「停止做这种测试」）。交付前只做 `$env:GOROOT='D:\Go'; go build ./...` + `node --check internal/web/console.js`，页面效果交用户自己打开目验（需要时提示 `go run scripts/devserver.go` → 127.0.0.1:18080/console，密钥 dev-secret）。写新交互代码时要静态自查事件绑定是否覆盖所有容器（教训：复制委托只挂了 #key-rows，抽屉里的同款按钮成死键）、flex 容器的滚动区要显式 flex:1+min-height:0（教训：抽屉底栏不贴底）。

## Architecture decisions

- 宿主（CLIProxyAPI）对插件发起的嵌套执行（host.model.execute_stream）**故意不上报用量**：内层 `InternalSource=true` 不建 reporter，外层被 nestedTracker 抑制。插件执行器路径的 token 统计只能靠插件自己解析流。v0.2.4 已修复：`Accumulator.FeedChunk` 兼容裸 JSON（宿主 openai→openai 直通翻译会剥掉 `data:` 前缀并丢弃 `[DONE]`）。已在生产验证生效。
- 被动入库（usage.handle 兜底）模型名必须优先用宿主上报的 `Alias`（用户配置名），`Model` 是上游实际路由名（如 OpenRouter 回报 `stealth/ox-alpha`）。v0.2.3 修复。
- 面板 Token 显示用 `fmtTok`（K/M/B 自动升级，阈值取 999.5 倍数避免 1000K）；概览「总消耗 Token」主值完整显示精确到个位。非 token 数字仍用 `fmtInt`（万/亿）。

## Discovered durable knowledge

- **2026-09 大批改动（修复+优化+功能批，schema v14）**：
  - **双写判重谓词重写（根因修复：请求数波动）**：用户实锤「挂着不动请求数自己变少」。根因（已对照宿主源码核实）：①宿主 usage 回调走**单 worker 后台队列**（`Publish` 只入队、goroutine 逐条 dispatch）——队列积压时回调晚于请求完成很久，超 8s 认领宽限即 miss；②宿主 `RequestedAt` 是请求开始时刻（reporter 构造时快照）、`Latency` 从同一时刻懒计算**含宿主全链路开销**（鉴权/翻译/重试），与执行器观测的纯上游延迟差可达数秒——旧谓词「延迟±150ms」必然漏判，被动行既没被认领吸收也没被入库探测合并，残留重复行由对账删掉时表现为请求数下降。修复：`duplicateProbeTx`/`dedupePairSQL` 改**结构化区间谓词**——被动行 ts 必须落在执行器请求区间 [e.ts−15s, e.ts+e.latency+30s] 内 + **完成时刻对齐**（两侧 ts+latency 差 ≤10s，宿主侧另加 30s 队列尾延迟）+ **token 相容过滤**（两侧计数差 ≤64，任一侧 0 不约束——同请求两侧计数差只来自口径归一；这是同模型并发下区分不同请求的关键信号）。PassiveDedupeHint/FindDuplicateExecutor 签名追加了 TotalTokens/InputTokens。**勿改回窗口对称比较**。测试钉：TestProbeMatchesHostFullChainLatency / TestProbeRejectsDifferentConcurrentRequests / TestDedupeRequestsFullChainLatencyPair。
  - **schema v14 失败原因留痕**：requests 加 `status_code` + `error_note`（SanitizeErrorNote 清洗截断 200 字符，RedactSource 同款防上游错误信息回显凭据）；执行器 buildRequest 记 status，路由 failover 轨迹（`failoverTrail`，上限 4 跳）与 ai_judge 回落错误（`RouteMatch.AIFallbackErr`，**ResolveChain 改收 *RouteMatch 按指针写回**）都并入结算行 error_note；请求明细加「原因」低频列（列偏好），CSV 加 status_code/error_note 两列。**route.failover / route.ai_fallback 审计已退役**（请求路径高频写 + audit_events 长期保留的组合不可持续）；`audit_retention_days` 配置（默认 90，0=跟随 retention_days）接入 ApplyRetention。
  - **汇率自动刷新**：`fxRefreshLoop`（main.go，30min 间隔 + 启动即刷，仅租约持有者 Writable() 才打上游；reconfigure/shutdown 经 runtimeState.fxStop 收口）。此前纯懒加载——面板没人打开缓存就陈旧到底。
  - **CI**：`.github/workflows/ci.yml`（push/PR 跑 gofmt 检查 + go vet + node --check + go test）；release.yml 的四平台重复测试只在 linux-amd64 保留即可（未改，后续可收敛）。
  - **backup.max_bytes 可配置**（默认 256MiB，BackupConfig.MaxBytesOrDefault；httpapi 恢复 LimitReader 经 svc.BackupMaxLimit() 同源）——旧 64MiB 硬上限在默认 365 天保留期 + 高频部署下必炸且恰在最需要备份时。
  - **可观测性批**：`GET /reservations/held`（进行中请求视图，系统页宽幅面板，held 行含 stale 标记）；`GET /model-routes/health`（目标健康：进程内冷却 + 近 60 分钟失败统计——**usage_rollups 没有 upstream_model 列**，按 model 列即别名聚合；RoutesHealth 必须走 ListRoutesCompiled 因为 Refs 只在编译后填充）；通知加**错误率告警**（ErrorRateAlert/WindowMin/Pct，全局状态行 `__global__` 不随 Key 存亡清理）与**密钥临期预警**（ExpireWarnDays，days 用四舍五入）；`/health` 的 Stats 加 wal_bytes；定期报告汇总板块加环比（上期无记录省略）。
  - **计价试算器**（纯前端）：价格页「试算」按钮 → openSheet，BigInt 与服务端 costForRule 同口径逐档向上取整；onOk 返回 false 保持弹窗打开反复调参。**教训：console.js 是单 IIFE，追加代码必须插在「启动」段之前**（IIFE 内闭包变量才可达）——追加到 `})();` 之后监听器绑定时 ReferenceError 被吞、点击无反应且无报错，浏览器实测才暴露。
  - **面板实测环境坑**：devserver 是 `go run`——改前端后必须杀掉 18080 端口进程重启才吐新 HTML（`Get-NetTCPConnection -LocalPort 18080` 找 PID）；浏览器 reload 会吃 HTML 缓存，带 `?v=N` 强制拉新。测试库用 `CPA_DEV_DATA_DIR` 隔离。

- **Source 字段泄露上游 API Key（2026-08 修复，用户实测导出实锤）**：宿主 `resolveUsageSource`（usage_helpers.go:412，已对照宿主源码）在 auth 无邮箱/账号信息时**回落把上游 api_key 原样填进 usage Record.Source**——插件侧任何直接外显 Source 的路径都是泄露面。修复分四层：①`RedactSource` 启发式强化——原版只拦 `sk-` 前缀与 ≥32 位纯字母数字，带 `-_.` 分隔符或较短的 Key 全漏；现改为**任一空白分隔 token** 长度 ≥20、字符集仅 `[A-Za-z0-9._-]`、且同时含字母与数字即判凭据整串清空（邮箱有 @、URL 有 / 被字符集排除，正常保留）；②store 写入收口——`insertRequestTx`（requests 唯一 INSERT 点）/`upsertRollupTx`/`fillKeeperZeroTx` 三处自带清洗，任何未来调用方不可能绕过；③执行器 `buildRequest` 对 SourceFormat 也清洗；④`GroupByDimension` 的 source 维度读侧清洗（历史脏 rollup 行随保留期老化，不做 DB 迁移，与 TPS 脏数据同一策略）。测试钉：redact_test.go 新规则表、TestRedactSourceAtWrite（写入收口）、TestGroupByDimensionSourceRedacted（维度读侧）。**启发式残余风险**：无分隔符且不含数字的短 Key（<20 位）识别不了；带数字的合法长标签（如 openai-compatible-v2）会被误杀，属「宁可误杀」的有意取舍。

- **console.js 顶层执行顺序 TDZ 陷阱（v0.7.3 白屏实锤，用户浏览器复现）**：`const dispCurSel = new Select(...)` 写在 fmtMoney 附近（76 行），而 `Select`/`CARET`/`savePref` 定义在后面（142/452 行）——顶层代码立即执行时 const 处于暂时性死区，ReferenceError 让整个脚本加载即崩，页面只剩背景。`node --check` 只查语法查不出这种错；gofmt 同理。**规则：所有组件实例化（new Select/Combo）与 DOM 事件绑定必须放在「页签调度」段之前（组件定义全部就绪之后），顶层只放纯数据初始化。** 修复后用 devserver + 浏览器实测登录前后渲染正常（用户明确要求时可用浏览器验证，不受「禁止 playwright」默认规则约束）。

- **2026-08 数据表精简（schema v13）**：
  - **requests 列清单单一来源**：`store.RequestColumns` + `store.ScanRequest` 导出，service 层禁止手抄副本——曾两次实锤副本漂移：service 内联清单漏 currency/cost_native（v12 引入即丢字段），store 的 requestColumns 竟一直缺 `upstream_model`（v5 加列只改了 service 副本，GetRequest 从未返回过 UpstreamModel）。改列必须只动 requestColumns+scanRequest 一处。
  - **认证额度功能整体移除（v13 DROP）**：auth_quota_snapshots / auth_quota_window_baselines 自 v0.1 起无任何写入方（宿主无推送回调），页签永远为空。已删 store/authquota.go、service.AuthQuotas、/auth-quotas 端点与双注册、面板 tab-auth/view-auth/loader 与 auth-* CSS；snapshotTables 同步移除（旧备份含这两表会被 schema 版本强校验拒绝，属既有约定）。设计文档曾把「OAuth 认证额度快照」列为 P0 特性但从未接线——以后接类似宿主推送特性时，先确认宿主侧真的有回调再建表。

- **2026-08 请求原生币种入账 + 顶栏改进（schema v12，用户明确要求）**：
  - **requests 行原生币种入账**：v12 加 `currency` + `cost_native_micro`（CNY 规则存 micro-CNY 原生金额，这是权威入账记录）；`cost_micro_usd` 保留为按规则**锁定汇率**折算的美元等值——额度扣减（plugin_keys 累计器与限额恒 USD）与跨币种聚合（rollups）必须单一口径，¥/$ 无法直接相加，这条不能按用户直觉去掉。costForRule 改返回 (usd, native)；`PriceNative` 供被动路径（main.go 两处 usage.handle 分支）取币种。scanRequest 对 USD 行 CostNativeMicro==0 时回填 = CostMicroUSD（兼容旧行）。
  - **CSV 导出契约**：requests CSV 头变为 `cost, currency, cost_usd, priced`（cost=原生金额，currency 指明币种，cost_usd=美元等值）。
  - **顶栏显示币种**改为 .sel 选择框（disp-cur，带「显示币种」文字标签 + title 说明「仅影响显示，不影响额度与计价口径」）；请求明细 CNY 行费用显示 ¥ 原生金额（title 给美元等值），详情弹窗加「计价币种」fact。
  - **顶栏自动刷新**：bar-tools 里 checkbox + 秒数输入（5..86400，偏好 ui_auto-refresh / ui_auto-refresh-secs），按 interval 调 reloadActive()（document.hidden / app 未登录时跳过）；**替代并移除**了请求明细面板的固定 30s 开关（ui_req-auto 偏新作废）。

- **2026-08 计价币种与构建结论**：
  - **计价币种（schema v11）**：pricing_rules 加 `currency`（USD/CNY）+ `fx_rate_milli`。价格四档与 PerImageMicroUSD 以**规则币种的 micro 单位**存储（CNY 规则即 micro-CNY/百万 token），结算时 `usdCost()` 按**保存时锁定**的汇率整笔 ceil 折算成 micro-USD——账本恒为 USD、不随行情漂移，改价需重存规则。httpapi 对 CNY 且未显式给汇率时用 `svc.ExchangeRate`（永不失败，兜底 7.2）锁定。normalizeCurrency 钳 rate_milli 到 500..50000。models.dev 同步强制 USD/1000。恢复走 `INSERT..SELECT *`，schema 版本强校验意味着旧版本备份不可直接恢复（既有约定）。
  - **面板显示币种**：`disp-cur` 偏好（ui_ 前缀同步），tabs 栏按钮切换；`fmtCur()` 包一层（cny = microUSD×rate 取整后 fmtMoney ¥），仅换展示位（读数/趋势/维度/密钥卡/明细/详情/余额清单），表单回填与限额输入恒 USD；汇率未就绪回退 USD。原「≈ ¥」附注在 cny 模式下跳过避免重复。
  - **构建体积实测量级（Go 1.27, c-shared DLL ≈16.7MB）**：`-s -w` 后无符号可查；符号级构成 runtime≈5.8MB + modernc/sqlite≈2.2MB+rodata + shoutrrr 等 github 依赖 1.5MB + type/funcdata 元数据≈2MB，无单一可剔除死重。已测无效：extldflags=-s、osusergo/netgo、-buildid=（KB 级）。采纳：zip -9（Windows 用 .NET ZipArchive Optimal，Compress-Archive 档位不可控）、去除 google/uuid 依赖（service.NewUUID，crypto/rand 16B hex）→ −9KB。UPX（AV 误报+c-shared 风险）与换 mattn/go-sqlite3（重写单写者语义）评估后否决。

- **2026-08 功能批·第二轮持久结论**（8 提交，schema v9+v10）：
  - 冷却是进程内状态器（routes.go cooldowns map）：新增 `MarkRouteSuccess`（流式在 dialOK 时清、非流式在成功结算前清）；429 的 Retry-After（秒数或 HTTP 日期）经 `cooldownSecondsFor` 采用并钳到 1s~10min。ResolveChain 全冷却分支按 `cooldown_policy`：`force` 返回原链照打、`block`（默认）维持 ErrAllTargetsCooling。
  - 单请求异常告警：NotifySettings 新增 single_cost_alert/single_cost_micro_usd/single_token_threshold 三字段；Settle 尾部 `maybeNotifySingleUsage`（设置走 notifySettingsCached 60s TTL 缓存，热路径仅两次比较），命中后 goroutine 异步发送，同 Key 1h 冷却（preferences `notify_single_state`）。httpapi notifySettings 的入参结构体是显式字段清单，加设置字段必须同步补。
  - 周期额度时区：`quota.cycle_offset_minutes`（config 归一 -720..840）→ store 包级 atomic `SetCycleOffsetMinutes`（main.configure 调用）→ `cycleTime` 先加偏移再按 UTC 取年月日；CycleStart 返回真实时刻需减回偏移。cycle_key 是字符串，切偏移后旧键自然滚动归零，无迁移。
  - 请求次数限额（日/月，schema v9 四列）：独立第三族，与金额/Token 互斥规则无关；结算 UPDATE 内复用 daily/monthly_cycle_key 归零推进（CASE WHEN 换周期则置 1）；预占期在途计入走 HoldReservation 聚合里 created_at>=周期起点的 COUNT。Balance JSON 契约新增 daily/monthly_remaining_requests。
  - routelang 新增布尔字面量 true/false 与变量 hour/weekday(ISO 1=周一..7=周日，按 cycle_offset 本地日历)/has_tools/has_system（顶层 tools/system/systemInstruction RawMessage 存在性探测，OpenAI messages 内的 system role 不探测）/kid/key_label/caller_id；BuildRouteEnv 签名追加了 `key *store.PluginKey`（干跑传 nil）。
  - 自动备份：`backup.enabled/dir/keep/hour`（默认关/backups/7/4 点本地时刻，启动时当天已过点补一份）；service.RunAutoBackup 临时文件+rename 原子落盘、时间戳命名轮转；main.autoBackupLoop 仅租约持有者（Writable）执行，失败经 NotifyErrorEvent 上报；快照不含 key-peppers，须自行另行备份。
  - 新增 GET /keys/candidates（kid+label 轻量全量候选，上限 2000），请求明细密钥联想不再受 /keys 分页限制；面板请求明细「30s 刷新」toggle 偏好走 ui_req-auto。

- **2026-08 功能批持久结论**（154483f）：
  - **HTTP 响应头必须先于首写设置**（实测实锤：导出端点原「先写体再设头」的顺序在真实 HTTP 下 Content-Type/Content-Disposition 全被首写快照丢弃，浏览器拿不到文件名）。导出类处理器一律先用 `svc.ExportTarget(req, png)` 拿文件名+Content-Type 预置头再写体；文件名由 `exportFileName` 统一构造（kind+时间范围标记 `_YYYYMMDD-HHMM[_-终点]|_all`+UTC 时间戳）。中途出错头已提交无法回改成 JSON，只能记日志。
  - 面板偏好跨设备同步约定：本地键经 `savePref` 双写 localStorage 与 /preferences 的 **`ui_` 前缀键**（勿与 notify_* 冲突）；登录后 `syncPrefsFromServer` 以服务器为准回灌本地并整页重载一次（sessionStorage `ui-prefs-reloaded` 防循环；custom 时间范围无起止不同步）。MultiSelect 等 closure 组件没有程序化设值接口，重载是有意的最稳方案。
  - **未计价模型清单（unpriced_models）已整体移除（2026-08 用户裁决「太丑」后先改版仍否决）**：/costs 不再返回 unpriced_models，面板计价覆盖卡只剩四项统计；勿再往该卡内嵌清单/排行类组件——用户对窄卡里塞列表的形态接受度差。
  - 触顶预估 `keyEtaText`：日速率=今日已用/今日已过比例（UTC），档位选取与 usdPick/tokPick 同口径；今日无消耗或已过 <5% 不外推。只用于详情弹窗 kd-note，卡片布局不动。

- **2026-08 修复批的持久结论**（三提交 65a32b0/cabe08e/d45353e，均带测试钉）：
  - `ResolveChain` 在 ai_judge 失败时返回（兜底链, true, AI错误）——**链与错误非互斥**，main 层判失败必须用 `cerr != nil && chain == nil`，否则回落语义被破坏（首轮实锤 bug）。测试 TestResolveRoutingAIFallback。
  - `usageClaim` 有 `released` 标志（c.mu 内先置位再摘桶）：attach/openFor 都要查它，保证「选择命中→交付」窗口里 release(0) 后宿主回调回落被动入库而不是被吞。settle 失败且 rec 已 attach 的极端情况只能丢弃+warnf（handleUsage 已返回 claimed，无法追溯被动入库）。
  - `Service.Close()` 停 reservationBeatLoop（beatsStop 通道）；reconfigure 对旧 svc 必调，configure 失败路径还要把 runtimeState 四字段清空（nil svc 时各 handler 已有干净的 service_unavailable 分支）+ 停旧 notifyStop。
  - `FindKeyByCallerScope` 只取 Usable 语义的 Key（enabled/unrevoked/unexpired，SQL 过滤），兜底鉴权不能被已撤销的最新 Key 遮蔽。
  - 流式读泵有 `streamIdleTimeout`（10 分钟）无进度守护：startStreamIdleWatchdog 刷 atomic 时间戳，超时 closeHostModelStream 解除阻塞中的 stream_read——宿主回调是 C ABI 同步调用无取消语义，这是唯一能解卡死的手段。直连与路由两个泵都要接。
  - `requestBodyWithStreamUsage` 是字节级定点注入（json.Decoder 定位顶层 stream_options 字节区间，解码器 InputOffset−RawMessage 长度=值起点）：**不要再改回整包 map 往返**——Marshal 会重排序键并 HTML 转义 `<>&`，对宿主是可见的请求体变异。TestRequestBodyWithStreamUsage 七分支锁定。
  - `ResetStatistics` 的 KeyCounters 必须金额与 token 八列同步清；`snapshotTables` 现含 model_routes/notify_endpoints/report_configs，RestoreFrom 提交后触发 notifyPricingChanged+notifyRoutesChanged（validateSnapshot 的 schema 版本强校验保证旧备份不会因新表缺列误入）。
  - 趋势周粒度桶表达式是 `date(x,'unixepoch','-6 days','weekday 1')`：SQLite `weekday N` 在日期已是 N 时不前进，先 weekday 再 -7 days 会把周一退到上周（实锤过）。TestTrendsWeekBucketMonday。
  - `ApplyRetention` 每批一个独立写事务（retentionBatchSize=20000，deleteBatchesTx）；usage_rollups 是 WITHOUT ROWID 表，删除按复合主键元组 IN 子查询写。
  - Stats 的七个计数合并为一条标量子查询单往返。

- **gzip 池化 writer 的空流泄漏（v0.4.0 回归，2026-02 修复）**：`sync.Pool` 取出的 `gzip.Writer` 若 Reset 后从未被写入，`Close()` 仍会向目标输出一整个「空 gzip 流」的二进制字节——在 lazyGzipWriter 的明文分支（响应小于 min_bytes）恰好追加到 JSON 尾部，浏览器 `r.json()` 报 "Unexpected non-whitespace character after JSON"，DevTools 里表现为可读 JSON 后一个 � 替换符。修复：仅 `lw.useGz` 为真才 Close。判据函数 `jsonBytesValid`（httpapi/lazygzip_test.go）与浏览器同口径，可复用于任何「body 必须是单值 JSON」的断言。

- **入库时防重（2026-02 性能优化批）**：双写残余竞态改为两侧写事务内探测合并——被动侧 `store.RecordPassiveUsage`（带 PassiveDedupeHint）与执行器侧 `SettleReservation` 在同一事务内 `duplicateProbeTx` 探测另一口径行，命中即合并、不插行；`ReconcileRequestDuplicates`（store+service）已**整体删除**，main.go handleUsage 不再落库后对账，service.Settle 不再起 goroutine 对账。`DedupeRequests` 仅存于 Maintain 兜底。依据：单写者串行化使事务内「先查后插」原子成立，谓词与旧对账完全一致故覆盖面等价。
- **schema v7**：`idx_reservations_caller_status(caller_id,status)`（caller_scope 预占聚合）与 `idx_plugin_keys_scope_created(caller_scope,created_at)`（FindKeyByCallerScope）。另：读池 cache_size 已降 -2000、maxReadConns=4；写池保持 -8000；DSN 增加 journal_size_limit(64MiB)。
- **prepared statement 缓存实测否决**：database/sql 的 `tx.Stmt` 对 modernc 驱动仍逐次重编译（零收益）；更危险的是惰性 `db.PrepareContext` 在写事务内会与单写连接池自锁（MaxOpenConns=1，事务占满唯一连接，取不到连接做准备→永久挂死，实测发生）。热路径语句统一经 `Store.execHotTx` 收口，未来绕过 database/sql 时只改这一处。
- **/keys 服务端分页契约（2026-02）**：query 支持 `status=active|disabled|revoked|expired` 与 limit/offset；响应含 `status_counts`（只受 caller/search 过滤，不受 status/分页影响），前端徽标与「共 N 枚」靠它拿全量口径。**OnlyActive 旧口径（不看 expires）必须保留**：通知扫描靠它发现「已过期但形式上启用」的 Key 发告警——曾误把它并入 active 语义导致 TestNotifySweepEdgeTriggers 失败。前端 keysView.cache 现在只是当前页数据：reqKeyCombo 密钥联想候选暂只覆盖当前页（全量候选接口待产品确认）。
- **面板数据面收敛**：维度分组后端硬上限 500（limit<=0 或超限都 clamp，Total/Count 仍全量下推）；trends 超 400 桶自动倍增桶距（范围端点缺失时用 MIN/MAX(bucket_minute) 补齐）；usage 页 loadDim 显式 limit=50。gzip 中间件受 `response_compression`/`response_compression_min_bytes` 控制（首写长度判定，lazyGzipWriter）；`httpapi.New` 签名是 `(svc, st, httpapi.Options{ManagementKey, CompressionEnabled, CompressionMinBytes})`。管理 RPC 已不用 httptest（手工构造 Request + rpcResponseWriter）。console 明文/gzip 两版都 sync.Once 常驻。
- **认领注册表分桶（2026-02）**：usageClaims 由全局切片改 `map[归一化模型名]*claimBucket`，桶锁独立；kid 优先+FIFO 选择语义不变（跨桶扫描用 created 比较保 FIFO）；过期残留登记/匹配时惰性摘除。usageClaim.buckets 记录所属桶供 O(模型数) 注销。

- 本机环境：`GOROOT` 被错误设为 `D:\Go\bin`，正确值是 `D:\Go`——每次 go build/test 前需 `$env:GOROOT='D:\Go'`。
- **禁止用 PowerShell 管道改写源文件**（`(Get-Content) -replace | Set-Content`）：会把 UTF-8 中文注释写成乱码并吞换行导致语法错误。改文件一律用 edit/write 工具。**2026-02 会话又犯两次：即使只替换 ASCII 标识符也会毁文件**（httpapi/reports_test.go 被 Get-Content|Set-Content 吞换行截断中文后整文件重写）——无条件禁用，每次先 Read 再 edit/write。
- 用量缓存字段双口径：OpenAI 系上游把命中记进 `cached_tokens`（含在 input 内，cache_read/creation 为 0），Claude 系单独记 `cache_read_tokens/cache_creation_tokens`（input 不含）。库内存上游原样值，计价归一只在 `usageparse.Billable()`；面板展示「缓存读」统一用 `max(cache_read, cached)`（cacheReadOf()，v0.3.0 起应用于全部展示位）。部分上游（stepfun/agnes/nvidia 实测）完全不报缓存字段，插件无法补数据；缓存写在 OpenAI 系无对应字段。
- 根包 cgo 测试（routes_test.go、usageclaim_test.go 带 `//go:build cgo`）：v0.3.1 发版回归中 `go test ./...` 根包实际运行并通过——此前「本机无 gcc 无法本地跑」的结论已过时，直接跑勿预设跳过。若某环境下根包显示 "no test files"，是 CGO 被禁时的 main_stub.go 兜底现象。
- internal\store\redact_test.go 存在既有 gofmt 未格式化问题（非本人引入，未修）；另有 9 个 Go 文件（httpapi.go、service/{admin,analytics,modelsdev,service}.go、store/{keys,migrate,pricing,quota,types}.go）是 v0.3.2–0.3.4 其他会话提交带入的格式漂移——回归 gofmt 时预期出现，发版提交不碰。
- 密钥面板现为画廊卡片 + 居中详情 dialog 架构（第 4/5 版重写，v0.3.10 定型）：renderKeys 输出 `<article class="ky-card">` 进 `.ky-grid`，卡内状态 pill、标签+kid 芯片同行（kid 统一 6+…+4 短格式，data-copy 点击复制）、余额区对半分（左金额右 Token，竖分割线，大字「余 X · 已用 Y」，进度条贴卡片下缘；单档独占整行 .ky-single）；点击卡片开 #key-dialog（原生居中 dialog——右缘滑出会盖住宿主管理页右上角四钮面板，**任何 fixed 右侧浮层都别用了**），Esc 经 cancel 接退出动画、背板/X 关闭、焦点返还；余额骨架屏同构预占位。签发按钮在工具栏最左。签发/编辑共用的 `.sheet` 弹窗（console.js openSheet/animateCloseSheet）同样有弹入/弹出动画（v0.3.11）：关闭走 closing 类 + 150ms 后 `close()`（复用 kd-out/kd-fadeout 关键帧），Esc 经 cancel preventDefault 接入，openSheet 开场清残留 closing 类防「动画期间重开被挂起定时器误关」；onOk 报错除 toast 外落 sheet-note 并亮出复制按钮。
- **限额域语义（v0.3.10）**：金额限额与 Token 限额互斥二选一（service.IssueKey/UpdateKey 按合并后状态校验）；数值口径 正数=上限、0=禁用（真实限额，引擎按 已用>上限 判定）、-1=不限（与显式 null 同效，httpapi 双路径归一）。编辑表单全量回填现值并总是全量提交两族（未选族发 -1）；并发例外：max_concurrent_requests 仍是 0=不限（显式 null 同样清空归零）。
- **通知与路由（v0.3.12）**：shoutrrr 端点（notify_endpoints 表 URL AES-GCM+active pepper 加密）+ 每分钟扫描 goroutine（configure 启动/runtimeState.notifyStop 收口，Writable() 门闩）；告警边沿触发存 preferences notify_state，错误上报同源 1h 冷却。requests.upstream_model（schema v5）记录二次路由：被动路径取宿主 Model（≠Alias 时），执行器靠 usageparse.Accumulator 嗅探响应顶层 model 字段——**shoutrrr 发送只能带 title 不能带 level**（level 非 lark 等服务合法键，会报 not a valid config key），严重度由标题文案区分。UI 教训：.split.even 只有两条轨道，三卡并排需 .split.triple；固定行数表格用 table-wrap min-height 留白而非假行；概览圆环为 SVG dasharray 顺时针扫描（DONUT_ANIM_MS=550）。
- UI 验证环境：`scripts/devserver.go` 起 127.0.0.1:18080/console（密钥 dev-secret，`CPA_DEV_DATA_DIR` 指定数据目录）。验证方式见 Rules 的「禁止链式调用 playwright-cli」条目。
- 用户实际部署的宿主不在本机（无 8317 监听），排查线上问题只能靠用户导出的 CSV 或宿主源码（github.com/router-for-me/CLIProxyAPI）。
- 配置加载用非严格 `yaml.Unmarshal`（无 KnownFields）：删除配置结构体字段不会破坏用户已有 YAML（未知键被静默忽略）。v0.3.0 据此彻底删除了 `quota.stream.max_buffer_bytes`。注意 main.go 的 ConfigFields 注册需与配置结构体同步维护。
- 自算 TPS 双层防线：写入侧可信上限 `maxPlausibleTPSMilli = 3_000_000`（3000 token/s，service.go Settle，全库唯一写入点）——宿主缓冲整段转发时 generation 仅几毫秒，会算出物理不可能的 TPS 污染聚合均值；展示侧 console.js `fmtTPS()` 同口径钳制（>3000 token/s 或 ≤0 → "-"），用于隐藏 v0.3.0 之前入库的历史脏行及其 rollup 均值。不做 DB 迁移清洗，靠保留期自然老化。
- playwright-cli 快照 .yml 可能滞后于实时 DOM（曾按 mtime 取最新仍读到旧计数误导验证）——验证页面状态用 eval 直接读 live DOM；eval 参数内避免转义双引号（PowerShell 会拆参报 "too many arguments"），JS 内部用单引号或按索引选元素。
- 请求筛选语义（internal/service/analytics.go requestFilter，面板与 CSV 导出共用）：model 按「精确名或 `渠道/后缀`」匹配——`(model = ? OR model LIKE '%/<输入>' ESCAPE '\')`，与库内判重口径一致；key_id/caller_id/provider/result 保持精确等值。前端筛选框用原生 `<input list>` + `<datalist>` 联想：模型候选取 `/usage/dimension?dimension=model`，密钥候选举 keysView.cache 渲染 `<option value="kid">标签</option>`。
- shoutrrr v0.17.1：所有服务 Send 开头跑 UpdateConfigFromParams，Params 里未声明的键=硬错误（网络 I/O 前）。lark 合法键 [link secret title]，URL 格式 `lark://open.larksuite.com/<token>` 或 `open.feishu.cn`（服务自拼 /open-apis/bot/v2/hook/，签名加 ?secret=）；title 是 telegram/ntfy/generic/discord/lark 的通用合法键。level 参数类型是 types.MessageLevel（不是 types.Level）。
- service.GroupByDimension 对 rollup 支持的维度默认读 usage_rollups 表——测试造数据必须同时种 requests 与 usage_rollups 两张表，否则模型/密钥分组为空。
- store.KeyUpdate 可清空字段用双层指针（`**money.Micro`/`**time.Time`）：外层 nil=不改、内层 nil=清空；httpapi 更新路径数值解析统一走 jsonInt64（裸数字与带引号字符串都收）——max_concurrent_requests 曾漏用导致编辑恒报「必须是整数」（v0.3.10 回归已修+测试钉）。preferences/meta 是自由 KV：notify_settings/notify_state/notify_error_state 已被通知功能占用（错误上报冷却 map[source]unix）。新增管理路由必须双写 httpapi register() 与 main.go managementRegistration()（对账测试双向校验）。
- requests.upstream_model 列(schema v5)：上游实际声明的模型名(二次路由真名)，仅在与 model 列(宿主别名)不同时填写，空=直连/未知。执行器路径靠 usageparse.Accumulator.Model() 嗅探响应顶层 "model"(首块命中即止)，被动/认领回填用宿主上报 Model。requests CSV 导出表头含该列(model 后第二列)。
- 缓存命中率口径(前后端必须一致)：hit=max(cache_read+cache_creation, cached)，denom=input+cache_read+cache_creation；面板展示一律走前端 cacheHitRate()，后端 cacheHitRateBP 同口径仅供 API。
- 错误上报设计：NotifyErrorEvent(ctx, source, text) 受 notify_settings.error_alerts 开关；范围=报告失败(source="report")+存储租约丢失("storage")；同来源 1h 冷却、按尝试时间推进(失败也计入)。store.SetLeaseLostHandler 在心跳协程触发，回调必须立即返回(异步发送)。
- **顶栏布局定型（2026-08，用户否决控件直排顶栏「太丑」）**：bar-tools 只留 时间范围 / 刷新(图标) / 设置齿轮 / 退出 四件，全部避让宿主右上四钮面板（bar-tools margin-right:auto 靠左）。显示币种与自动刷新（含秒数）收纳进齿轮弹层 `#settings-pop`（复用 .pop 视觉，Escape/点击外部关闭并归还焦点；自动刷新未勾选时秒数行置灰 .off）；币种选择框仍要带文字说明的选择框（用户明确要求过，勿退回纯 $/¥ 图标）。窄屏媒体查询里 .pop 的 left:0 覆盖只作用于 .range，`.tb-settings .pop` 必须保持 right:0（贴右缘向左展开），否则向右溢出屏。
- 概览/用量页布局（v0.3.12 定型）：概览 = 读数行 → 趋势图 → `.split.triple` 三卡 1:1:1（模型占比|密钥消耗|计价覆盖；.split.even 只有两条轨道，三卡会换行）；两张占比卡为**单圆环**（console.js drawDonut/donutEntries：按当前指标取前 5+其余合并「其他」灰 #9aa5b1；hover 段/图例联动中心详情并压暗其余段；入场为顺时针单前沿扫描——每段时长∝弧长、delay=前序累计、linear 缓动，DONUT_ANIM_MS=550）；计价覆盖卡数据复用 loaders.overview 已拉的 /costs 响应（renderCostCoverage）。用量页 = 首行 `.split.even` 1:1（维度聚合|上游路由，两表均固定 5 行/页 + table-wrap.fixed5 min-height 留白 + 常驻换页栏，上游路由首列 bar-cell 与维度表同构）→ 请求明细全宽；**宽表格禁止同行拼接**（1:3 同行方案被用户否决「太丑了」）。前端分页统一「全量拉回+切片+pager」，视觉量按全量算：维度/路由 5 条/页、计价规则 10 条/页（用户指定值）。趋势图天/周/月粒度柱宽 = 槽宽−固定间隙（上限 96px），细粒度保持 24px 上限。「更新于」时间戳紧跟 tab 组之后靠左（margin-left:auto 推最右会被宿主四钮面板遮挡）。
- **宿主对插件管理 API 的响应体做 HTML 转义**（2026-08 实锤，v0.5.1 后）：internal/pluginhost/management.go `ServeManagementHTTP` → `htmlsanitize.JSONBodyIfLikely` 对 JSON 所有字符串值跑 html.EscapeString（`"`→`&#34;`、`>`→`&gt;`），**请求体原样透传不转义**。后果：任何含引号/尖括号的字段经 GET 读回都是实体形态，编辑后原样回存即损坏（模型集合规则首个踩中）。往返型字符串字段必须双层防御：保存期服务端 decodeHTMLEntities（httpapi/modelroutes.go）+ 前端 unescapeEntities 显示归一（console.js loaders.routes）；新功能照抄此范本。
- **model.register 注册链路（已对照宿主源码核实）**：能力键 model_registrar / 方法名 model.register / 响应 {provider,models} 经 json 大小写不敏感解码全部正确；恒声明能力位（勿按「存在启用路由」条件声明——宿主启动时无路由则能力位=false，建路由后必须等一次 reconfigure 才会被当 registrar，这是「别名没进 /v1/models」的实际踩坑路径）。宿主 syncPluginModelRuntime 触发点=启动/config.yaml 重载/auth 文件增删改/管理端保存任意配置，无定时器——插件运行期新增数据后需触发其一。executor 插件的 registrar 模型经 RegisterExecutors 以 clientID `plugin:<id>:<provider>:executor` 进公开 registry（provider 与原生冲突时才走 auth 合并路径）。
- **面板新增表格组件的唯一合法形态（2026-09 实锤）**：CSS 里没有 `.tbl` 类，全仓表格样式只有 `table.data`（th 底边框/加粗小字号、td 内边距 9px 12px、td.num 右对齐 tabular-nums、cell-clip 截断、pill 状态胶囊）。v0.8.0 新增「进行中请求/目标健康」两面板用了不存在的 `class="tbl"`，渲染成无样式裸表格被用户实锤「格式样式完全错误」。新增任何表格一律抄请求明细/维度聚合的 `table-wrap + table.data + w-grow/cell-mono/cell-clip/pill` 结构；`node --check` 只查语法查不出类名拼错，新面板要用浏览器 computed-style 断言（tableClass/thBorderBottom/numAlign）核验。
- **store 层时间列扫描口径**：reservations/plugin_keys 等表的时间列存 UnixMilli 整数，必须按 int64 Scan 再 time.UnixMilli(x).UTC() 转换（范本 scanReservation）；直接扫 *time.Time 会得 "unsupported Scan, storing driver.Value type int64 into type *time.Time"，接口 500。ListHeldReservations 是迄今唯一踩坑点（v0.8.1 修复+测试钉住），新查询照抄 scanReservation 口径。
- **seed/规则语言两处语法约束（2026-09 实测）**：routelang 目标名必须带引号（`-> "gpt-4o-mini"`，裸名报「无法识别的字符 "-"」）、逻辑运算符是 `&&`/`||`（英文 and/or 报「条件后应为 ->」）；规则编译失败时 RoutesHealth 优雅降级（refs 为空→该集合不进健康面板），不会报错——面板缺集合先查规则能否编译。
