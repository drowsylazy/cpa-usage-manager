# CPA Usage Manager

<p align="center">
  <img src="logo.svg" alt="CPA Usage Manager" width="96">
</p>

[CPA Usage Manager](https://github.com/drowsylazy/cpa-usage-manager) 是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 Go 插件（`c-shared` 动态库），为宿主提供**密钥额度管理 + 用量统计 + 模型路由**三合一能力，并自带一个内嵌的简体中文管理面板。

- **额度即服务**：给每个使用者签发独立的 `cum-` 插件密钥，按金额 / Token / 请求次数三族限额精确管控花费，请求前严格预占（fail-closed），超额直接拒绝而不是事后追账。
- **看得见的用量**：逐请求明细（含失败原因与状态码）、分钟级聚合、趋势图、维度统计、费用与价格覆盖率，全部落在本机 SQLite，不依赖任何外部服务。
- **集合别名与规则路由**：把多个上游模型组合成一个别名，用规则脚本按请求特征（大小、时间、是否 agent 流量、哪个密钥……）自动选路，失败自动切换目标、目标冷却与健康视图。
- **出事能排查**：失败请求带状态码与错误摘要、目标健康面板、错误率告警、进行中请求视图——「为什么失败 / 为什么绕开了某个目标 / 服务坏了没」在面板里都有答案。

## 功能一览

| 模块 | 能力 |
| --- | --- |
| 密钥管理 | 签发 / 轮换 / 撤销 / 删除 / 明文回显（管理密钥保护）；标签、归属（caller）、有效期、可用模型白名单、最大并发 |
| 额度限额 | 金额（USD）与 Token 二选一：总 / 日 / 周 / 月四档；请求次数（日 / 月）独立附加；周期按本地时区或 UTC 滚动归零 |
| 计价 | 统一规则表（exact / glob / regexp + 优先级），对输入 / 输出 / 缓存读 / 缓存写四类 Token 计价；支持人民币规则（保存时锁定汇率）；一键同步 [models.dev](https://models.dev) 价格簿；内置计价试算器 |
| 用量统计 | 逐请求明细（延迟 / 首字延迟 / TPS / 缓存命中率 / 失败原因）、分钟聚合、趋势堆叠图、模型 / 密钥 / 上游路由等八个维度、上游二次路由透视 |
| 模型路由 | 集合别名 + 自研规则语言（`priority` / `weighted` / `ai_judge`）、failover 自动换目标、失败目标冷却与恢复、规则干跑测试、目标健康面板、别名自动注册进 `/v1/models` |
| 通知报告 | 经 [shoutrrr](https://github.com/nicholas-fedor/shoutrrr) 推送到 Telegram / Discord / 飞书 / ntfy / webhook 等：额度越线、密钥过期与临期预警、错误率告警、单请求费用异常、系统错误；日 / 周 / 月定期报告（含环比） |
| 运维 | CSV / PNG 导出、数据库备份恢复（上限可配）、每日自动备份、保留期自动清理、对账去重、进行中请求视图、gzip 压缩 |

## 安装

### 从 Release 下载（推荐）

到 [Releases](https://github.com/drowsylazy/cpa-usage-manager/releases) 下载对应平台的压缩包并解压到 CLIProxyAPI 的插件目录：

| 文件 | 平台 |
| --- | --- |
| `cpa-usage-manager_<版本>_windows_amd64.zip` | Windows x64 |
| `cpa-usage-manager_<版本>_linux_amd64.zip` | Linux x64 |
| `cpa-usage-manager_<版本>_linux_arm64.zip` | Linux ARM64 |
| `cpa-usage-manager_<版本>_darwin_arm64.zip` | macOS Apple Silicon |

每个 Release 均附 `checksums.txt`。发布产物由 CI 自动构建并通过 ABI 符号自检。

### 从源码构建

需要 Go 1.26+ 与 C 编译器（Windows 用 MinGW-w64），构建产物按平台命名为 `cpa-usage-manager.dll` / `.so` / `.dylib`：

```powershell
# Windows (PowerShell)
.\scripts\build.ps1
```

```bash
# Linux / macOS
./scripts/build.sh
```

构建后放到宿主的 `plugins/<goos>/<goarch>/` 目录（仓库内的 `scripts/deploy.ps1` 可一键部署），重启宿主生效。

## 快速上手

**1. 写插件配置**（宿主 `config.yaml`，全部字段可省略）：

```yaml
plugins:
  items:
    cpa-usage-manager:
      config: |
        quota:
          enabled: true        # 接管前端鉴权；false = 纯统计模式
```

**2. 打开面板**：重启宿主后登录管理界面，侧栏出现「CPA 用量管理」，共 6 个页签：概览 / 密钥 / 用量 / 价格 / 模型集合 / 系统。

**3. 签发第一枚密钥**：在「密钥」页签点「签发密钥」，按需填标签与限额，得到形如 `cum-<kid>-<secret>` 的插件密钥。**明文只显示这一次**，请立即复制保存。

**4. 接入客户端**：把客户端的 Base URL 指向宿主、API Key 换成这枚 `cum-` 密钥。此后每笔请求都会经过：鉴权 → 额度预占 → 上游执行 → 按实际用量结算。

**5. 配置计价**（可选）：默认所有模型免费计费。「价格」页签可手动添加规则、从 models.dev 搜索引入、或用「试算」按钮估算指定用量的费用；无规则命中时的策略由 `pricing.unknown_policy` 控制。

**6.（可选）配模型集合**：在「模型集合」页签创建别名（如 `auto`），用规则脚本映射目标链，别名会自动注册进宿主的 `/v1/models`；「目标健康」面板实时展示各目标的冷却状态与近期失败。

## 配置参考

配置由宿主以内联 YAML 注入（也可经 `config_file` 或环境变量 `CPA_USAGE_MANAGER_CONFIG_FILE` 指向外部文件）。以下为默认值：

```yaml
data_dir: ./data/cpa-usage-manager   # 数据目录（0700）：数据库 + pepper 文件
database_file: cpa-usage-manager.db
busy_timeout: 5s
retention_days: 365                  # 逐请求明细与分钟聚合的保留天数
audit_retention_days: 90             # 内部审计留痕保留天数（0=跟随 retention_days）

quota:                               # 额度子系统
  enabled: true                      # false = 退回纯统计模式（不鉴权、只被动记录用量）
  cycle_offset_minutes: 0            # 日/周/月周期相对 UTC 的偏移；480 = UTC+8，本地零点归零
  keys:
    pepper_env: CPA_USAGE_MANAGER_KEY_PEPPERS   # pepper 环境变量名（优先）
    pepper_file: key-peppers                    # 其次 data_dir 下的文件；都没有则自动生成
    active_pepper_id: active                    # 签发新密钥使用的 pepper 代际（支持轮换）
  limits:
    max_token_estimate: 1000000       # 单请求预占 Token 严格上限
    default_output_reserve: 4096      # 请求未带 max_tokens 时的输出预占
    require_estimate: false
  settlement:
    missing_usage: settle_reserved    # 上游未回用量时：settle_reserved 按预占扣费 | release 释放
    host_usage_wait: 1500ms           # 流式兜底等待宿主用量回调的窗口（非流式不等待）
  stream:
    stale_reservation_timeout: 2h     # 无心跳在途预占自动释放

pricing:
  unknown_policy: allow               # 无计价规则命中：deny 拒绝 | allow 免费 | default 用兜底规则
  models_dev_sync:
    enabled: true
    provider_priority: []             # 提供方优先级
    ignore_suffixes: []               # 忽略的模型名后缀
    model_mappings: {}                # 显式模型名映射

backup:                               # 每日自动备份（默认关闭）
  enabled: false
  dir: backups                        # 相对 data_dir；快照不含 key-peppers，请一并备份
  keep: 7                             # 保留份数
  hour: 4                             # 每日触发的本地小时；重启后当天已过点会自动补一份
  max_bytes: 268435456                # 备份/恢复的单文件上限（默认 256MiB，可按需调大）

response_compression: true            # 管理面板响应 gzip
response_compression_min_bytes: 1024
```

汇率说明：USD→CNY 汇率由插件后台自动刷新（每 30 分钟，双源 + 兜底），用于人民币计价规则的保存时锁定与面板显示切换；账本与额度口径恒为 micro-USD。

### Pepper 与密钥安全

- 密钥明文只在**签发 / 轮换**时返回一次；数据库只存 HMAC 哈希、AES-GCM 密文和短指纹。
- Pepper 从环境变量（`quota.keys.pepper_env`）读取，或落在 `data_dir/key-peppers`（0600），首次启动自动生成。**备份数据目录时必须连同 pepper 一起备份**，否则密文无法解密。
- 不存储、不回显任何上游（OAuth / API Key）凭据，认证字段只保存清洗后的展示信息。

## 模型路由（集合别名）

在「模型集合」页签为别名（如 `auto`）编写规则脚本，把请求按特征路由到不同目标链：

```bash
when input_tokens <= 8000
  -> weighted { "gpt-4o-mini": 3, "deepseek-chat": 1 }   # 加权随机，选中者排首
when ai_judge(["simple", "hard"]) == "hard"
  -> priority ["claude-opus-4", "gemini-2.5-pro"]        # 声明序即回退顺序
-> "claude-sonnet-4"                                     # 无条件兜底（必填，最后一条）
```

- **可用变量**：`input_tokens`、`body_len`、`model`、`stream`、`thinking_effort`、`source`、`hour`、`weekday`、`has_tools`、`has_system`、`kid`、`key_label`、`caller_id`。
- **failover**：可重试失败（401/402/403/408/429/5xx、连接类错误）自动换下一健康目标；失败目标冷却 `cooldown_seconds` 秒，恢复的目标准确后立即重新参与选举，429 的 `Retry-After` 也会被尊重。流式请求仅在首字节前切换。
- **目标健康**：模型集合页实时展示各目标的冷却状态与最近 60 分钟失败统计；目标转移轨迹随请求行落在「原因」列。
- **ai_judge**：用一个小模型对请求做难度分级（发送脱敏摘要，绝不发送原始请求体），结果缓存 10 分钟；评判失败自动回落兜底分支。规则保存前可在「测试此规则」里干跑验证。
- **计价**：按集合二选一——按实际成功目标计价（默认）或按别名固定声价；维度统计恒记别名。

完整语法、变量口径与运行时行为见 **[docs/routing.md](docs/routing.md)**。

## 通知与报告

在「系统」页签配置 [shoutrrr](https://github.com/nicholas-fedor/shoutrrr) 通知端点（如 `telegram://…`、`lark://…`），支持多端点同时推送：

- **额度告警**：任一限额档余量低于阈值百分比或用尽时推送一次，余量回升后自动重新武装；
- **密钥告警**：到期前 N 天临期预警（可配）与过期告警；
- **错误率告警**：滑动窗口内失败占比超过阈值即推送（全局视角，恢复后重新武装）；
- **单请求异常**：单笔请求费用 / Token 超过设定阈值立即推送（每小时每密钥限一条）；
- **系统错误**：备份失败、存储降级等；
- **定期报告**：日 / 周 / 月频率，自由组合汇总（含环比）、失败明细、模型与密钥 Top N 等板块，覆盖上一个完整周期。

## 数据与备份

- 单一 SQLite 数据库（纯 Go 驱动），WAL + 单写者租约，多进程部署时自动完成租约交接。
- 逐请求明细与分钟聚合按 `retention_days` 自动清理；密钥、计价规则等策略数据长期保留。
- 「系统」页签可手动下载备份 / 恢复快照；开启 `backup.enabled` 后每日自动落一份本地快照并按 `keep` 轮转。备份/恢复的单文件上限由 `backup.max_bytes` 控制（默认 256MiB）。
- **升级注意**：恢复快照时 schema 版本强校验——旧版本插件不能恢复新版本备份，请先升级插件再恢复。

## 面向开发者

```bash
go test ./...                  # 全量测试
go vet ./...                   # 静态检查
go run ./scripts/smoke.go      # 端到端冒烟
```

- `scripts/devserver.go` + `scripts/seed.go`：本地起一个带拟真数据的管理面板（<http://127.0.0.1:18080/console>，密钥 `dev-secret`，可用 `CPA_DEV_DATA_DIR` 指定数据目录），调 UI 不用连宿主。
- `scripts/abi-smoke.c`：校验动态库导出符号。
- 架构与设计决策见 [DESIGN.md](DESIGN.md)；路由规则手册见 [docs/routing.md](docs/routing.md)。

## 常见问题

**签发的密钥明文忘了怎么办？**
面板无法找回明文（库里只有哈希与密文）。用「轮换」换一枚新密钥并更新客户端，或重新签发后删除旧密钥。

**pepper 没有随数据目录备份，恢复后密钥不能用？**
`key-peppers` 文件必须与数据库一起备份。丢失后：哈希校验仍可用（Key 还能鉴权），但「明文回显」与通知 URL 解密失效，只能重新签发密钥。

**`quota.enabled=false` 是什么行为？**
纯统计模式：插件不接管鉴权、不执行请求，仅被动记录宿主上报的用量（无额度扣减）。

**别名建好了但没进 `/v1/models`？**
注册触发点为宿主启动 / 配置重载等时机；新建集合后若未出现，重载一次宿主配置即可（详见 docs/routing.md 的注册链路说明）。

**美元和人民币怎么算的？**
账本与所有限额恒为 micro-USD。人民币计价规则按**保存时锁定**的汇率折算美元入账（改价需重存规则）；面板顶栏的显示币种只影响展示。

**旧版本插件的备份能直接恢复吗？**
不能跨大版本降级恢复：恢复时做 schema 版本强校验，备份版本高于当前插件会被拒绝。先升级插件再恢复。

## 许可证

[MIT](https://github.com/drowsylazy/cpa-usage-manager/blob/main/LICENSE)
