# 本地 Agones dev 联调（minikube + Agones）

> W4 ⑬（2026-06-09）。把 ds_allocator / hub_allocator 从 mock 地址推进到本地 Agones 联调。
>
> ⚠️ **AGENTS.md §3 / §11.1**：本目录里所有「安装工具 / 起 minikube / helm 装 Agones / 拉镜像 /
> 启重服务」的命令**由 Codex / 用户执行**，Claude 只负责写清单 + 风险 + 验收标准。
> apply 业务 manifest（Fleet / RBAC）属本地 dev 集群操作，也由 Codex 执行。

---

## 🚀 真 DS 闭环·快速开始（无 mock，线上等价）

想跑「登录 → 大厅 Hub DS → 匹配进战斗 DS → 结算回大厅」的**真 DS 全链路**（用真 UE Linux DS 包，
不是 mock 假地址），一条命令：

```powershell
# 起 minikube + 装 Agones + apply RBAC/16-ds-envoy/Fleet + 部署 20 个后端服务(allocator=agones)
# 末尾 [8/8] 会自动跑 e2e_k8s.ps1(宿主 Envoy 桥接 + UDP 回程中继 + 验收清单),无需再手动跑
pwsh tools/scripts/start.ps1 -Mode k8s
```

只有宿主桥接/中继单独坏了(如手动杀了 port-forward)才需单跑修复：

```powershell
pwsh tools/scripts/e2e_k8s.ps1 -SkipImageLoad   # 只重建宿主桥接/中继,不动集群
```

`e2e_k8s.ps1` 自动完成：校验集群/Agones/Fleet 就绪 → 从 Fleet yaml 解析真 DS 镜像精确 tag 并
`minikube image load` → 起宿主 Envoy 桥接(`k8s_envoy_bridge.ps1`：对各业务 k8s Service 做
`kubectl port-forward` + 拉起 docker envoy `:8443`/`:8444`) → 轮询等 `pandora-battle` /
`pandora-hub` Ready → docker driver 下拉起容器版 UDP 回程中继 → 打印端到端验收清单与
实时观察命令。常用开关：`-NoRelay`（自己起中继）、`-SkipImageLoad`（镜像已 load）、
`-TimeoutSec`（等 Fleet 超时）。

> **DS 回调为什么能通**：k8s 模式下 DS(Pod)走**集群内 Envoy「DS 面」网关**
> `pandora-envoy.pandora.svc.cluster.local:8444`(由 `16-ds-envoy.yaml` 部署,线上同款拓扑),
> 它把 grpc-web 转成 gRPC 后直连 `ds-allocator.pandora.svc:50020` 等集群内 Service。
> Pod 内不再写 `host.docker.internal`(minikube 的 Pod 解析不了该域名,DNS 层就失败)。
> 宿主 Envoy `:8443` 服务 UE 玩家客户端，宿主 `:8444` 仅保留回环调试面；`k8s_envoy_bridge.ps1`
> 的 port-forward 是给宿主 Envoy upstream 用的，与 GameServer DS 回调无关。

> **为什么不是 `-Mode docker`**：docker-compose 里 ds_allocator 跑在 Linux 容器内,既不能 exec
> Windows DS、又没有 Agones 可调,代码只有 local/agones/mock 三种 provider,故 docker 只能落 mock。
> 要真 DS 用 `-Mode k8s`(本机 Agones,线上等价)或 `-Mode local`(本机直接 exec Windows DS)。
>
> **前置**:真 UE Linux DS 镜像须先由 UE 侧打包到 `deploy/ds/stage/LinuxServer`。本地可用
> `deploy/ds/build-image-minikube.ps1` 直接构建到 minikube 内置 Docker daemon（然后跑
> `e2e_k8s.ps1 -SkipImageLoad`），也可用 `deploy/ds/build-image.sh` 在宿主构建
> `pandora/battle-ds:dev` / `pandora/hub-ds:dev` 后让 `e2e_k8s.ps1` 执行 `minikube image load`。

详细环境准备 / 手测分配 / 心跳 stub 见下文各节。

---

## 🔁 电脑重启后 / 一键重置

**电脑重启后**(minikube 容器被停、宿主 go 进程/UDP 中继都没了,但集群状态和已 load 的镜像都还在磁盘上):

```powershell
pwsh tools/scripts/start.ps1 -Mode k8s -Resume   # minikube start + 等 Pod 恢复 + 自动重建宿主桥接/中继(末尾自动跑 e2e_k8s.ps1)
```

`-Resume` 是**快路径**:只 `minikube start`(集群/镜像都在磁盘,Pod 自动重建)再等关键 Pod 就绪,
**不重新 build/load 20 个镜像**,几十秒就回到上次状态。其它模式同理:

| 模式 | `-Resume` 做什么 |
|---|---|
| `k8s` | `minikube start` + 等 login/ds-allocator/hub-allocator 就绪 |
| `docker` / `intranet` | `docker compose up -d`(不加 `--build`,不重建镜像) |
| `local` | 基础设施容器随 Docker 自动恢复 + 重启宿主 go 服务 |
| `online` | 不适用(远端集群 Pod 自管) |

**环境乱了 / 想从头来**(`-Resume` 报错说找不到镜像或 Fleet,多半是之前 `minikube delete` 过):

```powershell
pwsh tools/scripts/start.ps1 -Mode k8s -Reset    # minikube delete 后全新部署(会重建+重 load 镜像,较慢;末尾同样自动跑 e2e_k8s.ps1)
```

`-Reset` = 彻底清掉旧状态再全新起(k8s 会 `minikube delete`;docker 会 `down -v` 清卷)。
线上 `online` 模式**禁用** `-Reset`(不对生产/测试集群做销毁式重置)。

> 经验法则:**正常重启用 `-Resume`(快),状态损坏才用 `-Reset`(慢但干净)。**

---

## ☁️ 线上真集群部署(online:测试服 / 生产 kbs)

线上 Fleet 跟本地有一处**必须换掉**、一处**默认已对齐**:
  1. DS 镜像:本地是 `pandora/battle-ds:dev` / `pandora/hub-ds:dev`(只在你机器上),远端要换成 registry 可拉取的完整镜像名
  2. DS 回调地址:Fleet 默认已是线上同款 `pandora-envoy.pandora.svc.cluster.local:8444`(本地由 `16-ds-envoy.yaml` 提供同名 Service,线上由边缘网关提供);若线上网关 DNS 不同,用 `-DsGatewayAddr` 覆盖

所以 `-Mode online` **强制要求**镜像/网关参数与环境对应的 kube-context 映射
(缺一即 fail-fast，不会把测试部署误打到生产):

```powershell
# 测试服集群(-Env test)
pwsh tools/scripts/start.ps1 -Mode online -Env test -TestKubeContext pandora-test `
  -Registry registry.mycorp.com -Tag v1.2.3 `
  -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 `
  -HubDsImage    registry.mycorp.com/pandora/hub-ds:v1.2.3 `
  -DsGatewayAddr pandora-envoy.pandora.svc:8444

# 生产 kbs 集群(-Env prod,会要求二次输入 kube-context + 大写 PROD 确认)
pwsh tools/scripts/start.ps1 -Mode online -Env prod -ProdKubeContext pandora-prod `
  -Registry registry.mycorp.com -Tag v1.2.3 `
  -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 `
  -HubDsImage    registry.mycorp.com/pandora/hub-ds:v1.2.3 `
  -DsGatewayAddr pandora-envoy.pandora.svc:8444
```

| 参数 | 作用 | 默认 |
|---|---|---|
| `-Registry` / `-Tag` | 20 个 Go 服务镜像来源(kustomize overlay 覆盖占位镜像) | 必填 |
| `-TestKubeContext` / `-ProdKubeContext` | 将 `-Env test/prod` 绑定到各自允许的 context；也可用 `PANDORA_K8S_TEST_CONTEXT/PROD_CONTEXT` | 对应环境必填 |
| `-BattleDsImage` / `-HubDsImage` | 远端 Fleet 的真 DS 镜像名(apply 前临时改写 Fleet yaml,**不改仓库文件**) | 必填 |
| `-DsGatewayAddr` | 覆写 Fleet 的 5 处 DS 回调 env(Battle 3 + Hub 2)→ 线上网关实际 DNS | 必填 |
| `-DsGatewayTls` | 改写 `PANDORA_DS_ALLOCATOR_TLS`；同集群 DS 面明文，只有集群外 TLS 边缘才设 `1` | `0` |
| `-BuildPush` | 本地构建并 push 20 个 Go 服务镜像到 `-Registry`(发布动作,需人工授权) | 关 |

> Fleet 改写是**在临时文件里做再 apply**:`20-fleet-battle.yaml` / `30-fleet-hub.yaml` 仓库原文
> 保持本地 dev 值,git 不会脏。线上 Agones 须由集群管理员预装(脚本只 apply 业务 RBAC/Fleet,不 helm install)。
>
> DS 镜像本身仍由 UE 侧 `deploy/ds/build-image.sh` 构建后,由人手动 `docker push` 到 registry
> (与 Go 服务镜像分开,脚本不替你 push DS 镜像)。
>
> 线上 DS 崩溃、`kubectl logs --previous`、Release 符号归档、Prometheus/Grafana 指标与
> profiler 排查见 [`docs/ops/linux-ds-observability.md`](../../../docs/ops/linux-ds-observability.md)。

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
# Windows / 国内网络下必须禁用 preload，否则容易卡在 Google preload tarball 下载；
# kicbase 使用已验证可拉取的阿里云镜像。
& 'C:\Program Files\Kubernetes\Minikube\minikube.exe' start `
  --driver=docker `
  --cpus=4 `
  --memory=6144 `
  --kubernetes-version=v1.30.0 `
  --base-image=registry.cn-hangzhou.aliyuncs.com/google_containers/kicbase:v0.0.50 `
  --preload=false `
  --cache-images=false `
  --interactive=false

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
# 先建 pandora 命名空间(RBAC 的 ServiceAccount 建在 pandora ns,没它 apply 会失败),
# 再 RBAC → DS 面 Envoy 网关(本地 minikube 必需,DS 回调 :8444 靠它) → Fleet
$ctx = 'pandora-agones' # 改成你的本地 minikube context；所有变更显式钉住，不依赖 current-context
kubectl --context $ctx apply -f deploy/k8s/services/00-namespace.yaml
kubectl --context $ctx apply -f deploy/k8s/agones/10-rbac-allocator.yaml
kubectl --context $ctx apply -f deploy/k8s/agones/16-ds-envoy.yaml
kubectl --context $ctx apply -f deploy/k8s/agones/20-fleet-battle.yaml
kubectl --context $ctx apply -f deploy/k8s/agones/30-fleet-hub.yaml

# 等真 Pandora DS Fleet 全部 Ready（镜像内已接 Agones SDK）
kubectl get fleet
kubectl get gameservers -L agones.dev/fleet,pandora.dev/region
# 期望:pandora-battle 2 个 Ready, pandora-hub 1 个 Ready(副本数以 20/30-fleet yaml 的 replicas 为准)
```

## 3. 让本机 allocator 连 minikube apiserver

> ⚠️ **提交规范**：两个 allocator 的 `*-dev.yaml` 基线一律保持 `mode: local`、`agones.enabled: false`,
> 且 `api_server` / `token_path` 用通用 in-cluster 默认值。**不要把本机 minikube 的临时 apiserver 地址、
> token 路径提交进仓库**。本地切到 Agones 链路靠 `start.ps1 -Mode k8s`(脚本生成 cluster 配置),
> 见 `tools/scripts/gen_cluster_config.ps1` 的 `-AllocatorMode agones`。

allocator 当前是**本机进程**（docker-compose dev 之外单独跑），需要把它指向 minikube
apiserver + 提供 `pandora-allocator` ServiceAccount 的 token。

```powershell
# 3.1 拿 minikube apiserver 地址
$apiServer = (kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
# 3.2 给 ServiceAccount 签一个短期 token（k8s >=1.24;SA 建在 pandora ns,见 10-rbac-allocator.yaml）
$token = (kubectl create token pandora-allocator -n pandora --duration=24h)
# 3.3 写到本机文件供 allocator 读(token_path 指向它)
Set-Content -Path "$env:TEMP\pandora-allocator.token" -Value $token -NoNewline
```

然后**临时**改两个 allocator 的 dev yaml 的 `agones` 段（**只在本机改，验完即还原，勿提交**）：

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
  advertise_host: "127.0.0.1"              # docker-driver 必填,见 §3.1;真集群留空用 status.address
```

> 也可用 `kubectl proxy --port=8001` 起本地代理，然后 `api_server: http://127.0.0.1:8001`
> + `token_path: "-"`（不带 token，proxy 复用 kubeconfig 凭证），免去 token/TLS 配置。

### 3.1 Windows 客户端 → Linux DS 回程中继（docker driver 必读）

minikube 用 **docker driver** 时，GameServer Pod 的 `status.address` 是集群内网 IP，Windows
客户端**直连不到**。解决办法两段：

1. allocator 侧把返回地址改写为客户端可达的宿主地址。`start.ps1 -Mode k8s` 的优先级是
   `-AdvertiseHost` → `PANDORA_DS_ADVERTISE_HOST` → 自动解析局域网 IPv4；都不可用才回退
   `127.0.0.1`（仅本机客户端）。
2. 本机起 UDP 中继，把宿主 `<advertise_host>:<port>` 转发到 minikube 节点同端口。**docker driver 下推荐用
   `e2e_k8s.ps1` 自动起的容器版中继**（`--network pandora-agones`，直连 minikube 节点 IP）；
   只在调试时才用进程版 `udp_relay.ps1`：

```powershell
# 容器版(e2e_k8s.ps1 自动做;手动等价命令,挂 pandora-agones 网络):
docker run -d --name pandora-udp-relay --network pandora-agones `
  -p 127.0.0.1:7000-8000:7000-8000/udp `
  -e TARGET_HOST=$(minikube -p pandora-agones ip) -e PORT_RANGE=7000-8000 `
  pandora/udp-relay:dev

# 进程版(仅调试;默认 profile=pandora-agones,自动解析 minikube -p pandora-agones ip):
pwsh tools/scripts/udp_relay.ps1
# 链路:client --UDP--> 127.0.0.1:<port> --[tools/udp-relay]--> <minikube ip>:<port> --> GameServer
```

> ⚠️ **必须用当前 profile（`pandora-agones` / `192.168.58.x`）的 minikube IP 和 docker network**。
> 旧的默认 `minikube` profile（`192.168.49.x`）network 重启后可能残留：若用裸 `minikube ip`、
> `--network minikube` 或 `docker network inspect minikube`，relay 会挂到错误 Docker 网络，
> **`pandora-udp-relay` 看似启动成功，但 UDP 包进不了 Hub DS——表现为客户端登录成功、却卡在进不去大厅**。
> `e2e_k8s.ps1` 启动前会校验 `TARGET_HOST` 是否落在该 docker network 的 IPv4 subnet 内，不匹配直接 fail。

> 真集群 / 非 docker-driver 不需要本中继，`advertise_host` 留空直接用 `status.address`。

---

## 4. 分两步验证（重要：心跳 ≠ Agones SDK）

### 第一步：Agones 分配链路（真 Pandora DS 镜像，**现在就能做**）

真 Pandora DS 镜像已接 Agones SDK，Fleet Ready 后可完整验证「分配 → Allocated → 返回真实 addr」：

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

### 第二步：DS 业务心跳上报（真 UE DS）

Fleet Ready 只证明真 DS 的 Agones SDK health 正常；还必须验证它向 ds_allocator / hub_allocator
发送 Pandora 业务 Heartbeat（gRPC unary 每 5s，与 Agones SDK health 是两条独立链路，详见
`docs/design/ds-arch.md` §0.2）。

- 真 UE Pandora Hub DS / Battle DS（Pandora-Client 独立仓库，DS 侧已接管）按
  `docs/design/agones-dev.md` 的「DS 心跳上报契约」实现，心跳链路 + locator HUB/BATTLE
  上报闭环端到端由真 UE DS 跑通。
- 后端侧需验证的闭环：
  - **心跳 / sweep / locator**：DS 周期调 Heartbeat + SetLocation；DS 心跳中断后观察
    hub/ds_allocator sweep 标 draining/abandoned；BATTLE→HUB 带 fence matchId 合法回流（W4 ⑪）。
  - **战斗结算 → 段位回滚补偿链（不变量 §4 第二段）**：DS 同步 ReportResult → 事务出箱 →
    `pandora.player.update` → player 段位回写；NORMAL 验 Elo 守恒、ABANDONED 验 mmr_delta 全 0、
    幂等复测 alreadyRecorded=true、outbox 清零。

---

## 5. 风险 / 注意

- **DS 端口**：Pandora Hub/Battle DS 与 Fleet 均使用 UDP `7777`；修改 DS 监听端口时必须同步清单。
- **minikube 资源**：默认 2 个 Battle + 1 个 Hub GameServer，再加 Agones controller；建议
  `--memory>=6144`，不足会 Pending。
- **token 时效**：`kubectl create token` 默认/指定时效到期后 allocator 调用会 401，需重签。
  in-cluster 部署用投影 token 自动轮转（allocator 代码每次请求重读 token 文件已支持）。
- **insecure_skip_tls_verify 仅 dev**：生产必须配 `ca_path`，禁用跳过校验。
- **GameServerAllocation 是一次性对象**：手测用 `kubectl create`（非 `apply`），每次触发一次分配。
- **关停**：`minikube stop` / `minikube delete`（删整个集群）由 Codex/用户执行。

---

## 6. 验收标准（Codex 跑完交 Claude 复核）

- [ ] `kubectl get fleet` 显示 pandora-battle(2)、pandora-hub(1) 全 Ready
- [ ] 手测 GameServerAllocation 返回 `state=Allocated` + 真实 address:port
- [ ] ds_allocator `agones.enabled=true` 下 `AllocateBattle` 返回真实 ds_addr，日志 `allocator_mode=agones`
- [ ] hub_allocator `agones.enabled=true` 下 `AssignHub region=cn` 返回真实 hub_ds_addr，日志 `fleet_mode=agones`
- [ ] 被分配的 GameServer 上能看到 `pandora.dev/match-id` 等业务标签
- [ ] （UE DS 就绪后）Heartbeat 链路 + locator HUB/BATTLE 上报闭环跑通
