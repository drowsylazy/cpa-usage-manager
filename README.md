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
usage / usage/summary / usage/dimension / requests / trends / costs / balance
audit / auth-quotas / preferences / exchange-rate
export/csv / export/png / backup / restore / reset / maintain
```

资源路由仅 `/v0/resource/plugins/cpa-usage-manager/console`（SPA HTML 壳，不含业务数据）。

插件 Key 格式为 `cum-<kid>-<secret>`。数据库只保存 pepper HMAC 哈希、AES-GCM 密文和指纹；明文只在签发、轮换或受管理密钥保护的 reveal 操作中返回。敏感响应使用 `Cache-Control: no-store`。