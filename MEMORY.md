# 项目记忆：cpa-usage-manager

> **本文件是仓库内权威项目记忆（2026-02 迁入）**：任何 AI Agent 在本仓库工作前必须先通读本文件并以它为准；产出新的持久结论（架构决策、约定规则、踩坑教训）后应同步更新本文件。AGENTS.md 对此有明确要求。
>
> 仓库：F:\cpa-usage-manager（CLIProxyAPI 的 Go c-shared 插件）。权威设计规范是 DESIGN.md，版本历史在 AGENTS.md 第 5 节。

## Rules

- 发版流程：改 registry.json 版本号 + AGENTS.md 版本历史 → 单次提交（前缀按变更类型：补丁用 `fix:`，功能版用 `feat:`，如 `feat: v0.3.0 中文摘要`）→ 打 `vX.Y.Z` 标签推送 → CI 自动构建四平台并创建 GitHub Release。推送后**不**监控 CI（仓库约定）。
- 仓库文档与用户沟通使用中文。
- **UI 改动禁止跑 playwright/浏览器自动化验证**（用户两次明确叫停：先投诉链式调用卡死，后直接要求「停止做这种测试」）。交付前只做 `$env:GOROOT='D:\Go'; go build ./...` + `node --check internal/web/console.js`，页面效果交用户自己打开目验（需要时提示 `go run scripts/devserver.go` → 127.0.0.1:18080/console，密钥 dev-secret）。写新交互代码时要静态自查事件绑定是否覆盖所有容器（教训：复制委托只挂了 #key-rows，抽屉里的同款按钮成死键）、flex 容器的滚动区要显式 flex:1+min-height:0（教训：抽屉底栏不贴底）。

## Architecture decisions

- 宿主（CLIProxyAPI）对插件发起的嵌套执行（host.model.execute_stream）**故意不上报用量**：内层 `InternalSource=true` 不建 reporter，外层被 nestedTracker 抑制。插件执行器路径的 token 统计只能靠插件自己解析流。v0.2.4 已修复：`Accumulator.FeedChunk` 兼容裸 JSON（宿主 openai→openai 直通翻译会剥掉 `data:` 前缀并丢弃 `[DONE]`）。已在生产验证生效。
- 被动入库（usage.handle 兜底）模型名必须优先用宿主上报的 `Alias`（用户配置名），`Model` 是上游实际路由名（如 OpenRouter 回报 `stealth/ox-alpha`）。v0.2.3 修复。
- 面板 Token 显示用 `fmtTok`（K/M/B 自动升级，阈值取 999.5 倍数避免 1000K）；概览「总消耗 Token」主值完整显示精确到个位。非 token 数字仍用 `fmtInt`（万/亿）。

## Discovered durable knowledge

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
- 概览/用量页布局（v0.3.12 定型）：概览 = 读数行 → 趋势图 → `.split.triple` 三卡 1:1:1（模型占比|密钥消耗|计价覆盖；.split.even 只有两条轨道，三卡会换行）；两张占比卡为**单圆环**（console.js drawDonut/donutEntries：按当前指标取前 5+其余合并「其他」灰 #9aa5b1；hover 段/图例联动中心详情并压暗其余段；入场为顺时针单前沿扫描——每段时长∝弧长、delay=前序累计、linear 缓动，DONUT_ANIM_MS=550）；计价覆盖卡数据复用 loaders.overview 已拉的 /costs 响应（renderCostCoverage）。用量页 = 首行 `.split.even` 1:1（维度聚合|上游路由，两表均固定 5 行/页 + table-wrap.fixed5 min-height 留白 + 常驻换页栏，上游路由首列 bar-cell 与维度表同构）→ 请求明细全宽；**宽表格禁止同行拼接**（1:3 同行方案被用户否决「太丑了」）。前端分页统一「全量拉回+切片+pager」，视觉量按全量算：维度/路由 5 条/页、计价规则 10 条/页（用户指定值）。趋势图天/周/月粒度柱宽 = 槽宽−固定间隙（上限 96px），细粒度保持 24px 上限。「更新于」时间戳紧跟 tab 组之后靠左（margin-left:auto 推最右会被宿主四钮面板遮挡）。
