<div align="center">

<img src="logo.svg" alt="CPA Usage Manager" width="112">

# CPA Usage Manager

**CLIProxyAPI 的密钥额度 · 用量统计 · 模型路由一体化插件**

[![Release](https://img.shields.io/github/v/release/drowsylazy/cpa-usage-manager?style=flat-square&label=%E5%8F%91%E7%89%88&logo=github&color=blue)](https://github.com/drowsylazy/cpa-usage-manager/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/drowsylazy/cpa-usage-manager/ci.yml?style=flat-square&label=CI&logo=githubactions&logoColor=white)](https://github.com/drowsylazy/cpa-usage-manager/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/drowsylazy/cpa-usage-manager?style=flat-square&label=%E8%AE%B8%E5%8F%AF%E8%AF%81&color=blue)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/%E5%B9%B3%E5%8F%B0-Windows%20%7C%20Linux%20%7C%20macOS-6c7a89?style=flat-square)](https://github.com/drowsylazy/cpa-usage-manager/releases)

[功能特性](#%E2%9C%A8-%E5%8A%9F%E8%83%BD%E7%89%B9%E6%80%A7) · [快速上手](#%F0%9F%9A%80-%E5%BF%AB%E9%80%9F%E4%B8%8A%E6%89%8B) · [配置参考](#%E2%9A%99%EF%B8%8F-%E9%85%8D%E7%BD%AE%E5%8F%82%E8%80%83) · [模型路由](#%F0%9F%94%80-%E6%A8%A1%E5%9E%8B%E8%B7%AF%E7%94%B1) · [常见问题](#%E2%9D%93-%E5%B8%B8%E8%A7%81%E9%97%AE%E9%A2%98)

</div>

---

## ✨ 功能特性

### 🔑 密钥额度

为每个使用者签发独立的 `cum-` 插件密钥，请求前**严格预占**（fail-closed），超额直接拒绝而非事后追账。

- **三族限额**：金额（USD）/ Token 二选一，附以请求次数（日 / 月），总 / 日 / 周 / 月四档周期独立滚动
- **周期时区**：支持按本地时区归零（`cycle_offset_minutes`），日限额在你当地的零点重置
- **并发管控**：每密钥最大并发限制，进行中请求实时可见
- **明文可找回**：密钥详情「查看明文」随时解密回显完整 `cum-` 密钥用于配置客户端，忘记明文无需重新签发
- **密钥安全**：明文仅签发时返回一次，库中只存 HMAC 哈希 + AES-GCM 密文 + pepper，解密回显需宿主管理密钥

### 📊 用量统计

逐请求明细到趋势聚合，全部落在本机 SQLite，**零外部依赖**。

- **逐请求记录**：延迟（首字 / 总）、TPS、缓存命中率、失败原因与状态码
- **趋势与维度**：分钟级聚合堆叠趋势图，模型 / 密钥 / 来源等八维度透视
- **上游路由透视**：别名 × 实际命中模型，二次路由一目了然
- **费用口径**：输入 / 输出 / 缓存读 / 缓存写四类 Token 计价；美元与人民币规则分开入账（CNY 规则的原生金额恒人民币），美元等值按实时汇率折算
- **导出**：请求明细 / 维度 / 趋势数据 CSV，趋势图 PNG

### 🔀 模型路由

把多个上游模型组合成一个集合别名，用规则脚本按请求特征自动选路。

```bash
when input_tokens <= 8000
  -> weighted { "gpt-4o-mini": 3, "deepseek-chat": 1 }   # 加权随机，选中者排首
when ai_judge(["simple", "hard"]) == "hard"
  -> priority ["claude-opus-4", "gemini-2.5-pro"]        # 声明序即回退序
-> "claude-sonnet-4"                                     # 无条件兜底（必填）
```

- **failover**：可重试失败自动换下一健康目标，失败目标冷却，`Retry-After` 被尊重
- **ai_judge**：小模型难度分级（脱敏摘要），结果缓存，失败自动回落兜底
- **干跑测试**：规则保存前在编辑器内实测候选链与变量快照
- **目标健康**：实时冷却状态与近 60 分钟失败统计
- 自动注册进宿主 `/v1/models`，完整语法见 **[docs/routing.md](docs/routing.md)**

### 🔔 通知报告

经 [shoutrrr](https://github.com/nicholas-fedor/shoutrrr) 推送，多端点并发（Telegram / Discord / 飞书 / ntfy / webhook…）。

- 额度越线 / 用尽（边沿触发，余量回升重新武装）· 密钥临期与过期 · 错误率滑动窗口告警 · 单请求费用异常
- 日 / 周 / 月定期报告：汇总（含环比）、失败明细、模型与密钥 Top N 板块自由组合

## 📦 安装

### 从 Release 下载（推荐）

到 [Releases](https://github.com/drowsylazy/cpa-usage-manager/releases) 下载对应平台的压缩包，解压到宿主插件目录 `plugins/<goos>/<goarch>/`，重启宿主生效。每个 Release 附 `checksums.txt`，产物由 CI 自动构建并通过 ABI 符号自检。

| 压缩包 | 平台 |
| --- | --- |
| `cpa-usage-manager_<版本>_windows_amd64.zip` | Windows x64 |
| `cpa-usage-manager_<版本>_linux_amd64.zip` | Linux x64 |
| `cpa-usage-manager_<版本>_linux_arm64.zip` | Linux ARM64 |
| `cpa-usage-manager_<版本>_darwin_arm64.zip` | macOS Apple Silicon |

### 从源码构建

需要 Go 1.26+ 与 C 编译器（Windows 用 MinGW-w64）：

```powershell
# Windows (PowerShell)
.\scripts\build.ps1
```

```bash
# Linux / macOS
./scripts/build.sh
```

## 🚀 快速上手

**1. 启用插件**（宿主 `config.yaml`，全部字段可省略）：

```yaml
plugins:
  items:
    cpa-usage-manager:
      config: |
        quota:
          enabled: true        # 接管前端鉴权；false = 纯统计模式
```

**2. 打开面板**：重启宿主后登录管理界面，侧栏出现「CPA 用量管理」——概览 / 密钥 / 用量 / 价格 / 模型集合 / 实时 / 系统 七个页签。

**3. 签发密钥**：「密钥」页签 → 签发密钥 → 填标签与限额 → 得到 `cum-<kid>-<secret>`。**明文只显示这一次**，立即保存；之后忘了也不怕，密钥详情的「查看明文」可随时解密回显。

**4. 接入客户端**：客户端 Base URL 指向宿主，API Key 换成这枚 `cum-` 密钥。每笔请求经过：鉴权 → 额度预占 → 上游执行 → 按实际用量结算。

**5. 配置计价**（可选）：默认全模型免费。「价格」页签手动添加规则、从 [models.dev](https://models.dev) 搜索引入、或用内置试算器估算费用。

**6. 配模型集合**（可选）：创建别名（如 `auto`）编写规则脚本，见[模型路由](#%F0%9F%94%80-%E6%A8%A1%E5%9E%8B%E8%B7%AF%E7%94%B1)章节或完整手册 [docs/routing.md](docs/routing.md)。

## ⚙️ 配置参考

配置由宿主以内联 YAML 注入（也可经 `config_file` 或环境变量 `CPA_USAGE_MANAGER_CONFIG_FILE` 指向外部文件）。以下为默认值：

```yaml
data_dir: ./data/cpa-usage-manager   # 数据目录（0700）：数据库 + pepper 文件
database_file: cpa-usage-manager.db
busy_timeout: 5s
retention_days: 365                  # 逐请求明细与分钟聚合的保留天数
audit_retention_days: 90             # 内部审计留痕保留天数（0=跟随 retention_days）

quota:                               # 额度子系统
  enabled: true                      # false = 纯统计模式（不鉴权、只被动记录用量）
  cycle_offset_minutes: 0            # 周期偏移；480 = UTC+8，日限额本地零点归零
  keys:
    pepper_env: CPA_USAGE_MANAGER_KEY_PEPPERS   # pepper 环境变量名（优先）
    pepper_file: key-peppers                    # 其次 data_dir 下的文件；都没有则自动生成
    active_pepper_id: active                    # 签发新密钥的 pepper 代际（支持轮换）
  limits:
    max_token_estimate: 1000000       # 单请求预占 Token 严格上限
    default_output_reserve: 4096      # 请求未带 max_tokens 时的输出预占
    require_estimate: false
  settlement:
    missing_usage: settle_reserved    # 上游未回用量（仍有响应数据）：settle_reserved 按预占扣费 | release 释放；上游未产生任何响应数据（HTTP 错误/空响应）时一律零成本结算
    host_usage_wait: 1500ms           # 流式兜底等待宿主用量回调的窗口（非流式不等待）
  stream:
    stale_reservation_timeout: 2h     # 无心跳在途预占自动释放

pricing:
  unknown_policy: allow               # 无计价规则命中：deny 拒绝 | allow 免费 | default 兜底规则
  models_dev_sync:
    enabled: true
    provider_priority: []             # 提供方优先级
    ignore_suffixes: []               # 忽略的模型名后缀
    model_mappings: {}                # 显式模型名映射

backup:                               # 每日自动备份（默认关闭）
  enabled: false
  dir: backups                        # 相对 data_dir；快照不含 key-peppers，请一并备份
  keep: 7                             # 保留份数
  hour: 4                             # 每日触发的本地小时
  max_bytes: 268435456                # 备份/恢复单文件上限（默认 256MiB）

response_compression: true            # 管理面板响应 gzip
response_compression_min_bytes: 1024
```

> 💡 **美元与人民币是分开的**：计价规则自带币种——USD 规则价格按美元、CNY 规则按人民币（每百万 Token 填写）。CNY 规则的请求行按**人民币原值入账**（`cost_native` 恒 micro-CNY），不经过任何折算；其美元等值（`cost_usd`）仅用于额度扣减与跨币种聚合，按**当前实时汇率**折算（后台每 30 分钟自动刷新，不随规则保存锁定）。汇率行情变化只影响美元等值的换算，不影响人民币账面。

### 🔐 Pepper 与密钥安全

- 密钥明文在签发 / 轮换时返回一次；需要时可随时在密钥详情「查看明文」解密回显（AES-GCM），用于配置客户端。
- Pepper 读自环境变量或 `data_dir/key-peppers`（0600），首次启动自动生成。**备份数据目录必须连同 pepper 一起备份**，否则密文无法解密、明文回显失效。
- 不存储、不回显任何上游凭据，认证字段只保存清洗后的展示信息。

## 🔀 模型路由

在「模型集合」页签为别名编写规则脚本，把请求按特征路由到目标链：

- **变量**：`input_tokens`、`body_len`、`model`、`stream`、`thinking_effort`、`source`、`hour`、`weekday`、`has_tools`、`has_system`、`kid`、`key_label`、`caller_id`
- **failover**：401/402/403/408/429/5xx 与连接类错误自动换下一健康目标；失败目标冷却 `cooldown_seconds` 秒；流式请求仅首字节前切换；429 的 `Retry-After` 被尊重
- **冷却兜底**：全部目标冷却时按 `cooldown_policy` 拒绝（block，默认）或忽略冷却照打（force）
- **计价**：按实际成功目标（默认）或按别名声价，二选一
- **目标健康面板**：冷却剩余时间与近 60 分钟失败统计，回答「为什么绕开了某个目标」

完整语法、变量口径与运行时行为见 **[docs/routing.md](docs/routing.md)**。

## 💾 数据与备份

- 单一 SQLite（纯 Go 驱动），WAL + 单写者租约，多进程部署自动完成租约交接
- 明细与聚合按 `retention_days` 自动清理；密钥、计价规则等策略数据长期保留
- 「系统」页手动下载 / 恢复快照；`backup.enabled` 开启后每日自动落盘并按 `keep` 轮转
- 恢复时 schema 版本强校验——旧插件不能恢复新版本备份，请先升级再恢复

## 🛠️ 面向开发者

```bash
go test ./...                  # 全量测试
go vet ./...                   # 静态检查
go run ./scripts/smoke.go      # 端到端冒烟
```

- `scripts/devserver.go` + `scripts/seed.go`：本地起一个带拟真数据的管理面板（<http://127.0.0.1:18080/console>，密钥 `dev-secret`），调 UI 不用连宿主
- `scripts/abi-smoke.c`：校验动态库导出符号
- 架构与设计决策见 [DESIGN.md](DESIGN.md)；路由规则手册见 [docs/routing.md](docs/routing.md)

## ❓ 常见问题

<details>
<summary><b>签发的密钥明文忘了怎么办？</b></summary>

打开面板「密钥」页签 → 该密钥的详情 → **查看明文**，解密回显完整的 `cum-<kid>-<secret>` 即可用于配置客户端（需要宿主管理密钥）。若 pepper 文件丢失则解密失效，此时用「轮换」换一枚新密钥并更新客户端。
</details>

<details>
<summary><b>pepper 没有随数据目录备份，恢复后密钥不能用？</b></summary>

`key-peppers` 必须与数据库一起备份。丢失后：哈希校验仍可用（Key 还能鉴权），但「明文回显」与通知 URL 解密失效，只能重新签发密钥。
</details>

<details>
<summary><b><code>quota.enabled=false</code> 是什么行为？</b></summary>

纯统计模式：插件不接管鉴权、不执行请求，仅被动记录宿主上报的用量（无额度扣减）。
</details>

<details>
<summary><b>别名建好了但没进 <code>/v1/models</code>？</b></summary>

注册触发点为宿主启动 / 配置重载等时机；新建集合后若未出现，重载一次宿主配置即可。
</details>

<details>
<summary><b>美元和人民币怎么算的？</b></summary>

美元与人民币**分开入账**：CNY 计价规则的价格按人民币填写、请求行按人民币原值入账（`cost_native` 恒 micro-CNY），不经折算。其美元等值（`cost_usd`）仅供额度扣减与跨币种聚合，按**当前实时汇率**折算（每 30 分钟自动刷新，不锁定）；面板顶栏的显示币种只影响展示。
</details>

<details>
<summary><b>旧版本插件的备份能直接恢复吗？</b></summary>

不能跨版本降级恢复：恢复时做 schema 版本强校验，备份版本高于当前插件会被拒绝。先升级插件再恢复。
</details>

## 📄 许可证

本项目以 [MIT](./LICENSE) 协议开源。

---

<div align="center">

**如果这个项目帮到了你，欢迎给一个 ⭐ Star**

</div>
