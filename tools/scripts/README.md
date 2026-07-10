# tools/scripts 导航

后端仓库的 PowerShell 运维/开发脚本索引。**不要 ad-hoc 新起脚本**;新增前先看这里有没有可复用的。

> 约定:所有脚本从**仓库根目录**运行,例:`pwsh tools/scripts/start.ps1 -Mode local`。

## 1. 启动 / 编排(日常主链)

| 脚本 | 用途 | 被谁调用 |
|---|---|---|
| `start.ps1` | 项目总入口,5 模式编排(local/docker/intranet/battle/k8s/online) | 根目录 `start.cmd` |
| `play.ps1` | 策划友好入口(战斗模式,可开编辑器/客户端) | `策划一键启动/停止.cmd`、`内网一键启动/停止.cmd` |
| `dev_all.ps1` | 一键起基础设施 + 全业务 go 服务 | `start.ps1`(local) |
| `dev_up.ps1` | 起 docker 基础设施(MySQL/Redis/Kafka/etcd/Prometheus) | `start.ps1`、`dev_all.ps1`、`play.ps1` |
| `dev_down.ps1` | 停基础设施容器 | `start.ps1`、`dev_all.ps1`、`play.ps1` |
| `dev_status.ps1` | 查看开发环境状态(容器 + 端口监听) | 手动 |
| `run_services.ps1` | 宿主 go 服务编排(启/停/看日志) | `start.ps1`、`play.ps1`、`dev_all.ps1`、`dev_tools.ps1` |
| `gen_cluster_config.ps1` | 生成集群版配置(容器地址 / allocator 模式改写) | `start.ps1`(docker/battle 等) |
| `tidb_up.ps1` | TiDB 集群一键起(社交库可选) | 手动(见 `deploy/tidb-init/README.md`) |

## 2. k8s / 真 DS 链路

| 脚本 | 用途 | 被谁调用 |
|---|---|---|
| `e2e_k8s.ps1` | 本地 minikube+Agones 真 DS 闭环(load 镜像 + 桥接 Envoy + 等 Fleet + UDP 中继) | 手动(`k8s` 模式起完后) |
| `k8s_envoy_bridge.ps1` | 宿主 Envoy 端口转发桥接 | `e2e_k8s.ps1` |
| `udp_relay.ps1` | UDP 回程中继(minikube docker driver 下 DS 连通) | `e2e_k8s.ps1` |
| `reset_data_service_schema.ps1` | 开发期定向重置 data_service 的 `player_data` 表与玩家缓存；固定本地 minikube context，默认停服不重启 | 手动；需 `-Confirm`/`-Force` |
| `reset_data_service_schema_k8s.bat` | 上述重置脚本的 Windows k8s 包装器；第二参数 `restart` 可在新镜像就位后重启并验表 | 手动/双击 |

> DS(Hub/Battle)本身由 Pandora-Client / UE 侧仓库产出,后端不再维护 DS 编译或 stub 脚本。

## 3. 证书 / TLS

| 脚本 | 用途 | 被谁调用 |
|---|---|---|
| `envoy_cert.ps1` | Envoy TLS 证书校验/自愈(共享库) | `dev_up.ps1`、`install_shared_dev_ca.ps1`、`k8s_envoy_bridge.ps1` |
| `install_shared_dev_ca.ps1` | 安装全队共享开发 CA | 手动(见 `deploy/dev-ca/README.md`) |
| `import_dev_ca.ps1` | 客户端信任开发 CA 证书 | 手动 |

## 4. 镜像 / 工具链 / proto

| 脚本 | 用途 | 被谁调用 |
|---|---|---|
| `export_images.ps1` | 导出业务镜像 tar(离线分发) | `出离线镜像包.cmd` |
| `import_images.ps1` | 离线导入镜像 | 手动(见 `docs/ops/planner-quickstart.md`) |
| `install_dev_tools.ps1` | 安装开发工具链(go/docker/kubectl/minikube/mkcert 等) | 手动(见 README) |
| `proto_gen.ps1` | 生成 go pb(proto 改动后由 Codex 跑) | 手动(见 CLAUDE.md §5) |

## 5. 压测 / 发布 / 诊断

| 脚本 | 用途 | 被谁调用 |
|---|---|---|
| `dev_tools.ps1` | 开发工具集(清 MySQL/Kafka/etcd、重置 offset) | 手动(见 `docs/design/stress-discipline.md`) |
| `stress_snap.ps1` | Prometheus 快照批量抓取(压测采集) | 手动 |
| `stress_summarize.ps1` | 压测单轮汇总(5 段二维表) | 手动 |
| `release_preflight.ps1` | 发布前预检(配置安全 / 密码强度) | 手动(见 `docs/ops/release-checklist.md`) |
| `http2_probe.ps1` | 探测 Envoy 客户端连接是否走 HTTP/2 | 手动(见 `docs/design/gateway-decision.md`) |
