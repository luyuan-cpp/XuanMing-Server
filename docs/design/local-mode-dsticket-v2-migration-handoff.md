# Handoff：mode=local 迁移到 DSTicket v2（RS256 + Model-B 权威链）

> 日期：2026-07-20。交接对象：Claude Code。
> 目标读者假设不了解本对话上下文，本文自包含。
> 涉及两个仓库：Go 后端 `XuanMing-Server`（本仓库）与 UE 客户端/DS `F:\work\Pandora-Client-SVN`。

## 0. 一句话目标

让 Windows 本机开发模式（`mode: local`，allocator 直接 exec `PandoraServer.exe`）的 DS 验票链路
从 legacy **local-off-v1（HS256 + 关闭权威门）** 迁移到与线上一致的
**DSTicket v2（RS256 本地公钥验签 + 实例绑定 + 授权租约/排空门 + placement 权威提交）**，
同时**保留本地快迭代体验**（不引入 Docker/k8s/minikube 依赖；仍然直接跑 Windows exe、可挂调试器）。

完成后的终局收益（后续独立任务，不在本次范围）：UE 侧可彻底删除 HS256 路径
（`ValidateTicket` / `FPandoraDSTicketSecretPolicy` / `HS256LocalOff` 档）与 Scheme-C 在线桥。

## 1. 现状（为什么本地现在不能走 RS256）

### 1.1 UE 侧三档验票（已就绪，v2 代码无需新写）

`Pandora-Client-SVN\Pandora\Source\Pandora\{Public,Private}\Gameplay\Default\PandoraDSGameModeBase.{h,cpp}`：

- **RS256Local（生产 v2，方案 B1）**：DS 只持公钥 JWKS（env `PANDORA_DSTICKET_JWKS_FILE` +
  `PANDORA_DSTICKET_KEYSET_REVISION`）。`ValidateTicketV2` = RS256 验签 + iss/aud/dst_ver/短时票
  + ds_type + **实例三元组绑定**（票据 ds_pod/ds_uid/ds_instance_epoch 必须等于本机 active 凭据，
  由 `UPandoraAgonesSubsystem::GetDSCredential` 提供）+ **授权租约/排空门**
  （`UPandoraDSBackendSubsystem::IsAcceptingNewPlayers`，靠权威心跳 ACK 维持租约）
  + match/roster/jti。Battle 还要 PreLoginAsync 经 `Login.VerifyDSTicket` 在线提交 placement，
  Hub 走 PostLogin Admission ACK。**roster 空 + dst_ver=2 会被拒**（`battle authoritative roster not ready`）。
- **HS256LocalOff（当前本地档）**：仅完整激活的 Windows local-off-v1；HS256 本地验签，
  secret 来自 env `PANDORA_DS_TICKET_SECRET` 或编译期公开占位值。
- **OnlineAuthority（Scheme-C 迁移桥）**：非 Agones、无 JWKS、无 local-off 时的兜底。

档位机械裁决（`FPandoraDSTicketVerifierProfilePolicy::Select`）：
**JWKS 任一 env 出现 / Agones / `PANDORA_DSTICKET_RS256_REQUIRED` 非空 → 锁死 RS256Local**，
绝不回落。所以后端只要给本地 DS 注入 JWKS env，UE 自动切 v2——但 v2 的
实例绑定/租约/placement 门也随之全部激活，后端必须配套，否则一个玩家都进不来。

### 1.2 后端侧本地锁

两个 allocator 启动时硬校验只接受 local-off-v1：

- `services/battle/hub_allocator/cmd/hub_allocator/main.go` ~L375：
  `auth.ValidateDSLocalHubProfileOffV1(...)`，失败即 `os.Exit(1)`，hint：
  `mode=local requires ds_auth.mode=off + authority_mode=legacy + signing key (local-off-v1); Redis Model-B local authority is not implemented`
- `services/battle/ds_allocator/cmd/ds_allocator/main.go` ~L264：
  `auth.ValidateDSLocalProfileOffV1(...)`，同上。

本地拉起 DS 时强制注入 `PANDORA_DS_LOCAL_PROFILE=local-off-v1`（`ExtraEnv` 伪造会被覆盖，
见 `services/battle/hub_allocator/internal/biz/local_fleet.go` 与
`internal/biz/local_fleet_test.go` ~L95、`services/battle/ds_allocator/internal/data/local_allocator_test.go` ~L289）。

签发侧：`pkg/config/config.go` ~L426 注释——
`留空 PrivateKeyFile = 本服务不启用 v2 签发，沿用 legacy HS256 DSTicket(dev/local-off 不变)`。
两个 dev yaml（`services/battle/hub_allocator/etc/hub_allocator-dev.yaml`、
`services/battle/ds_allocator/etc/ds_allocator-dev.yaml`）都未配 v2 签发。

### 1.3 已有可复用资产

- `tools/dsticketkeys`：生成 RSA-2048 `private.pem` + `jwks.json`（RFC 7638 kid、revision 元数据）；
  flags：`-out`（拒绝覆盖）、`-revision`、`-merge`、`-private-in`、`-active-kid`。
- `tools/scripts/start.ps1` 的 `Ensure-DsTicketDevKeyMaterial`（~L364）：k8s 模式下生成/复用
  dev 钥对（落 `run/cluster/dsticket/`），可作为本地模式钥料自举的参考实现。
- v2 签发器/验签器：`pkg/auth/dsticket.go`（`DSTicketSigner`/`DSTicketVerifier`，配置
  `private_key_file`/`active_kid`/`ttl`/`jwks_file`/`keyset_revision`，`pkg/config/config.go`
  `DSTicketConf`，`SignerEnabled()` = `PrivateKeyFile != ""`）。
- Model-B 权威链的 Redis 实现（Agones 模式在用）：
  - Hub 权威记录：`services/battle/hub_allocator/internal/data/hub_auth_repo.go`（Redis-only）
  - Battle 权威记录：`services/battle/ds_allocator/internal/data/battle_auth_repo.go`（Redis-only）
  - DS 回调令牌签发/验签：`pkg/middleware/dsauth.go`（HS256 服务令牌 + Redis gen 计数，无 K8s 依赖）
  - Hub/Battle credential 签发：`pkg/auth/jwt_dscallback.go`（无 K8s 依赖）
  - Hub Admission ACK：`services/battle/hub_allocator/internal/service/hub_service.go` `AssignHub`
  - Battle placement Commit：login 服务 `VerifyDSTicket`
  - **唯一真正 K8s/etcd 依赖**：`pkg/dsauthfence`（etcd writer capability fence），仅在
    `mode=agones && authority_mode=redis && ds_auth.mode=enforce` 的 modelB 组合下由
    ds_allocator `main.go` `AcquireRuntime` 使用。本地 Redis（docker compose）现成。

## 2. 目标架构（local-v2）

```
[start.ps1 -Mode local]
  ├─ 钥料自举: dsticketkeys → run/dev/dsticket/{private.pem, jwks.json}（Windows 本地路径）
  ├─ 签发方服务(所有 DSTicket 签发者)读 private_key_file + active_kid → 签 dst_ver=2 RS256 票
  │    票内含: 实例三元组(ds_pod/ds_uid/ds_instance_epoch)、battle: match_id/roster/allocation_id、
  │            hub: hub_assignment_id/release-track、短 TTL、jti
  ├─ allocator local fleet exec PandoraServer.exe 时注入:
  │    PANDORA_DSTICKET_JWKS_FILE=<绝对 Windows 路径>\run\dev\dsticket\jwks.json
  │    PANDORA_DSTICKET_KEYSET_REVISION=1
  │    PANDORA_DSTICKET_RS256_REQUIRED=1        （fail-closed 保险）
  │    （不再注入 PANDORA_DS_LOCAL_PROFILE=local-off-v1 —— 见 §4.3 陷阱）
  │    照旧注入: PANDORA_DS_TOKEN / AGONES_GAMESERVER_NAME / PANDORA_DS_TYPE / PANDORA_REGION /
  │              各回调地址(指向本机 allocator/login)
  ├─ DS(UE) 自动锁入 RS256Local 档，走与线上完全一致的验票代码路径
  └─ 本地 Model-B 权威链(Redis): credential 下发/心跳租约 ACK/Hub Admission ACK/
       Battle VerifyDSTicket placement Commit —— 全部走本机已有 Redis
```

不引入 etcd fence（那是 agones modelB 的脑裂围栏）；本地单机开发接受这一简化，
但必须显式标注（新 profile 名建议 `local-v2`，guard 中写明差异）。

## 3. 实施步骤（建议顺序）

### Phase 0 — 现状核实（先做，勿跳过）

子代理/检索核实以下 UNVERIFIED 项，结论直接影响后续工作量：

1. **签发方全集**：全仓搜 `DSTicketConf`、`SignerEnabled`、`SignDSTicket`（legacy）与
   `DSTicketSigner`（v2）的使用方。k8s 契约里提到"四个签发方"
   （`tools/scripts/start.ps1` Ensure-DsTicketDevKeyMaterial 注释）。确认 login（SelectRole 重签
   hub 票）、matchmaker、hub_allocator、ds_allocator 中谁真正签 DSTicket，本地 dev yaml 分别缺什么。
2. **本地模式现有 Model-B 覆盖度**：`ds_allocator/cmd/main.go` ~L215 注释说
   `local-off-v1 不接 Redis pending/ACK，但仍必须给 UE 完整 Model-B tuple`。核实本地模式下：
   DS 回调 credential 是否已下发（`PANDORA_DS_TOKEN` 已注入）？UE 的权威心跳
   （`UPandoraDSBackendSubsystem` 5s Heartbeat）在本地是否有服务端应答并带租约 ACK？
   若心跳链路本地已通，只是"租约 ACK/admission 语义"关闭，工作量显著小于从零实现。
3. **v2 claims 完整签发**：v2 票需要实例三元组 + allocation_id（battle）/hub_assignment_id（hub）。
   本地 allocator 生成 pod 名/UID/epoch 的位置（hub: `local_fleet.go`；battle:
   `local_allocator.go`，UID 本地随机、epoch 疑似恒 1——见探查报告
   `services/battle/ds_allocator/internal/biz/gameserver.go` ~L41 `ResolveExpectedPodUID`）。
   确认签发时能拿到与注入给 DS 的 credential 完全一致的三元组。
4. **UE 侧 local-off 激活条件**：`UPandoraAgonesSubsystem::IsLocalOffProfileActive` 的完整判定
   （Windows、非 Agones、本地 pod 前缀、scope 等），确认去掉 env 注入后它稳定为 false。

### Phase 1 — 钥料自举（Windows 本地路径）

- 在本地启动流程（`start.cmd` → `tools/scripts/start.ps1 -Mode local` 分支，或
  `run_services.ps1`，以实际入口为准）加入本地版钥料自举：若
  `run/dev/dsticket/{private.pem,jwks.json}` 不存在则 `go run ./tools/dsticketkeys -out run/dev/dsticket -revision 1`。
  参考 `Ensure-DsTicketDevKeyMaterial`，但**不走 k8s Secret/ConfigMap**，直接落文件。
  半套残留拒绝覆盖的语义保留（人工清理）。
- `run/` 已不入版本库（核实 .gitignore）。
- 注意 `dsticketkeys` stdout 隔离问题（见 `tools/scripts/tests/dsticket_keyset_contract_test.ps1`
  ~L187 的契约：`| Out-Host`，函数只返回 kid 字符串）。

### Phase 2 — 签发侧切 v2

- 给 Phase 0 确认的每个签发方 dev yaml 增加：

  ```yaml
  ds_ticket:
    private_key_file: "run/dev/dsticket/private.pem"   # 相对/绝对以 conf 加载语义为准
    active_kid: "<自举后回填，或支持从 jwks.json 对账推导>"
    ttl: "120s"        # 与线上一致的短时票
  ```

  active_kid 不应要求人手填：优先做成启动脚本从 `private.pem` 推导（参考
  `Get-PandoraDSTicketKeyMaterialContract` 在 start.ps1 的对账逻辑）后以环境变量/生成配置注入。
- 签出的票必须是 `dst_ver=2` 且带实例三元组/allocation_id 等 v2 claims，与 agones 路径同一代码。
  若本地路径此前走 legacy `SignDSTicket`（HS256），改调 v2 signer；**不要**保留"v2 配了就 v2、
  没配回落 HS256"的静默逻辑——local-v2 profile 下缺私钥直接启动失败（fail-closed）。

### Phase 3 — 本地 Model-B 权威链（核心工作量）

- 新增显式 profile（建议 `ds_auth` 下 `local_profile: "local-v2"` 或等价开关），并新增
  guard `ValidateDSLocalProfileV2` / `ValidateDSLocalHubProfileV2`（`pkg/auth/ds_local_profile.go`
  旁边）：要求 v2 signer 已配置 + Redis 可用；**不要求 etcd**。旧
  `ValidateDSLocal*ProfileOffV1` 保留（迁移期两档并存，由配置显式选择，不自动判定）。
- 打开本地模式的 Redis 权威链（对齐 agones 语义，去掉 etcd fence 部分）：
  - DS 权威心跳 → 租约 ACK（维持 UE `IsAcceptingNewPlayers()`==true；断供 20s UE 会自我 fencing）。
  - Hub：PostLogin Admission ACK（`AssignHub` 链路）+ hub_assignment_id 签进 hub 票。
  - Battle：login `VerifyDSTicket` placement Commit（UE Battle PreLoginAsync 必调，本地 login 需可达
    且开启该 RPC 的 v2 语义）；roster 必须随分配下发（v2 空 roster 直接拒）。
  - 三元组一致性：allocator 写进票的 pod/uid/epoch == 注入 DS credential 的 pod/uid/epoch
    ==（UE 心跳上报的）本机 active 凭据。
- main.go 的 modelB/etcd 分支：为 local-v2 走"Redis 权威、无 etcd fence"的组合，明确注释
  这是本地开发简化（无跨实例脑裂场景）。

### Phase 4 — DS 环境注入切换

- `local_fleet.go` `buildEnv()` 与 `local_allocator.go` 对应位置：
  - 增：`PANDORA_DSTICKET_JWKS_FILE`（**绝对 Windows 路径**，UE 进程 working_dir 是客户端仓库，
    相对路径不可靠）、`PANDORA_DSTICKET_KEYSET_REVISION=1`、`PANDORA_DSTICKET_RS256_REQUIRED=1`。
  - 删（local-v2 时）：`PANDORA_DS_LOCAL_PROFILE=local-off-v1`、`PANDORA_DS_TICKET_SECRET`。
  - 保留：`PANDORA_DS_TOKEN`、`AGONES_GAMESERVER_NAME`、`PANDORA_DS_TYPE`、`PANDORA_REGION`、
    回调地址（本机 allocator/login/locator）。
  - "ExtraEnv 不得伪造 profile"的防护测试同步更新（`local_fleet_test.go` ~L95、
    `local_allocator_test.go` ~L289）。
- Hub 端口 7777 / Battle 7800+、exe 路径
  `F:\work\Pandora-Client-SVN\Pandora\Binaries\Win64\PandoraServer.exe` 等启动机制全部不变。

### Phase 5 — 脚本与测试

- `start.cmd` / `内网服务器一键启动*.cmd` / `策划一键*.cmd` 涉及的本地启动路径确认钥料自举先于服务启动。
- Go 测试：新 guard 单测；local fleet env 注入断言（有 JWKS、无 local-off env、无 HS secret）；
  签发端 v2 claims 断言。
- 端到端验收（人工/机器人）：本地一键启动 → 客户端 login → 选角 → 进 Hub DS →
  匹配 → Battle DS → 结算回 Hub。DS 日志必须出现 `v2 纯本地 PreLogin 通过`（LogPandoraDSAuth），
  **不得出现** `Windows local-off-v1 已机械激活` 或任何 HS256/Scheme-C 路径日志。
  另验证负例：改坏 jwks.json → DS 拒绝所有连接（fail-closed，不回落）。

### Phase 6（独立后续，另开任务）— UE 删 HS256

local-v2 跑通并稳定后，在客户端仓库 `F:\work\Pandora-Client-SVN` 删除：
`ValidateTicket`、`FPandoraDSTicketSecretPolicy`（类 + `PandoraAgonesSnapshotTest.cpp` 相关用例）、
`EPandoraDSTicketVerifierProfile::HS256LocalOff` 及其分支、`FPandoraTicketVerifier::Verify` HS256 路径、
`ExpectedIssuer/ExpectedAudience` 在 HS 路径的用法；Scheme-C（OnlineAuthority 桥）按原计划一并退役。
后端删 legacy `SignDSTicket`（HS256）与 `PANDORA_DS_TICKET_SECRET` 全链路。

## 4. 关键陷阱（必读）

1. **半配置即锁死**：UE 侧 JWKS env 只出现一个（或文件坏/revision 不符）→ 锁 RS256 且验票必拒，
   不回落。所以 env 注入与钥料自举必须原子成套。
2. **租约断供 = 全员被踢**：UE `OnAuthorityLeaseLost`（连续 20s 无凭据绑定的权威心跳 ACK）会
   fencing 全部存量玩家。本地实现心跳 ACK 时注意 5s 周期与 20s 窗口，调试断点挂太久也会触发——
   这是预期行为，但要在文档/日志里讲清楚，避免被当成 bug 反复排查。
3. **不要残留 local-off env**：若 `PANDORA_DS_LOCAL_PROFILE=local-off-v1` 与 JWKS env 同时存在，
   UE profile 仍是 RS256Local（JWKS 优先），但 `bLocalOffProfileActive=true` 会让
   `ShouldUseOnlineAdmissionRPC` 返回 false → Battle 跳过 placement Commit → InitNewPlayer 因
   拿不到在线成功凭证而 **fail-closed 拒绝所有 Battle 玩家**。切 v2 时必须删掉该 env。
4. **v2 leeway 硬钳 ≤15s**：`FPandoraDSTicketV2VerifyParams::MaxLeewaySeconds`。本机与后端时钟
   同机不成问题，但票 TTL 要给足网络/断点余量（线上 120s 短时票语义保持）。
5. **v2 空 roster 拒绝**：battle 分配必须把 player_ids/roster 传到 DS（`SetExpectedPlayers`），
   local-off 时代"空名单放行"在 dst_ver=2 下不存在。
6. **jti 单次消费**：客户端重连需重新取票（现有客户端逻辑已如此，勿在本地"复用旧票"调试）。
7. **Windows 路径**：所有传给 UE 进程的文件路径用绝对路径；yaml 里的相对路径以各服务
   conf 加载的 working dir 语义核实。
8. **密钥文件纪律**：`dsticketkeys` 拒绝覆盖非空输出目录是刻意的；脚本遇半套残件必须报错引导
   人工清理，绝不静默换钥（在跑集群/本地票据会全废）。
9. **契约测试**：`tools/scripts/tests/` 下有多个 dsticket 契约测试
   （`dsticket_keyset_contract_test.ps1`、`dsticket_rotation_contract_test.ps1`、
   `gen_cluster_b1_contract_test.ps1`、`online_manifest_contract_test.ps1`）。改 start.ps1/生成器
   后必须全部跑绿；其中有"fleet 注解禁止出现字面 PANDORA_DS_TICKET_SECRET"之类的机械纪律。

## 5. 验收清单

- [ ] `start.cmd`（local 模式）零人工干预完成钥料自举 + 全服务启动。
- [ ] Hub/Battle DS 日志：走 `v2 纯本地 PreLogin`；无 local-off / HS256 / Scheme-C 日志。
- [ ] 全闭环：login → 选角 → Hub DS → 匹配 → Battle DS → 结算 → 回 Hub，全程可玩。
- [ ] 顶号/重连（同 player 二连）行为与线上一致（单会话踢旧）。
- [ ] 负例：坏 JWKS / 缺 revision / 票过期 / roster 外玩家 / jti 重放 → 全部拒绝且日志可定位。
- [ ] 租约断供 20s → DS fencing 存量玩家（可用暂停 allocator 进程模拟）。
- [ ] Go 单测 + `tools/scripts/tests/*.ps1` 契约测试全绿；`go build ./...` 通过。
- [ ] 快迭代体验不回退：无 Docker/k8s 依赖，改 UE 代码→编译→一键启动→进服 仍是分钟级。

## 6. 探查报告勘误（供 Claude Code 校准预期）

先前一份自动探查报告结论过于乐观（"只加 3 行 yaml + 2 行 env 即可"）。它遗漏了：
UE 一旦锁入 RS256Local，实例绑定门、授权租约门、Battle placement Commit、Hub Admission ACK、
非空 roster 全部生效——这些正是 guard 报错 `Redis Model-B local authority is not implemented`
所指的缺口，是本任务的主要工作量（Phase 3）。etcd fence 是唯一建议本地豁免的组件。
