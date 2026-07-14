# Inventory Istio 静态候选

方案 A 已获批准，但本目录当前只保存独立静态候选，**不属于普通 online 发布输入**。
`../kustomization.yaml`、`tools/scripts/start.ps1` 和普通 `../netpol.yaml` 均不得引用或动态拼接本目录、
`../mesh-shared-identity/` 或 `../ds-terminal-mesh/`；不要直接 `kubectl apply` 这些静态候选。

- `identity/`：五个专属 ServiceAccount 与六个 Inventory 内部 workload 的 revision-only sidecar patch；
  battle-result ServiceAccount 由共享 component 唯一声明。
- `gate/`：namespace revision 与内部/edge Deployment→ReplicaSet→Pod admission gate。
- `observe/`：PERMISSIVE 与 dry-run exact policy。
- `enforce/`：STRICT 与 active exact policy。
- `network/`：Inventory 专用 L4 allow，并从普通业务宽入口中排除 Inventory。

`../mesh-shared-identity/` 唯一拥有 battle-result ServiceAccount；Inventory 或
`../ds-terminal-mesh/` 候选组装时都必须显式引入它一次。DS-terminal 自行声明两端 revision patch，
不依赖本目录的 `identity/`；契约测试会同时组合两份候选并锁定唯一 ServiceAccount。

本地只读验证：

```powershell
pwsh tools/scripts/tests/inventory_mesh_contract_test.ps1
pwsh tools/scripts/tests/ds_terminal_mesh_contract_test.ps1
pwsh tools/scripts/tests/online_manifest_contract_test.ps1
```

有明确的非生产 Kubernetes 1.30+ 测试 context 时，再让真实 API server 编译全部 CEL（server dry-run
不持久化对象）：

```powershell
pwsh tools/scripts/tests/inventory_mesh_contract_test.ps1 `
  -ServerDryRun -KubeContext <explicit-test-context>
```

重新接入普通发布前，必须完成 `docs/design/decision-revisit-internal-service-auth.md` §9 记录的真实
Kubernetes/Istio/外部 edge、v2 双证据与分阶段激活验收，并重新 review 接线。
