# cpa-usage-manager 性能优化方案（草案 v1）

> 目标：降低插件运行时的 **CPU、内存、IO** 占用，且不违反 DESIGN.md 锁定决策（单一 SQLite/WAL/单写者租约、usage 单路径入库、整数 micro-USD 计价等）。
> 证据来源：2026-02 对全部源码的通读（store/service/httpapi/web/main 五层），每条发现均带 `file:line`。
> 状态：**P0/P1 已实施**（见下），P2 其余项按需推进。

## 实施状态（2026-02）

| 项 | 状态 | 说明 |
|---|---|---|
| P0-1 | ✅ 已实施 | `service.ParseRequestMeta` 单次类型化解析，经 `ReservePlan.Meta` 贯穿预占与落库；偏差：流式注入 `include_usage` 仅 OpenAI 流仍保留一次 map 解析（直接 Marshal 结构体会破坏原请求体，不可行） |
| P0-2 | ✅ 已实施 | service 挂起表 + 心跳协程批量刷 `TouchKeysLastUsed`；queueKeyTouch 同时负责拉起心跳协程（纯鉴权场景） |
| P0-3 | ✅ 已实施 | 入库时防重：`RecordPassiveUsage`/`SettleReservation` 事务内探测合并；删除逐请求对账与 goroutine；Maintain 兜底保留 |
| P0-4 | ✅ 已实施 | schema v7：`idx_reservations_caller_status` + `idx_plugin_keys_scope_created` |
| P0-5 | ✅ 已实施 | gzip 尊重开关与 min_bytes（首写长度判定，lazyGzipWriter）；httptest 替换为手工 Request + rpcResponseWriter；New 改 Options 签名 |
| P0-6 | ✅ 已实施 | console 明文 sync.Once 常驻 |
| P0-7 | ✅ 已实施 | 维度分组硬上限 500（Total/Count 不受影响）；trends 超 400 桶自动倍增桶距；前端 loadDim 带 limit=50 |
| P1-1 | ❌ 实测否决 | tx.Stmt 对 modernc 驱动仍逐次重编译（零收益），惰性 Prepare 更会与单写连接池自锁挂死；保留 `execHotTx` 收口待未来绕过 database/sql |
| P1-2 | ✅ 已实施 | 并发 COUNT+held 金额/token SUM 合并为单次扫描；caller 口径金额+token 一条查询；拒绝前按 expires_at 补救清扫一次再复核 |
| P1-3 | ✅ 已实施 | 读池 cache_size -2000、maxReadConns=4；写池保持 8MiB |
| P1-4 | ✅ 已实施 | 认领按归一化模型名分桶（map[model]*bucket），登记/注销 O(模型数)，kid 优先+FIFO 语义不变；未用 16-shard 哈希（模型桶已消除主要竞争点） |
| P1-5 | ✅ 已实施 | /keys 支持 status 过滤与 status_counts 聚合，前端真分页；偏差：密钥联想候选暂降级为当前页（需产品确认后再加轻量候选接口）；OnlyActive 旧口径保留供通知扫描发现「已过期未撤销」Key |
| P2-8 | ✅ 已实施 | journal_size_limit(64MiB)；其余 P2 项维持按需 |

回归记录：`go vet ./...`、`go test ./...`（含根包 cgo 测试）、`scripts/smoke.go`、`scripts/migcheck.go` 全绿；`go build -buildmode=c-shared` 动态库构建通过。UI 侧改动按仓库规则未做浏览器自动化，待 devserver 目验。

---

## 0. 摘要

本插件以 c-shared DLL 形态运行在宿主（CLIProxyAPI）进程内，每个 LLM 请求都会经过执行器/拦截器/被动统计三条路径之一，因此**每请求热路径上的每一次多余解析、每一个多余写事务都会随 QPS 线性放大**。

当前每请求（quota 模式执行器路径）的开销画像：

| 维度 | 现状 | 优化后目标 |
|---|---|---|
| 请求体全量 JSON 解析 | 最多 4 次（估算 token、提模型、提 tier、提 reasoning_effort 各一次整包 `Unmarshal` 到 `map[string]any`） | 1 次 |
| SQLite 写事务 | 约 4 个（预占 + TouchKeyLastUsed + 结算 + 结算后对账） | 约 2 个 |
| 预占事务内语句数 | 4–8 条 | 2–4 条（聚合查询合并 + 索引支撑） |
| 管理响应体积 | 信封内 Body 经 base64 膨胀 +33%，小响应也被 gzip | 小响应跳过 gzip，尊重既有配置阈值 |
| 常驻内存上限 | 读池 9 连接 × 8MiB 页缓存 ≈ 72MiB | ≤ 16MiB |

以下按 **P0（高收益低风险，建议首批）/ P1（中等收益）/ P2（观察与微优化）** 分级。

---

## 1. 已经做对的事（不要重复建设）

排查中确认以下设计已经过优化，**本方案不再触碰**：

- 预占心跳集中化：单一后台协程 30s 一轮批量续期，非每请求一个 ticker（requestpath.go:42,68-84）；
- 陈旧预占清扫经 CAS 节流到 30s 一次（service.go:413-417）；
- 计价规则内存快照 `atomic.Pointer` + TTL 60s，命中零 DB 往返（service.go:157,591-601）；
- 正则缓存 `regexpCache`（上限 512、超限重建）（store/match.go:63-94）；
- gzip writer `sync.Pool` 复用（httpapi.go:137-154）；console 预压缩字节常驻（web.go:43-52）；
- 流式解析短路：chunk 先做 `"usage` 子串探测，绝大多数 chunk 零分配返回（usageparse.go:218-222）；
- 流式结算不留全流副本（旧版主要内存来源已移除，main.go:1020-1023 注释）;
- 报表/趋势全部 SQL 聚合于 `usage_rollups`，无 Go 内存全量聚合；
- 面板无定时轮询（console.js 无 setInterval），数据纯事件驱动加载；
- WAL `synchronous(NORMAL)` + `wal_autocheckpoint(4000)`，无逐请求 fsync（store.go:257-273）。

---

## 2. P0 —— 高收益、低风险，建议首批实施

### P0-1 合并请求体的重复 JSON 解析（CPU，热路径最大项）

**现状**：同一请求体在一次执行器调用中被完整反序列化多次：

- `estimateTokens` 整包 `json.Unmarshal(body, &map[string]any)` 只为取 `max_tokens`（requestpath.go:169）；
- model 为空时 `extractModel` 再整包解析一次（requestpath.go:187-196）；
- 按张计价时 `extractImageCount` 第三次整包解析（requestpath.go:199-217）；
- 结算落库时 `buildRequest` 里 `requestString` + `requestNestedString` 又各整包解析一次，只为取 `service_tier` 和 `reasoning.effort`（main.go:1124-1125、1147-1180）。

大请求体（长上下文、带图 base64）可达数百 KB～数 MB，每次都是 O(body) 的分配与扫描。

**方案**：新增一次性的 `requestMeta` 解析——用类型化结构体 + `json.RawMessage` 定点字段做**单次** `Unmarshal`：

```go
type requestMeta struct {
    Model          string          `json:"model"`
    MaxTokens      json.RawMessage `json:"max_tokens"`
    // …max_completion_tokens / generationConfig.maxOutputTokens / n /
    //  stream_options.include_usage / service_tier / reasoning / thinking
}
```

在执行器入口解析一次后贯穿 Reserve → buildRequest 复用；`requestBodyWithStreamUsage`（main.go:1191-1214）也改为在该结构上改字段再 Marshal 一次，替代「整包解 map → 改 → 整包编回」。

**预期收益**：executor 路径每请求减少 2–4 次 O(body) 解析与相应临时 `map[string]any` 分配；长上下文场景 CPU 收益显著。
**风险**：低。字段语义保持不变，现有测试（requestpath_test.go 等）覆盖口径。
**验证**：`go test ./...`；用 seed 数据对比改造前后 `scripts/smoke.go` 输出一致性。

### P0-2 TouchKeyLastUsed 节流批量化（IO，每请求 -1 个写事务）

**现状**：每次成功鉴权都独立开写事务更新 `plugin_keys.last_used_at`（service.go:292 → store/keys.go:526-536）。该字段只用于面板展示，精度要求低。

**方案**：进程内挂起表（`map[kid]unixMilli` + 短临界区），由已有的集中心跳协程（30s 一轮）顺带批量刷库（一条 `CASE WHEN` 批量 UPDATE 或按 kid 循环合并进同一事务）。进程意外退出丢失 ≤30s 的 last_used 更新，可接受。

**预期收益**：quota 模式每请求少 1 个 `BEGIN IMMEDIATE…COMMIT` 写事务（约省 1/4~1/3 的每请求写 IO 与 writeMu 占锁次数）。
**风险**：低。多进程部署下各实例各持挂起表，最终一致即可。
**验证**：单测钉住「鉴权成功但 last_used 延迟 ≤ 刷库周期」语义；面板密钥页显示正常。

### P0-3 入库时防重：查重并入单一写事务，取消逐请求事后对账（IO + CPU/goroutine）

**现状**：v0.2.2 认领机制根治了同进程重复入库，但兜底链路仍是「先插后补」三段式：

- 被动路径：只读预检 `FindDuplicateExecutor`（usage.go:237-263，独立读）→ 插入 `RecordUsage` → 再开一个写事务跑 `ReconcileRequestDuplicates`（main.go:1717；实现在 usage.go:280-325：加载 anchor 行 + 启发式配对 + 合并/删除 + 聚合扣减回补）；
- 执行器路径：`SettleReservation` 无条件 INSERT、不做任何探测；结算成功后**每个请求**起一个新 goroutine 跑对账（service.go:482-487）；
- 重复的根因是 TOCTOU 窗口：预检与插入不在同一事务里，执行器结算与宿主被动入库交错时互相看不见对方未提交的行，只能事后对账补救——而合并逻辑 `mergeRequestPairTx`（usage.go:332-374）本身已是事务安全的。

**为什么可以在入库时就避免重复**（对本项目结构的两条关键观察）：

1. **单写者串行化使事务内查重天然原子**。所有写事务经进程内 writeMu + 单写连接排队（store.go:233-235,394-397），跨进程由租约接管保证任一时刻只有一个实例可写。因此只要把查重放进各自的事务内，「先查后插」不再有窗口——两个事务必然一先一后提交，后者一定能看到前者已提交的行。
2. **匹配谓词不变，覆盖面与事后对账完全等价**。事务内探测复用与 `ReconcileRequestDuplicates` 相同的启发式谓词（模型候选 + 延迟±dupLatencyWindow + 时间±dupTSWindow + key_id 口径互补），凡是事后对账能配上的对，插入时探测同样配得上；反之，对账的盲区也仍是盲区。防重时机前移不缩小覆盖。

此外两路残余入口现状已经不入库：cum 密钥的迟到回调只回填不插行（main.go:1686-1694）；纯统计模式（quota 关闭）没有执行器行，天然无双写。

**方案**：

1. 抽出事务内版探测器 `findDuplicateExecutorTx(ctx, tx, …)`（谓词与现有一致，供两侧写入方共享）；
2. 被动路径改为 **record-or-merge**：`RecordUsage` 事务内先探测，命中则等价于对既有执行器行做合并（复用 `mergeRequestPairTx`，keeper=执行器行），未命中才 INSERT；main.go:1698 的只读预检保留为快路径（命中时连写事务都不必开启）；
3. 执行器路径在 `SettleReservation` 事务内、INSERT requests 之前加同一探测：命中说明被动行抢先落库 → 在同事务内插入执行器行并立即 `mergeRequestPairTx` 合并掉被动行，外部看不到任何中间态；
4. **整体删除逐请求对账**：service.go:482-487 的每结算 goroutine 与 main.go:1717 的落库后对账调用移除；
5. `Maintain` 的周期 `DedupeRequests`（usage.go:394-404）保留，作为升级遗留与异常场景的最终兜底。

**预期收益**：

- 每请求写事务从 ~4 个降到 ~2 个（预占 + 结算；被动路径从 2 个降到 1 个），并消除每结算一 goroutine 的调度开销与并发波动；
- 比「节流对账」更优：**任何时刻库内都不可见临时重复行**，面板无收敛窗口；
- 结算事务净增仅 1 条探测查询（时间窗有界，可走 idx_requests_model_ts / idx_requests_ts），省掉的却是整个对账事务。

**风险**：中。改动落在额度结算这一资金安全核心事务上（事务内语句 +1，回滚路径变长）；启发式误配对的固有风险与现状持平（贪心配对、聚合口径守恒的原则不变，usage.go:390-392）。需逐分支对拍测试。
**验证**：新增双写竞态用例，断言三种交错次序（被动先行 / 执行器先行 / 同批交错）均只余一行且分钟聚合守恒；dedupe_test.go、reservation_sweep_test.go、usageclaim_test.go 全量回归；手动触发 Maintain 确认兜底路径仍工作。

### P0-4 补齐预占路径缺失索引（CPU/IO，caller_scope 模式高优先）

**现状**：caller_scope 模式下额度判定按 caller 维度查询 `reservations WHERE status='held' AND caller_id=?`（quota.go:85,97,124），但索引只有 `(key_id,status)`、`(heartbeat_at) WHERE held`、`(key_id,settled_at)`（migrate.go:124-126），无 `(caller_id,status)`——只能走 held 部分索引后过滤或全扫。同类问题：`plugin_keys.caller_scope` 查询与 `ListKeys` 的 `%LIKE%` 过滤 + created_at 排序均无索引支撑（keys.go:245,280-298）。

**方案**：schema 迁移新增：
- `CREATE INDEX idx_reservations_caller_status ON reservations(caller_id, status)`；
- 视实际查询计划考虑 `plugin_keys(caller_scope, created_at)`。

**预期收益**：caller_scope 用户预占事务内的 COUNT/SUM 从全扫变索引范围扫；Key 数多时鉴权/列表查询同步受益。
**风险**：低。仅增索引；注意迁移版本号递增并兼容已有库。
**验证**：`EXPLAIN QUERY PLAN` 前后对比；migcheck.go 校验。

### P0-5 管理响应编码瘦身（CPU/网络，管理面全局）

**现状**（三个叠加问题）：
1. `rpcManagementResponse.Body` 是 `[]byte`，`encoding/json` 序列化为 base64，体积 +33%（main.go:163-167）——这是宿主信封契约的一部分，无法单方面取消；
2. gzip 中间件对所有响应一律压缩，`response_compression_min_bytes` 配置存在但**未被引用**（config.go:101-102,194-195 vs httpapi.go:139-154）——大量几百字节的小 JSON 响应付出 gzip level 6 的 CPU 还可能越压越大；
3. 响应体经历 Encoder.Encode 进 Recorder → 整段拷贝进信封结构 → base64 → `C.CBytes` 出边界，共 3 次 marshal + 2 次边界拷贝（httpapi.go:168-172、main.go:418-424,1401-1422,406-416）。

**方案**：
1. 让 gzip 中间件尊重 `min_bytes` 阈值（低于阈值的响应直接明文写出）——一行级改动，立即生效；
2. `httptest.NewRequest/NewRecorder` 替换为轻量直通实现（自定义最小 `http.ResponseWriter` 包装 `bytes.Buffer`，请求侧手工构造 `*http.Request`）消除 httptest 语义开销（main.go:1410,1419）；
3. （可选，P1）base64 属契约开销，若要与宿主协商二进制透传（信封增加 `body_encoding: "raw"` 字段）另立提案，不在本批。

**预期收益**：小响应省掉 gzip CPU；每条管理 RPC 减少 1–2 次全量缓冲拷贝。面板操作延迟与宿主信道带宽同步下降。
**风险**：低。第 2 项需保证 Header/Code/Flush 语义等价（现有 routes_test/httpapi_test 覆盖较全）。
**验证**：`go test ./...` + devserver 打开面板逐页目验。

### P0-6 console 明文 HTML 结果缓存（CPU/内存抖动）

**现状**：gzip 客户端命中预压缩字节，但非 gzip 请求每次重新 `assemble()` 拼 224KB 单文件（web.go:37-38）。

**方案**：`sync.Once` 缓存明文 assemble 结果（与 gzip 版对称）。
**收益**：省每次 ~224KB 分配与两次 ReplaceAll 扫描；对老客户端/代理回源场景有效。
**风险**：无（构建期注入内容进程内不变）。

### P0-7 面板维度接口默认全量拉取收敛（CPU/内存，服务端保护）

**现状**：usage 页 `loadDim` 调 `/usage/dimension` 不带 limit，后端 `limit<=0` 时 SQL 无 LIMIT 返回**全部分组**（console.js 加载处 + dimensions.go:95-124）；keys 页一次拉满 1000 条客户端分页（console.js:1336-1340）；trends 支持 "all" 范围且桶数无上限（console.js:476-500）。

**方案**：
1. usage 页 dimension 调用显式带 `limit=50`（与概览页一致）；
2. 后端给 `limit<=0` 加硬上限 clamp（如 500），防误用打爆；
3. trends 后端按粒度 clamp 最大桶数（如 400 桶，超出合并尾桶）。

**预期收益**：模型/密钥基数大的用户（数千模型名）打开用量页的数据量与渲染时间从不可控变为有界。
**风险**：低。「全量分组」目前无功能依赖（前端本来就分页切片）。

---

## 3. P1 —— 中等收益，第二批

### P1-1 热路径 SQL prepared statement 缓存（CPU）

**现状**：除两处（偏好批量 store.go:606、models.dev 同步 pricing.go:367）外，所有语句每次 `Exec/Query` 都由 modernc/sqlite 纯 Go 重新 prepare；executor 路径每请求 ~12–16 条语句全部重复编译。

**方案**：对固定高频 SQL（requests INSERT、rollup upsert、key SELECT by kid/hash、reservation 幂等 SELECT/INSERT/UPDATE、held SUM 系列、audit INSERT）建立 `map[sqlHash]*sql.Stmt` 缓存（RWMutex 保护；`database/sql` 的 `*sql.Stmt` 自带连接重连透明性，与单写连接兼容）。冷门语句维持现状。

**收益**：modernc prepare 是纯 Go 词法/语法解析，每语句数十 µs 级，每请求预计省 0.3–1ms CPU（以压测为准）。
**风险**：中。需注意 Stmt 与事务绑定语义（事务内应用 `tx.StmtContext(stmt)` 绑定版）、并发使用安全性、Close 生命周期随 Store 关闭。

### P1-2 HoldReservation 事务内聚合查询合并（CPU/锁持有时长）

**现状**：预占事务串行执行 4–8 条语句（quota.go:42-158）：清扫 UPDATE、幂等 SELECT、Key SELECT、并发 `COUNT(*)`、held 四档 SUM、caller 汇总 SUM、可选 token 两轮 SUM、INSERT。事务越长，writeMu 持锁越久，排队越长。

**方案**：
1. 并发 COUNT 与 held SUM 合并为一条 `SELECT COUNT(*), SUM(...) … WHERE status='held' AND …`；
2. 清扫 UPDATE 移出常规路径：依赖已有 30s CAS 节流的独立清扫（service.go:413-417 已具备），事务内仅在检测到超限时触发一次；
3. caller 汇总 SUM 仅在 caller_scope 且确有限额需要检查时执行（当前金额四档已条件化，保持同构）。

**收益**：常规路径从事务内 4–8 条降为 3–5 条；writeMu 临界区缩短，高并发下结算/心跳排队改善。
**风险**：中。额度判定逻辑是资金安全核心，必须逐分支对拍测试（quota 相关测试已较厚）。

### P1-3 SQLite 连接池内存预算下调（内存，一次性 -50MiB 级）

**现状**：读池 9 连接 + 写 1，每连接 `cache_size(-8000)`=8MiB，最坏 ~72–80MiB 常驻页缓存（store.go:237-239,270,279 注释自述）；`temp_store(MEMORY)` 使排序溢出全在内存。

**方案**：读连接 `cache_size` 降到 `-2000`（2MiB）、`maxReadConns` 从 8 降到 4（管理面单用户、读并发极低；WOL 冷读有操作系统页缓存兜底）。写连接保持 8MiB（写放大敏感）。

**收益**：最坏常驻内存从 ~72–80MiB 降至 ≤16MiB。
**风险**：低-中。极端大报表查询可能更多走 OS 缓存而非 SQLite 页缓存；用 seed 大数据集对比 trends/all 范围查询耗时确认无可测退化。

### P1-4 usageClaims 认领注册表分桶（CPU/锁竞争，高并发场景）

**现状**：认领为普通切片 + 单把全局互斥锁；登记（含 O(n) 过期清扫）、注销（O(n) 定位）、宿主回调匹配（O(n) 线性扫描 + 逐项私有锁）全部串行竞争同一把锁（main.go:1454-1456,1487-1496,1510-1517,1584-1606）。n = 在途请求数，绝对上限 10 分钟存活。

**方案**：按 `normalizeModelKey(model)` 建二级索引 `map[modelKey][]*usageClaim`（登记/注销 O(1) 均摊，匹配只扫同模型桶）；过期清扫改由登记时的惰性删除 + 定期兜底。锁拆分为分桶 shard 锁（如 16 shard 按 hash）。

**收益**：高并发（几十路在途流式请求）下消除全局锁串行点；常规个位数并发下收益有限。
**风险**：中。认领配对是防重复统计的关键机制，kid 优先/FIFO 语义必须原样保留（现有 usageclaim_test.go 钉住）。

### P1-5 密钥列表服务端分页（内存/CPU，数据面保护）

**现状**：`ListKeys` 在 `limit<=0` 时无条件全量加载（keys.go:296-299），前端固定拉 `limit=1000` 后客户端切片分页 20/页（console.js:1326,1336-1340）；`%LIKE%` 三列过滤 + created_at 排序全扫（配合 P0-4 的索引评估）。

**方案**：/keys 接口支持服务端 limit/offset + total 计数，前端改真分页（与 requests 列表同构，analytics.go:85-96 已有现成模式）。
**收益**：密钥数百以上时面板加载内存与序列化开销线性下降。
**风险**：低。注意 keysView.cache 被 datalist 联想复用（MEMORY.md 已记录），分页后联想候选改为当前页或单独轻量接口，需产品确认。

---

## 4. P2 —— 观察项与微优化（按需实施）

| # | 事项 | 位置 | 说明 |
|---|---|---|---|
| P2-1 | models.dev 目录刷新持锁拉网 | modelsdev.go:192-210 | `catalogMu` 在整个网络 Fetch 期间持有，TTL 过期后并发搜索全部串行等待；改为 single-flight + 快照读 |
| P2-2 | shoutrrr 每次发送新建 sender | notify.go:271-276 | 每条通知 `NewSenderWithOptions`；按 endpoint URL 缓存 sender 实例（低频路径，收益小） |
| P2-3 | PNG 导出画布复用 | exportpng.go:69,424 | 1280×720 RGBA ≈ 3.7MB/张临时分配 + 逐像素填充循环；sync.Pool 复用画布、`png.Encode` 用 `BestSpeed`（低频操作） |
| P2-4 | Stats 七条 COUNT(*) 合并 | store.go:755-767 | requests/audit_events 两张全表 COUNT 可合并为一条或改 rollup 估计值；仅 system 页触发 |
| P2-5 | FindDuplicateExecutor 扫描窗口收紧 | usage.go:250-254 | `model LIKE '%/x'` + `ORDER BY ABS(ts-?)` 无索引可用；P0-3 改造后该谓词同时承担快路径预检与事务内探测，建议一并加 ts 有界窗口与索引评估 |
| P2-6 | retryTransient 与 busy_timeout 叠加等待 | store.go:43-48,431-450 | DB 层 busy_timeout 已等待 5s，应用层又 5 次退避重试至 400ms；忙锁时单请求最长双重等待，需压测确认是否存在长尾后再定裁剪哪层 |
| P2-7 | callHost/writeResponse 边界分配 | main.go:406-474 | 每次 CString/CBytes 分配释放；free_buffer 回调在我方手中，理论上可做尺寸分桶的 C 内存池（跨 ABI 内存池复杂度高，仅在 profile 证明热点后做） |
| P2-8 | WAL `journal_size_limit` | store.go:257-273 | 增加 `journal_size_limit(67108864)` 兜底，防长时间写入下 -wal 文件失控占用磁盘（cheap insurance） |
| P2-9 | ListAudit 逐行 Unmarshal | quota.go:395-402 | audit 详情 JSON 逐行解码；量大时可惰性解码（仅展开行才解） |

---

## 5. 实施顺序建议

按「一批一版、每批可独立回归发布」推进：

1. **第一批（P0 全部）**：P0-1 / P0-2 / P0-3 / P0-4 / P0-5 / P0-6 / P0-7。
   其中 P0-3 改动落在结算事务这一资金安全核心上，需重点评审并通过新增的双写竞态用例后再实施。
2. **第二批（P1）**：P1-1 → P1-2 → P1-3 → P1-4 → P1-5（互相独立，可拆分）。
3. **第三批（P2）**：视上线后观测决定；P2-8（journal_size_limit）成本极低可提前捎带。

---

## 6. 度量与验证

**功能回归**（每批必跑，见 README/AGENTS.md 约定）：

```powershell
$env:GOROOT='D:\Go'
go test ./...
go vet ./...
go run ./scripts/smoke.go
# 发版前另跑 scripts/abi-smoke.c 导出符号校验
```

UI 改动（P0-5/P0-7/P1-5）遵循项目规则：**不做浏览器自动化**，`go run scripts/devserver.go` 起 127.0.0.1:18080/console（密钥 dev-secret）交用户目验；构建检查 `go build ./...` + `node --check internal/web/console.js`。

**性能基线与对比**（实施前后各采一次）：
- 微基准：为 requestpath 解析、HoldReservation、RecordUsage/Settle 写 `go test -bench`（seed 固定随机种子，可复用 scripts/seed.go 思路）；
- 常驻内存：devserver 下注入 1 万请求种子后读进程 RSS（P1-3 前后对比应 ≥50MiB 差异）；
- DB IO：对比固定脚本跑 1000 次模拟结算后的 `-wal` 文件大小与写事务计数（可在测试内包一层计数 hook）；
- 线上观测（用户侧）：宿主进程内存、DB 文件增长速度、面板页签切换体感。

**明确不做的事**：不改存储引擎、不改 WAL/单写者架构、不动 Key 格式与计价口径、不在生产 DLL 中引入 pprof/诊断端口（如需 profiling 仅加在 scripts/devserver.go 开发服务器上）。

---

## 附：证据索引（主要发现的 file:line 汇总）

| 发现 | 位置 |
|---|---|
| 请求体 4 次整包解析 | requestpath.go:169,187-196,199-217; main.go:1124-1125,1147-1180 |
| 每请求独立 TouchKeyLastUsed 写事务 | service.go:292; keys.go:526-536 |
| 每结算一 goroutine 跑对账 | service.go:482-487; usage.go:280-325 |
| 被动路径逐条对账 | main.go:1717 |
| 双写 TOCTOU：预检与插入分属不同事务；合并逻辑本身已事务安全 | usage.go:237-263（只读预检）、332-374（mergeRequestPairTx）、390-392（贪心配对守恒） |
| 单写者串行化（writeMu + 单连接 + 租约）支撑事务内查重原子性 | store.go:233-235,394-397 |
| 预占事务 4–8 条语句 | quota.go:42-158 |
| reservations 缺 (caller_id,status) 索引 | migrate.go:122-127 vs quota.go:85,97,124 |
| 热路径无 prepared stmt 缓存 | 全库仅 store.go:606、pricing.go:367 两处 PrepareContext |
| 读池 9×8MiB 页缓存 | store.go:31-33,237-239,270,279 |
| 管理 Body base64 +33% | main.go:163-167,418-424,1401-1422 |
| httptest 模拟每条管理请求 | main.go:1410,1419 |
| min_bytes 配置未生效 | config.go:101-102,194-195 vs httpapi.go:139-154 |
| console 明文每请求重拼 224KB | web.go:37-38 |
| dimension 不带 limit 全量返回 | dimensions.go:95-124; console.js loadDim |
| keys 拉 1000 条客户端分页 | keys.go:296-299; console.js:1326,1336-1340 |
| usageClaims 全局锁 O(n) | main.go:1454-1456,1487-1496,1510-1517,1584-1606 |
| modelsdev Fetch 持锁 | modelsdev.go:192-210 |
