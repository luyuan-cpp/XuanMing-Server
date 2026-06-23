# 本地 Agones dev 联调（minikube + Agones）

> W4 ⑬（2026-06-09）。把 ds_allocator / hub_allocator 从 mock 地址推进到本地 Agones 联调。
>
> ⚠️ **AGENTS.md §3 / §11.1**：本目录里所有「安装工具 / 起 minikube / helm 装 Agones / 拉镜像 /
> 启重服务」的命令**由 Codex / 用户执行**，Claude 只负责写清单 + 风险 + 验收标准。
> apply 业务 manifest（Fleet / RBAC）属本地 dev 集群操作，也由 Codex 执行。

---

## 🚀 真 DS 闭环·快速开始（无 mock，线上等价）

想跑「登录 → 大厅 Hub DS → 匹配进战斗 DS → 结算回大厅」的**真 DS 全链路**（用真 UE Linux DS 包，
不是 mock 假地址），两条命令：

```powershell
# 1) 起 minikube + 装 Agones + apply RBAC/Fleet + 部署 16 个后端服务(allocator=agones)
pwsh tools/scripts/start.ps1 -Mode k8s

# 2) load 真 DS 镜像 + 等 Fleet Ready + 后台起 UDP 回程中继 + 打印验收清单
pwsh tools/scripts/e2e_k8s.ps1
```

`e2e_k8s.ps1` 自动完成：校验集群/Agones/Fleet 就绪 → 从 Fleet yaml 解析真 DS 镜像精确 tag 并
`minikube image load` → 起宿主 Envoy 桥接(`k8s_envoy_bridge.ps1`：对 16 个 k8s Service 做
`kubectl port-forward` + 拉起 docker envoy `:8443`/`:8444`) → 轮询等 `pandora-battle` /
`pandora-hub` Ready → docker driver 下拉起容器版 UDP 回程中继 → 打印端到端验收清单与
实时观察命令。常用开关：`-NoRelay`（自己起中继）、`-SkipImageLoad`（镜像已 load）、
`-TimeoutSec`（等 Fleet 超时）。

> **DS 回调为什么能通**：k8s 模式下 16 个 Go 服务都在 `pandora` ns 的 ClusterIP 后面,而 UE
> 客户端/DS 只会打宿主 Envoy(`:8443`/`:8444`)。`k8s_envoy_bridge.ps1` 把每个 Service
> `port-forward` 到 `127.0.0.1:500xx`,正好对上现有 `envoy.yaml` 的 `host.docker.internal:500xx`
> upstream,所以 DS 的 `host.docker.internal:8444` 回调能真正落到 k8s 服务,闭环不再断。

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
pwsh tools/scripts/start.ps1 -Mode k8s -Resume   # minikube start + 等 Pod 自动恢复(不重建镜像)
pwsh tools/scripts/e2e_k8s.ps1                    # 重新起 UDP 中继 + 校验 Fleet + 打印验收清单
```

`-Resume` 是**快路径**:只 `minikube start`(集群/镜像都在磁盘,Pod 自动重建)再等关键 Pod 就绪,
**不重新 build/load 16 个镜像**,几十秒就回到上次状态。其它模式同理:

| 模式 | `-Resume` 做什么 |
|---|---|
| `k8s` | `minikube start` + 等 login/ds-allocator/hub-allocator 就绪 |
| `docker` / `intranet` | `docker compose up -d`(不加 `--build`,不重建镜像) |
| `local` | 基础设施容器随 Docker 自动恢复 + 重启宿主 go 服务 |
| `online` | 不适用(远端集群 Pod 自管) |

**环境乱了 / 想从头来**(`-Resume` 报错说找不到镜像或 Fleet,多半是之前 `minikube delete` 过):

```powershell
pwsh tools/scripts/start.ps1 -Mode k8s -Reset    # minikube delete 后全新部署(会重建+重 load 镜像,较慢)
pwsh tools/scripts/e2e_k8s.ps1
```

`-Reset` = 彻底清掉旧状态再全新起(k8s 会 `minikube delete`;docker 会 `down -v` 清卷)。
线上 `online` 模式**禁用** `-Reset`(不对生产/测试集群做销毁式重置)。

> 经验法则:**正常重启用 `-Resume`(快),状态损坏才用 `-Reset`(慢但干净)。**

---

## ☁️ 线上真集群部署(online:测试服 / 生产 kbs)

线上 Fleet 跟本地有两处**必须换掉**,否则远端拉不到镜像、DS 回调打到不存在的宿主地址:
  1. DS 镜像:本地是 `pandora/battle-ds:dev` / `pandora/hub-ds:dev`(只在你机器上),远端要换成 registry 可拉取的完整镜像名
  2. DS 回调地址:本地是 `host.docker.internal:8444`,远端要换成集群内 Envoy/网关的 DS 面 Service DNS

所以 `-Mode online` **强制要求**这几个参数(缺一即 fail-fast,不会把本地 Fleet 误打到远端):

```powershell
# 测试服集群(-Env test)
pwsh tools/scripts/start.ps1 -Mode online -Env test `
  -Registry registry.mycorp.com -Tag v1.2.3 `
  -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 `
  -HubDsImage    registry.mycorp.com/pandora/hub-ds:v1.2.3 `
  -DsGatewayAddr pandora-envoy.pandora.svc:8444

# 生产 kbs 集群(-Env prod,会要求二次输入 kube-context + 大写 PROD 确认)
pwsh tools/scripts/start.ps1 -Mode online -Env prod `
  -Registry registry.mycorp.com -Tag v1.2.3 `
  -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 `
  -HubDsImage    registry.mycorp.com/pandora/hub-ds:v1.2.3 `
  -DsGatewayAddr pandora-envoy.pandora.svc:8444
```

| 参数 | 作用 | 默认 |
|---|---|---|
| `-Registry` / `-Tag` | 16 个 Go 服务镜像来源(kustomize overlay 覆盖占位镜像) | 必填 |
| `-BattleDsImage` / `-HubDsImage` | 远端 Fleet 的真 DS 镜像名(apply 前临时改写 Fleet yaml,**不改仓库文件**) | 必填 |
| `-DsGatewayAddr` | 改写 Fleet 里 3 个 `host.docker.internal:8444` 回调 env → 集群内 Envoy DS 面 DNS | 必填 |
| `-DsGatewayTls` | 改写 `PANDORA_DS_ALLOCATOR_TLS`(线上 Envoy 终止 TLS 一般 `1`) | `1` |
| `-BuildPush` | 本地构建并 push 16 个 Go 服务镜像到 `-Registry`(发布动作,需人工授权) | 关 |

> Fleet 改写是**在临时文件里做再 apply**:`20-fleet-battle.yaml` / `30-fleet-hub.yaml` 仓库原文
> 保持本地 dev 值,git 不会脏。线上 Agones 须由集群管理员预装(脚本只 apply 业务 RBAC/Fleet,不 helm install)。
>
> DS 镜像本身仍由 UE 侧 `deploy/ds/build-image.sh` 构建后,由人手动 `docker push` 到 registry
> (与 Go 服务镜像分开,脚本不替你 push DS 镜像)。

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
# 顺序无强依赖，但建议先 RBAC 再 Fleet
kubectl apply -f deploy/k8s/agones/10-rbac-allocator.yaml
kubectl apply -f deploy/k8s/agones/20-fleet-battle.yaml
kubectl apply -f deploy/k8s/agones/30-fleet-hub.yaml

# 等 Fleet 全部 Ready（占位镜像 simple-game-server 自带 Agones SDK）
kubectl get fleet
kubectl get gameservers -L agones.dev/fleet,pandora.dev/region
# 期望:pandora-battle 2 个 Ready, pandora-hub 3 个 Ready(region=cn)
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
# 3.2 给 ServiceAccount 签一个短期 token（k8s >=1.24）
$token = (kubectl create token pandora-allocator -n default --duration=24h)
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

1. allocator 侧把返回地址改写成本机回环：`advertise_host: "127.0.0.1"`（§3 yaml 已含）。
   `start.ps1 -Mode k8s` 走脚本流程时会自动注入(`gen_cluster_config.ps1 -AllocatorAdvertiseHost 127.0.0.1`)。
2. 本机起 UDP 中继，把 `127.0.0.1:<port>` 转发到 minikube 节点同端口。**docker driver 下推荐用
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

- 真 UE Pandora Hub DS / Battle DS（独立仓库）按
  `docs/design/agones-dev.md` 的「DS 心跳上报契约」实现后，心跳链路 + locator HUB/BATTLE
  上报闭环才能端到端跑通。
- **UE DS 就绪前用 stub 脚本先验后端心跳 / sweep / locator 闭环**
  （`tools/scripts/ds_heartbeat_stub.ps1`，grpcurl 周期调 Heartbeat + SetLocation）：

```powershell
# 起 hub_allocator + player_locator(本机进程, 连 dev redis), 然后:
# 种子分片 → 持续心跳 → Ctrl+C 停 → 看 hub_allocator sweep 标 draining
pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role hub -AssignFirst -PlayerId 30907585389428737
pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role hub -PodName pandora-hub-global-1 -PlayerCount 42

# 战斗 DS: 需先经 matchmaker/AllocateBattle 建镜像(mock 名 pandora-battle-<matchId>)
pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role battle -PodName pandora-battle-123456 -MatchId 123456

# locator BATTLE→HUB 合法回流(带 fence matchId, W4 ⑪)
pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role hub -PodName pandora-hub-global-1 `
    -LocatorPlayerId 30907585389428737 -ShardId 1 -FenceMatchId 123456 -Count 1
```

- **战斗结算 → 段位回滚补偿链(不变量 §4 第二段)用 `battle_result_outbox_probe.ps1`**
  (grpcurl 同步 ReportResult → 事务出箱 → `pandora.player.update` → player 段位回写)：

```powershell
# 启动 player(:50002) + battle_result(:50022)(强依赖 MySQL pandora_battle/pandora_player + kafka),然后:
# NORMAL: A 队胜, 5v5, 幂等复测(第二次 alreadyRecorded=true), 验 Elo +16/-16 守恒
pwsh tools/scripts/battle_result_outbox_probe.ps1 -MatchId 987655001 `
    -BasePlayerId 30907586000000000 -WinnerTeam 0 -Idempotent

# ABANDONED: DS 崩溃补偿, 强制 mmr_delta 全 0(玩家不掉段)
pwsh tools/scripts/battle_result_outbox_probe.ps1 -MatchId 987655002 `
    -BasePlayerId 30907586100000000 -Outcome ABANDONED
```

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
