# 本地 Agones dev 联调（minikube + Agones）

> W4 ⑬（2026-06-09）。把 ds_allocator / hub_allocator 从 mock 地址推进到本地 Agones 联调。
>
> ⚠️ **AGENTS.md §3 / §11.1**：本目录里所有「安装工具 / 起 minikube / helm 装 Agones / 拉镜像 /
> 启重服务」的命令**由 Codex / 用户执行**，Claude 只负责写清单 + 风险 + 验收标准。
> apply 业务 manifest（Fleet / RBAC）属本地 dev 集群操作，也由 Codex 执行。

---

## 0. 两种 DS 模型（先理解再联调）

| | 战斗 DS（ds_allocator） | 大厅 Hub DS（hub_allocator） |
|---|---|---|
| Agones 模型 | **按需分配** GameServerAllocation | **常驻分片** LIST GameServer |
| Fleet | `pandora-battle` | `pandora-hub`（带 `pandora.dev/region` 标签）|
| 触发 | matchmaker 全员确认 → `AllocateBattle` | login → `AssignHub`（lazy-seed 分片到 Redis）|
| 容量判定 | 一对局一个 GameServer | hub_allocator 自己在 Redis 维护 `player_count`（500/实例）|
| 后端代码 | `data/agones_allocator.go`（W4 ⑫）| `biz/agones_fleet.go`（W4 ⑬）|

两者都**不引入 agones/client-go 重依赖**，用标准库 `net/http` 直连 k8s apiserver REST，
provider 无关（minikube / 自建 / ACK 一致），所以 **D7（k8s 选型）不卡此代码，只卡真集群联调**。

---

## 1. 环境准备命令（Codex 执行）

> 假设本机已装 Docker Desktop。命令按 Windows PowerShell 给出，必要处标注。

```powershell
# 1.1 装 minikube + kubectl + helm（如未装）
winget install Kubernetes.minikube
winget install Kubernetes.kubectl
winget install Helm.Helm

# 1.2 起 minikube（Docker driver，给足资源跑 Agones + 几个 GameServer）
minikube start --driver=docker --cpus=4 --memory=6144 --kubernetes-version=v1.30.0

# 1.3 装 Agones（官方 helm chart，装到 agones-system 命名空间）
helm repo add agones https://agones.dev/chart/stable
helm repo update
kubectl create namespace agones-system
helm install agones agones/agones --namespace agones-system --wait

# 1.4 校验 Agones controller 起来了
kubectl get pods -n agones-system           # agones-controller / agones-allocator 应 Running
kubectl get crd | Select-String agones      # 看到 fleets/gameservers/gameserverallocations
```

## 2. apply Pandora manifest（Codex 执行）

```powershell
# 顺序无强依赖，但建议先 RBAC 再 Fleet
kubectl apply -f deploy/k8s/agones/10-rbac-allocator.yaml
kubectl apply -f deploy/k8s/agones/20-fleet-battle.yaml
kubectl apply -f deploy/k8s/agones/30-fleet-hub.yaml

# 等 Fleet 全部 Ready（占位镜像 simple-game-server 自带 Agones SDK）
kubectl get fleet
kubectl get gameservers -L agones.dev/fleet,pandora.dev/region
# 期望:pandora-battle 2 个 Ready, pandora-hub 3 个 Ready(region=cn)
```

## 3. 让本机 allocator 连 minikube apiserver（Codex 执行 + 给 Claude 复核）

allocator 当前是**本机进程**（docker-compose dev 之外单独跑），需要把它指向 minikube
apiserver + 提供 `pandora-allocator` ServiceAccount 的 token。

```powershell
# 3.1 拿 minikube apiserver 地址
$apiServer = (kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
# 3.2 给 ServiceAccount 签一个短期 token（k8s >=1.24）
$token = (kubectl create token pandora-allocator -n default --duration=24h)
# 3.3 写到本机文件供 allocator 读(token_path 指向它)
Set-Content -Path "$env:TEMP\pandora-allocator.token" -Value $token -NoNewline
```

然后改两个 allocator 的 dev yaml 的 `agones` 段（**Claude 已留好字段，Codex 只填值**）：

```yaml
# ds_allocator-dev.yaml / hub_allocator-dev.yaml
agones:
  enabled: true
  api_server: "<上面的 $apiServer>"        # 形如 https://127.0.0.1:xxxxx
  namespace: "default"
  fleet_name: "pandora-battle"             # hub 填 "pandora-hub"
  token_path: "C:\\Users\\<you>\\AppData\\Local\\Temp\\pandora-allocator.token"
  insecure_skip_tls_verify: true           # minikube 自签证书,dev 临时开;或填 ca_path
  # ca_path: "<minikube ca.crt 路径>"      # 与 insecure_skip_tls_verify 二选一
```

> 也可用 `kubectl proxy --port=8001` 起本地代理，然后 `api_server: http://127.0.0.1:8001`
> + `token_path: "-"`（不带 token，proxy 复用 kubeconfig 凭证），免去 token/TLS 配置。

---

## 4. 分两步验证（重要：心跳 ≠ Agones SDK）

### 第一步：Agones 分配链路（占位镜像即可验，**现在就能做**）

simple-game-server 自带 Agones SDK，能让 Fleet 进 Ready，可完整验证「分配 → Allocated → 返回真实 addr」：

```powershell
# 4.1 手测 GameServerAllocation(不依赖 ds_allocator)
kubectl create -f deploy/k8s/agones/40-gameserverallocation-example.yaml -o yaml
#   看 status.state=Allocated + status.address + status.ports[0].port

# 4.2 起本机 ds_allocator(agones.enabled=true), grpcurl 调 AllocateBattle
#   期望返回真实 ds_addr(GameServer host:port), 不再是 mock 127.0.0.1:300xx
#   日志 allocator_mode=agones

# 4.3 起本机 hub_allocator(agones.enabled=true), grpcurl 调 AssignHub region=cn
#   期望 hub_ds_addr 为真实 GameServer host:port, 日志 fleet_mode=agones
```

### 第二步：DS 业务心跳上报（需真 UE DS 或 stub，留后续）

占位镜像**不会**向 ds_allocator / hub_allocator 发 Heartbeat（那是 Pandora 业务心跳，
经 gRPC unary 每 5s，与 Agones SDK health 无关，详见 `docs/design/ds-arch.md` §0.2）。

- 真 UE Pandora Hub DS / Battle DS（`D:\luyuan\Xuanming`，独立仓库）按
  `docs/design/agones-dev.md` 的「DS 心跳上报契约」实现后，心跳链路 + locator HUB/BATTLE
  上报闭环才能端到端跑通。
- 如需在 UE DS 就绪前先验后端心跳/sweep，可写一个 grpcurl 脚本周期调 `Heartbeat` 当 stub。

---

## 5. 风险 / 注意

- **占位镜像端口**：simple-game-server 默认绑 7654，本 Fleet 声明 containerPort 7777。
  GameServer 仍会进 Ready（Agones health 走 SDK，不探端口），分配返回的 host port 映射到 7777。
  真 UE DS 镜像须实际绑 7777（或同步改 Fleet）。
- **minikube 资源**：3+2 个 GameServer + Agones controller，建议 `--memory>=6144`，不足会 Pending。
- **token 时效**：`kubectl create token` 默认/指定时效到期后 allocator 调用会 401，需重签。
  in-cluster 部署用投影 token 自动轮转（allocator 代码每次请求重读 token 文件已支持）。
- **insecure_skip_tls_verify 仅 dev**：生产必须配 `ca_path`，禁用跳过校验。
- **GameServerAllocation 是一次性对象**：手测用 `kubectl create`（非 `apply`），每次触发一次分配。
- **关停**：`minikube stop` / `minikube delete`（删整个集群）由 Codex/用户执行。

---

## 6. 验收标准（Codex 跑完交 Claude 复核）

- [ ] `kubectl get fleet` 显示 pandora-battle(2)、pandora-hub(3) 全 Ready
- [ ] 手测 GameServerAllocation 返回 `state=Allocated` + 真实 address:port
- [ ] ds_allocator `agones.enabled=true` 下 `AllocateBattle` 返回真实 ds_addr，日志 `allocator_mode=agones`
- [ ] hub_allocator `agones.enabled=true` 下 `AssignHub region=cn` 返回真实 hub_ds_addr，日志 `fleet_mode=agones`
- [ ] 被分配的 GameServer 上能看到 `pandora.dev/match-id` 等业务标签
- [ ] （UE DS 就绪后）Heartbeat 链路 + locator HUB/BATTLE 上报闭环跑通
