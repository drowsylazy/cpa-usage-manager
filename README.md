# cpa-usage-manager

面向 CLIProxyAPI 的 Go `c-shared` 插件。当前实现覆盖 P0–P4 全部功能与 P5 的工程化脚本：

- P0–P2：配置、单一 SQLite 存储（WAL + 单写者租约）、货币与计价、usage 解析、fx
- P3：c-shared ABI、管理 API 全量路由、密钥/额度/计价/审计/备份恢复/导出
- P4：`/console` 完整单文件 SPA（7 页签、图表、多语言、密钥全生命周期管理）
- P5：四平台构建脚本、CI 发布、`scripts/abi-smoke.c` 导出符号校验

## 本地验证

```powershell
$env:GOROOT='D:\Go'   # 若 GOROOT 被误设
go test ./...
go vet ./...
go run ./scripts/smoke.go
```

发布动态库必须安装 C 编译器（如 MinGW-w64）并设置 `CGO_ENABLED=1`，使用 `scripts/build.ps1` 或 `scripts/build.sh`。产物名为 `cpa-usage-manager.dll`、`cpa-usage-manager.so` 或 `cpa-usage-manager.dylib`。

构建后可用 `scripts/abi-smoke.c` 校验导出符号：

```powershell
gcc -O2 -o abi-smoke.exe scripts/abi-smoke.c
.\abi-smoke.exe .\cpa-usage-manager.dll
```

## 管理 API

前缀 `/v0/management/plugins/cpa-usage-manager/*`，全部经宿主管理密钥鉴权：

```
health / overview / callers / callers/enabled
keys / keys/issue / keys/update / keys/rotate / keys/reveal / keys/revoke / keys/delete
pricing / pricing/delete / pricing/sync
model-routes / model-routes/save / model-routes/delete / model-routes/judge
usage / usage/summary / usage/dimension / requests / trends / costs / balance
audit / auth-quotas / preferences / exchange-rate
export/csv / export/png / backup / restore / reset / maintain
```

资源路由仅 `/v0/resource/plugins/cpa-usage-manager/console`（SPA HTML 壳，不含业务数据）。

插件 Key 格式为 `cum-<kid>-<secret>`。数据库只保存 pepper HMAC 哈希、AES-GCM 密文和指纹；明文只在签发、轮换或受管理密钥保护的 reveal 操作中返回。敏感响应使用 `Cache-Control: no-store`。

## 模型路由（集合别名）

面板「模型集合」页签定义集合别名（如 `auto`），用自研规则脚本把别名映射到有序目标链：

```
# 注释
when input_tokens <= 8000
  -> weighted { "gpt-4o-mini": 3, "deepseek-chat": 1 }   # 加权随机，选中者排首，其余按权重降序跟随
when ai_judge(["simple", "hard"]) == "hard"
  -> priority ["claude-opus-4", "gemini-2.5-pro"]        # 声明序即回退顺序
-> "claude-sonnet-4"                                     # 无条件兜底；必填且只能是最后一条
```

- 变量：`input_tokens`（body 字符数/2+1 封顶估算）、`body_len`、`model`（剥思考后缀）、`stream`、`thinking_effort`、`source`。无循环无赋值，求值必然终止。
- **failover 与冷却**：可重试失败（401/402/403/408/429/5xx/连接类错误；404 除 Responses 存储类引用外可转）自动换下一健康目标；失败目标进程内冷却 `cooldown_seconds`（默认 60）。流式仅在首字节前切换。
- **ai_judge**：评判模型在「AI 评判设置」配置（存偏好 KV），经宿主正常转发计费；发送脱敏摘要（结构化指标 + 对话文本前 2000 字符），结果缓存 10 分钟；失败/超时回落兜底分支并记审计。
- **落库与计价**：一次别名请求落单行——`model`=别名、`upstream_model`=实际成功目标；中间尝试只记审计 `route.failover`。计价模式按集合二选一：`target` 按最终成功目标规则（默认）/ `alias` 按别名声价。维度统计恒记别名。
- **暴露到 `/v1/models`**：启用中的别名经 `model_registrar` 能力位 + `model.register` 方法注册进宿主模型列表（需要较新宿主；旧宿主忽略该能力位，功能静默降级）。宿主在启动、config.yaml 重载、auth 文件变更或管理端保存配置时同步——新建集合后触发其一即可生效。

已知限制：冷却状态进程内保存（重启丢失、多实例各自独立，failover 链本身兜底）；`input_tokens` 为粗估上界（CJK 文本偏低 1.5~2x）；别名流量仅面向插件 Key（cum-），原生 Key 打别名走宿主原生路径。