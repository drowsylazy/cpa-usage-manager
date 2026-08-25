# cpa-usage-manager 模型路由（集合别名）性能优化方案

> 目标：把 v0.5.0 新增的模型路由热路径（命中别名的每个请求）收敛到直连路径的性能基线，同时不违反 DESIGN.md 锁定决策与既有结算语义。
> 证据来源：2026-02 对 main_routes.go / main.go(execute·runStream) / internal/service{routes,requestpath,service}.go / internal/routelang / internal/store{routes,migrate,quota}.go / internal/httpapi/modelroutes.go / internal/web/console.* 的通读，加上本轮新增微基准的实测数据（i5-11300H · go1.27 · windows/amd64，见 §7）。
> 状态：**P0-1 / P0-2 / P0-3 阶段一 / P1-1 / P1-2 / P1-3 已实施**（含对应验证测试：TestResolveChainDigestLazy、TestRouteSnapshotSingleFlight、TestHoldReservationAuditInTransaction）。基线参照仓库根 OPTIMIZATION.md（v0.4 批次已归档至 git 历史 f0eb9d4^）确立的「直连路径每请求只做一次请求体解析」。P0-3 阶段二与 §4 P2 按观测另行安排。

## 0. 摘要

v0.4 优化后，**直连路径**每请求只做 1 次类型化解析（`ParseRequestMeta`，实测 1MB 请求体仅 217B 临时分配）。**路由路径**在同一份请求体上目前最多做 **3 次整包解析 + 1 次（每个失败重试再各 1 次）整包反序列化/序列化往返**，其中两次在「规则根本不含 ai_judge」时纯属白付。实测画像（1MB 请求体、单目标成功场景）：

| 路由热路径环节 | 位置 | 实测开销(1MB) | 其中可避免 |
|---|---|---|---|
| 解析#1 `ParseRequestMeta`（BuildRouteEnv 用） | main_routes.go:48 | ~3.4ms / 217B | 0（保留，改为贯穿复用） |
| 解析#2 `RequestDigest`（整包→map+递归采集） | main_routes.go:49 | ~4.0ms / **1.24MB 垃圾** | **全部**（无 ai_judge 时） |
| 解析#3 `buildPlan` 内部再次 `ParseRequestMeta` | requestpath.go:250 | ~3.4ms | **全部**（meta 已在手） |
| 改写 `preparePayload` 整包 Unmarshal+Marshal | main_routes.go:174-197 | ~6.9ms / **5.4MB 垃圾** | 大部分（定点改写） |
| 每请求 `rand.New(rand.NewSource(...))` | routes.go:261 | 9.3µs / 5.4KB | 全部（全局源 9ns/0B） |
| `Prog.Eval` 求值本身 | eval.go | 0.04–0.21µs | 不是问题 |

即：**单个 1MB 别名请求比等价直连请求多付约 11–18ms 纯解析 CPU 与约 6.6MB 临时分配**；后者按 10 QPS 折算就是 ~66MB/s 的分配速率，全部落在 GC 上。以下按 P0（高收益低风险）/ P1 / P2 分级，冷路径单列（§5），查过确认无问题的点也列出（§6）防重复排查。

---

## 1. 已经做对的事（不要重复建设）

- 路由快照 `atomic.Pointer[routeSnapshot]` + 写回调即时失效 + 60s TTL 兜底（routes.go:85,89-111；service.go:176,216-217）：命中时零 DB 往返；
- 计价快照同构（service.go:625-635），Reserve 携带 `PricingOverride` 时免二次匹配（service.go:404-405）；
- 冷却状态器纯进程内 map，无 DB 参与（routes.go:162-202）；
- ai_judge 结果 LRU(512×10min) + single-flight（routes.go:414-457,685-729），评判设置 30s 缓存且经 `sync.Once` 每请求最多读一次（routes.go:210-218）;
- 规则求值 `&&`/`||` 短路（eval.go:64-88），ai_judge 这类高成本节点可靠条件短路跳过；Eval 本身纳秒级；
- 流式读泵复用 v0.4 的 `Accumulator.FeedChunk` 子串探测短路（main_routes.go:373），不留全流副本；
- 认领注册表沿用 v0.4 P1-4 的按模型分桶 + kid 优先/FIFO（main.go:1551-1743），锁粒度未因路由回退；
- 结算审计与落库同事务（store/quota.go:328-332），路由失败转移审计只在故障时发生（main_routes.go:237-243）；
- 直连老路完全未动：`execute()`/`runStream()` 先试 `resolveRouting`，未命中别名零额外开销（main.go:896,988）；
- 管理端点/面板 routes 区块事件驱动、无轮询、datalist 一次性 limit=200（console.js:2554-2599）。

---

## 2. P0 —— 高收益、低风险，建议首批实施 ✅ 已实施

### P0-1 RequestDigest 惰性化：只有规则真用 ai_judge 时才解析请求体（CPU/GC，热路径最大单项）✅

**现状**：main_routes.go:49 把 `service.RequestDigest(request)` 作为实参传给 `svc.ResolveChain(...)`。Go 实参急切求值，因此**只要命中别名，无论规则是否包含 ai_judge，都先整包 `json.Unmarshal` 到 `map[string]any` 再递归 `harvestTexts`**（routes.go:521-529,532-586）。递归对每个 map 节点先 `sort.Strings` 排全部键（routes.go:554-558）。实测 1MB 请求体 ~4.0ms CPU + 1.24MB 临时分配 + 2344 次分配调用——绝大多数用户的规则是纯 priority/weighted，这份摘要从产生到丢弃没人消费。

**方案**：
1. `ResolveChain` 签名把 `digest string` 改为 `digestFn func() string`（或 `func() (string, error)`）；
2. resolveRouting 侧仅在 `match.Route.Prog.UsesAI()` 时构造摘要闭包，并用 `sync.OnceValues(func() (string, error) { return service.RequestDigest(request), nil })` 包一层——同一请求多个 when 分支多次调用 ai_judge 也只解析一次；
3. `ResolveChain` 内部 env.AI 闭包（routes.go:211-218）里才调 `digestFn()`；`!UsesAI()` 时连 env.AI 都不必注入。

**预期收益**：非 ai_judge 规则的每个别名请求省 1 次整包 map 解析（实测 -4.0ms/-1.24MB@1MB，-0.54ms/-206KB@100KB）；含 ai_judge 的规则行为不变（摘要本就只在评判时需要）。
**风险**：低。摘要的消费点唯一（callJudge 提示词），懒执行不改变其值；`UsesAI()` 是编译期静态标记（parser.go:618-623），已在保存期校验链路上使用。
**验证**：现有 routes_test/main_routes_test 全绿；补一条「不含 ai_judge 的规则不触发 RequestDigest」的单测（可在测试里注入计数桩）；跑 §7 基准对比 Digest 项从路由路径消失。

### P0-2 请求体元数据只解析一次并贯穿路由全链（CPU，热路径）✅

**现状**：同一份 body 在一次路由请求中被 `ParseRequestMeta` 两次：
- main_routes.go:48 为 `BuildRouteEnv` 解析一次；
- main_routes.go:62 `BuildReservePlanWithPricing` → `buildPlan` 内部又无条件 `ParseRequestMeta(body)` 一次（requestpath.go:250）。

类型化解析虽然几乎零分配（实测 2 allocs/217B），但时间是 O(body) 的全量扫描（1MB ≈ 3.4ms）——第二次扫描结果与第一次完全相同。

**方案**：
1. requestpath.go 增加接受现成 `RequestMeta` 的变体：`buildPlanFromMeta(ctx, model string, meta RequestMeta, pricingModel string)`；`BuildReservePlan`/`BuildReservePlanWithPricing` 保持原签名，内部改为「解析一次后委托」，直连路径与其余调用方不受影响；
2. resolveRouting 在入口 `meta := service.ParseRequestMeta(request)` 一次，随后把它同时交给 `BuildRouteEnv(meta, ...)` 与新的计划构建变体。

**预期收益**：每个别名请求再省 1 次 O(body) 全量扫描（实测 -3.4ms@1MB / -0.34ms@100KB）；路由路径的解析次数回到与直连相同的 1 次（P0-1 生效后）。
**风险**：低。`RequestMeta` 本就是为「入口一次、全程复用」设计的载体（requestpath.go:150-153 注释自述该原则），此处只是把路由路径接入同一纪律。
**验证**：requestpath_test.go、routes_test.go 回归；断言 `ReservePlan.Meta` 与 env 变量取值口径不变（input_tokens/body_len/model/thinking_effort）。

### P0-3 preparePayload 收敛：整包 map 往返 → 单次解析复用，最终形态定点改写（CPU/GC，逐次重试线性放大）——阶段一 ✅ 已实施（bodyRewriter：循环外解析一次，每候选仅改 model 后 Marshal）；阶段二待评审

**现状**：preparePayload（main_routes.go:174-197）每次调用都对整个 body 做 `json.Unmarshal(body, &map[string]any)` → 改两个字段 → `json.Marshal(payload)` 回写。它被调用的位置：
- 非流式循环内每个候选各一次（main_routes.go:257-259）；
- 流式拨号每个候选各一次（main_routes.go:300）。

实测单次往返 1MB ≈ 6.9ms CPU + **5.4MB 临时分配**（map 结构 + 全部字符串/数组节点的深拷贝 + 序列化缓冲）。failover 到第 N 个目标就乘 N；且 Marshal 还会重排/规范化 JSON，输出体积可能膨胀。这是路由路径 GC 压力的最大来源。

**方案**（两阶段，第一阶段即可落地）：
1. **阶段一（P0，低风险）**：把「格式判定 + 整包解析」提到候选循环外做一次（`executeRoutedLoop`/runStream 的拨号循环各自持有改写后的工作副本），循环内每个候选只改 `payload["model"]` 后 Marshal。收益：N 次重试从 N 次 Unmarshal+N 次 Marshal 降为 1+N；单目标场景减半（-3.5ms/-3MB@1MB）。若解析失败（非 JSON 体）保持现状原样透传。
2. **阶段二（P1，可选进阶）**：彻底去掉 map 往返——用 `json.Decoder` Token 流扫描定位顶层 `"model"` 键的值字节区间，直接在原始字节上做定长/变长切片替换（model 名通常在 body 开头，实际扫描很快）；OpenAI 流式的 `stream_options.include_usage` 注入同样以顶层键追加方式完成（缺键时在末个 `}` 前插入）。输出是一次 memcpy 级拷贝，1MB 开销降到 <0.5ms 且零 map 垃圾。注意两点语义：重复同名顶层键时与 encoding/json「后者胜」的对齐策略需写测试钉住；非对象顶层（数组/标量）维持原样透传。

**预期收益**：单目标成功场景 1MB 省 ~3.5ms/~3MB（阶段一）→ ~6.5ms/~5.4MB（阶段二）；三目标全失败场景按倍数放大。
**风险**：阶段一极低（纯代码搬移）；阶段二中——JSON 定点改写是手写字节操作，必须用畸形输入表驱动测试（嵌套引号、转义、BOM、重复键、顶层非对象）覆盖；直连路径的 `requestBodyWithStreamUsage`（main.go:1223-1246）如一并切换可消除 v0.4 遗留的同类偏差，但建议作为独立提交便于回滚。
**验证**：TestPreparePayload 扩展为表驱动（阶段二必做）；`go test ./...`；devserver 目验流式与非流式各一发真实转发。

---

## 3. P1 —— 中等收益，第二批 ✅ 已实施

### P1-1 快照重载加 single-flight：TTL 过期瞬间的惊群（CPU/DB，突发场景）✅（pricingRules 同步处理）

**现状**：`routeSnapshot()`（routes.go:89-111）过期后没有任何并发合并：TTL（60s）到期的瞬间，所有在途别名请求（含 Settle 里的 `MatchRoute`，service.go:466）会**各自**执行一次 `ListModelRoutes` 全表 SELECT + 逐条 `routelang.Compile`（实测典型规则 ~8µs/84 allocs/条），最后一个写入者的快照覆盖前面所有人的成果。管理面保存路由触发的 `invalidateRoutes`（置 nil）同理。`pricingRules()`（service.go:625-635）是同一模式的先例，v0.4 文档未点名。

**方案**：给 Service 增加 `routeReloadMu sync.Mutex`，重载函数改为「double-checked locking」：拿锁后再查一次 TTL/nil，命中则直接返回他人刚建好的快照。`pricingRules` 同样处理（两处共 ~15 行）。
**预期收益**：重载风暴从 N×(SELECT+Compile) 收敛到 1×；高峰期 TTL 边界不再出现 DB 读毛刺。路由数少时绝对值小，但这是花 15 行买掉的尾部延迟尖峰。
**风险**：低。快照内容不变，只是去重构建过程；注意锁内不做网络 IO（这里本来就没有）。
**验证**：并发压测单测——50 goroutine 同时打 TTL 失效后的 MatchRoute，用计数桩断言 ListModelRoutes 只被调 1 次。

### P1-2 BuildRouteEnv 去掉每请求随机源构造（CPU/alloc，小请求占比高）✅（Env.Rand 已删，weighted 走 math/rand/v2 包级源）

**现状**：routes.go:261 每个别名请求执行 `rand.New(rand.NewSource(time.Now().UnixNano()))`。实测 **9.3µs + 5376B/次**——math/rand 的 rngSource 含 607×uint64 向量且 Seed 要初始化整个向量，而它服务的只是 weighted 链的一次 `Float64()`（eval.go:211-214，实测整次 Eval 仅 ~210ns）：构造开销是使用点的 ~44 倍。对小请求（几 KB body）这约占整条路由附加开销的一成半。
**方案**：Go ≥1.22 可直接删掉 Env.Rand 字段，weighted 分支改用 `math/rand/v2` 包级函数（运行时 per-thread ChaCha8 源，实测 9ns/0B，无锁无分配）；或保守做法：Env.Rand 改为惰性（首次 weighted 才构造）。
**预期收益**：每别名请求 -9.3µs/-5.4KB；weighted 与非 weighted 规则都不再付这笔固定税。
**风险**：低。加权分布语义不变（仍是 [0,1) 均匀抽样）；`math/rand/v2` 全局源的统计性质满足路由分流需求，无需可复现序列。
**验证**：routelang_test 的 weighted 用例回归；§7 基准 RandSource 项归零。

### P1-3 quota.reserve 审计并入预占事务：每请求写事务回到 2 个（IO/锁，直连+路由共同受益）✅（幂等重放不再重复记审计，语义略优于旧实现）

**现状**：OPTIMIZATION.md v0.4 批次宣称「每请求写事务收敛到 ~2」，但 `Reserve` 在 `HoldReservation` 事务之外又单独调 `AppendAudit("quota.reserve")`（service.go:451-454），后者是独立的 `BEGIN IMMEDIATE…COMMIT`（store/quota.go:405-407 经 store.Write）。即当前每成功请求实际是 **3 个写事务**：预占、预占审计、结算（结算审计已在事务内）。`SettleReservation` 早已支持变参 audits 入同事务（quota.go:243,328-332），HoldReservation 却没有对应通道。
**方案**：HoldReservationParams 增加 `Audit *AuditEvent`，事务尾 `appendAuditTx`；Reserve 删除独立 AppendAudit 调用。注意审计失败不应回滚预占（现状 `_ =` 吞错语义保持：appendAuditTx 出错时记 warn 但不 return err）。
**预期收益**：每请求 -1 个写事务（≈ -1/3 的每请求写 IO 与 writeMu 占锁次数），预占与留痕原子化（崩溃窗口不再出现「有预占无审计」）。
**风险**：中低。落在资金核心事务上，但模式与结算侧完全对称且有现成先例；事务内语句 +1。
**验证**：store 测试断言 HoldReservation 成功后 audit_events 有 quota.reserve 行、失败（额度拒）时没有；reservation_sweep_test/quota 相关回归。

---

## 4. P2 —— 观察项与微优化（按需实施）

| # | 事项 | 位置 | 说明 |
|---|---|---|---|
| P2-1 | filterCooldown 键字符串拼接 | routes.go:157-159,183-202 | 每目标每请求 `strconv.FormatInt+"|"+lower(target)` 分配 ~40B 且在 coolMu 临界区内；改 `map[struct{rid int64;t string}]time.Time` 结构体键即零分配。仅在链上目标多时有感 |
| P2-2 | StripThinkingSuffix 重复调用 | main_routes.go:47 vs routes.go:248 | resolveRouting 已算出 baseAlias，BuildRouteEnv 又对同一 rawModel 再剥一次（含 ToLower）；把 base 作参数传入即可。单次百 ns 级 |
| P2-3 | Settle 对同一预占行读两次 | service.go:458 + store/quota.go:255 | 事务外的 `GetReservation`（定价判定要 r.Model/HeldMicroUSD）与事务内的 SELECT 重复；可让 SettleReservation 的事务内读取结果承担全部决策（先开事务再定价需重构资金判定顺序，收益有限，观察后再动） |
| P2-4 | 认领超集注册的桶数随 refs 线性增长 | main_routes.go:87-96 | 每个 ref ×(裸名+带后缀) 各占一个模型桶，登记时逐桶 claimBucketsMu+b.mu 双重加锁与 O(桶内认领数) 清扫；refs 十几个的高并发下可观。可先把 Route.Refs 在快照构建期归一化缓存，登记时免逐个 normalizeModelKey；桶数本身受正确性约束不宜裁剪 |
| P2-5 | markRef 死存储 | routelang/parser.go:464 | `_ = seen[normalizeKey(model)]` 只读不写，refsSeen 从未被填充，去重实际靠 appendUnique O(n²) 兜底（parser.go:313-327）——编译期路径，量级无害，但属意图未实现的功能性瑕疵，建议顺手修正或在注释说明 |
| P2-6 | harvestTexts 预算耗尽后仍排序剩余键 | routes.go:554-565 | 达到 2000 字符上限后 walk 继续遍历兄弟分支并对后续 map 排序；预算检查前移可减少 AI 路径的无谓功。仅 UsesAI 请求受益，优先级低于 P0-1 |
| P2-7 | svc.Config() 整结构体拷贝 | service.go:204-208 调用于 main.go:1112 等 | config.Config 含多层嵌套，RLock+值拷贝每次结算一次；量级微，若未来 Config 膨胀再改原子指针快照 |
| P2-8 | aiJudge 键构造的中转 slice | routes.go:426 | append 链产生多个中间 []byte；仅评判触发时执行（LRU 未命中），量级无关痛痒，记录备查 |

---

## 5. 冷路径与管理面（单列，勿与热路径混同施策）

| 事项 | 位置 | 结论 |
|---|---|---|
| GET /model-routes 每次全量重编译全部规则 | httpapi/modelroutes.go:14-27 → routes.go:114-125 | **确认可接受**：面板打开页签才触发；Compile 实测 8µs/规则，几十条路由也在毫秒级；且它需要含停用/坏行的全量视图，与启用快照口径不同，不建议为省这点复用快照 |
| ValidateRouteRule 保存期编译+全表别名查重 | modelroutes.go:56, routes.go:274-330 | 保存动作低频，O(N) 别名比对无碍；已有 alias COLLATE NOCASE 唯一索引兜底并发（migrate.go:379） |
| modelRegistrarEnabled 直查 DB | main_routes.go:129-143 | 仅注册/reconfigure 时调用，非每请求；不动 |
| 评判设置 30s 缓存 + Mutex | routes.go:374-400 | 只在 UsesAI 请求的 ai_judge 首次触发时走到，且有 sync.Once 合并（routes.go:213）；非热点 |
| ai_judge LRU/single-flight 全局互斥 | routes.go:685-729,433-445 | 评判调用本身秒级耗时，锁开销可忽略；512 容量对单实例足够 |
| failover 审计独立写事务 | main_routes.go:237-243 | 只在目标失败转移时发生（异常路径），频率天然低；并入结算事务反而语义不清（此时尚无结算）。保留 |
| v8 schema 与索引 | migrate.go:362-381 | model_routes 行数=用户配置的路由数（个位~十位），ListModelRoutes 全表读无压力；alias NOCASE 唯一索引已覆盖查重；无需额外索引 |
| console routes 区块 | console.js:2545-2704 | 事件驱动加载、无轮询；卡片渲染 innerHTML 拼接量级=路由数；datalist 一次性 limit=200。不是问题 |

---

## 6. 查过但确认不是问题的点（防重复排查）

- **`Prog.Eval` 求值成本**：实测 39ns（priority）/ 210ns（weighted），AST 解释器对该语言的规模绰绰有余，不需要编译成字节码或缓存求值结果；
- **MatchRoute 本身**：map 查 + 至多两次 StripThinkingSuffix，strings.ToLower 对已小写输入走零分配快速路径；纳秒级；
- **冷却状态器内存**：进程内 map，条目=路由×目标数，到期条目由 filterCooldown 顺带清理（routes.go:194-198），无泄漏；
- **流式读泵逐 chunk 的 JSON 信封解析**：`rpcHostModelStreamReadResponse` 是宿主协议必需的小对象，chunk payload 本身走 FeedChunk 子串探测短路（usageparse.go:518,243-267），v0.4 已优化到位；
- **SniffModel**：子串探测级（usageparse.go:542），响应体上的开销远小于正式 Parse，二者分工明确；
- **认领 wait/settled/attach 的锁序**：c.mu 与 b.mu 无嵌套持有（attach 在桶锁外调用，main.go:1737-1742），无死锁风险也无竞争放大；
- **`Settle` 里的 MatchRoute（service.go:466）**：仅为 mode=target 定价服务，快照命中时是 O(1)；它的唯一隐患（TTL 惊群）已并入 P1-1 统一解决；
- **weighted 其余候选排序**（eval.go:229-250）：entries 数=规则里的目标数（个位），SliceStable 微不足道。

---

## 7. 度量与验证

**本轮已落地的微基准**（保留在库中，供实施前后对比）：

```powershell
$env:GOROOT='D:\Go'
go test ./internal/service/ -bench BenchmarkRouteHotPath -benchmem -run '^$' -count 3
go test ./internal/routelang/ -bench BenchmarkRoutelang    -benchmem -run '^$' -count 3
```

基线数据（2026-02，i5-11300H，go1.27.0 windows/amd64）：

| 基准 | ns/op | B/op | allocs/op |
|---|---|---|---|
| Digest 100KB | 543,981 | 205,804 | 2,056 |
| Digest 1MB | 3,927,934 | 1,238,247 | 2,344 |
| ParseMeta 100KB | 344,563 | 204 | 2 |
| ParseMeta 1MB | 3,401,772 | 217 | 2 |
| RoundTrip(Unmarshal+Marshal) 100KB | 828,135 | 353,662 | 2,071 |
| RoundTrip 1MB | 7,021,911 | 5,351,496 | 2,391 |
| rand.New(NewSource)+Float64 | 9,344 | 5,376 | 1 |
| rand/v2 式包级 Float64（对照） | 9.1 | 0 | 0 |
| routelang Compile（典型三段规则） | 8,013 | 9,651 | 84 |
| Eval priority / weighted | 39 / 210 | 48 / 144 | 1 / 5 |

**功能回归**（每批必跑）：`go vet ./...`、`go test ./...`（根包 cgo 测试含路由 failover 三件套）、`scripts/smoke.go`；发版前 `scripts/abi-smoke.c` 符号校验。UI 不做浏览器自动化，devserver 目验。

**实施顺序建议**：
1. 第一批 P0-1 + P0-2（互相独立、均为小改，合计把路由路径解析次数拉回直连基线）；
2. 第二批 P0-3 阶段一（循环外提）可与第一批同车；
3. 第三批 P1-1 / P1-2 / P1-3（互相独立）；
4. P0-3 阶段二（字节定点改写）单独评审单独提交；P2 按观测决定。

**度量口径**：以 §7 基准的前后对比为主（Digest/RoundTrip/RandSource 三项应分别归零/近半/归零）；端到端用 seed 数据集在 devserver 下压 1000 次别名请求对比墙钟与 GC 次数（`GODEBUG=gctrace=1`）。

**明确不做的事**：不改路由语言语义与 failover/冷却/计价口径、不动单写者架构与结算事务边界（除 P1-3 的对称扩展）、不为路由引入第三方 JSON 库（阶段二的定点改写坚持 stdlib Token 流实现）。

---

## 附：证据索引（主要发现的 file:line 汇总）

| 发现 | 位置 |
|---|---|
| RequestDigest 急切求值、无 UsesAI 门控 | main_routes.go:49；routes.go:521-529 |
| harvestTexts 逐层 sort.Strings | routes.go:554-558 |
| 路由路径第二次 ParseRequestMeta | requestpath.go:250（经 main_routes.go:62） |
| preparePayload 整包往返、逐候选重复 | main_routes.go:174-197；257-259（非流式循环）、300（流式拨号） |
| 每请求 rand 源构造 | routes.go:261；使用点 eval.go:211-214 |
| 快照重载无 single-flight（含 pricingRules 同款） | routes.go:89-111；service.go:625-635 |
| Settle 每请求 MatchRoute | service.go:466 |
| quota.reserve 审计独立写事务 | service.go:452-454；store/quota.go:405-407 |
| HoldReservation 后冗余 GetReservation 读 | store/quota.go:196 |
| filterCooldown 临界区内字符串拼键 | routes.go:157-159,186-199 |
| 认领超集注册 | main_routes.go:87-96；main.go:1577-1609 |
| markRef 死存储 | routelang/parser.go:464 |
| v8 表与 alias NOCASE 唯一索引 | store/migrate.go:362-381 |
| 管理端点全量重编译（确认为冷路径可接受） | httpapi/modelroutes.go:14-27；routes.go:114-125 |
