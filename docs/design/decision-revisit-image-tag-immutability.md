# decision-revisit:镜像 tag 不可变 + digest 固定 + imageID 验证(发布审核 P1 #5)

> 状态:**在线链路已实现、仍禁止生产执行**(2026-07-12 人要求修复审核阻断后由 Codex 落地;
> registry / 集群真值尚未验证,且 `start.ps1` 仍被 DSTicket 验票方案 B/C、生产 etcd mTLS/ACL、
> registry native immutable-tag 三道硬门拦住,不会 push/apply。
> 离线 tar 与 registry digest 等价性仍待真实 CRI 验证,不能宣称四件套全部闭环)。
> 关联:`tools/scripts/start.ps1`(BuildPush 第 1423 行、Fleet apply)、
> `tools/scripts/export_images.ps1`(离线镜像包)、`deploy/k8s/overlays/online/`(kustomize images)、
> `deploy/k8s/agones/20-fleet-battle.yaml` / `30-fleet-hub.yaml`、`deploy/offline-images/README.md`、
> CLAUDE.md §9 不变量 16(不停服滚动更新)、AGENTS.md §7 / §11.1 / §14。

## 1. 旧问题(reviewer 发布 P1 #5)

`start.ps1` 的 `BuildPush`(第 1423 行)把本地 `pandora/<svc>:dev` 打成
`$Registry/pandora/<svc>:$Tag` 后 `docker push`,`$Tag` 是**调用方传入的可变 tag**,允许覆盖推送
同名 tag。而 20 个业务 Deployment 与 2 个 Agones Fleet(battle / hub DS)全部使用
`imagePullPolicy: IfNotPresent`。

后果链:

- 同一 `$Tag` 被重复推送不同内容 → 各节点上是否是**新镜像**取决于本地是否已缓存该 tag;
- `IfNotPresent` 下,已缓存该 tag 的节点**不会重新拉取**,直接跑**缓存里的旧镜像**;
- 没有 digest pinning,也没有 Pod / GameServer `imageID` 回读验证 → 发布"成功"但线上实际
  跑的是旧二进制,且**无任何告警**。

这既是发布正确性问题,也间接违反不变量 16 的精神:滚动更新时不同节点可能跑**不同版本**的镜像
(有的拉了新、有的用旧缓存),混合版本不受控。

## 2. 为什么不能简单改成 `imagePullPolicy: Always`

reviewer 建议的"改 Always"会**砸掉本项目的离线镜像包(air-gap)部署模型**:

- 本仓库有 `deploy/offline-images/pandora-images.tar` 气隙分发路线(`export_images.ps1` →
  `docker save` → 内网 `docker load`),内网集群**没有可拉取的 registry**;
- `Always` 会让每个 Pod 启动都尝试从 registry 拉取 → 气隙环境直接 `ImagePullBackOff`,
  一键启动/离线部署全线崩。

所以正解不是改 pull policy,而是**让 tag/镜像标识本身不可变**,使 `IfNotPresent` 的"缓存命中"
等价于"内容相同",再加**发布后 imageID 验证门**兜底。

## 3. 新方案(待拍板,四件套)

### 3.1 不可变 tag 约定(禁止复用)

- 发布 tag 必须**全局唯一且不复用**:推荐 `<git-short-sha>`(或 `<semver>-<git-sha>` /
  `<yyyymmddHHMM>-<git-sha>`),禁止再用 `dev` / `latest` / 固定版本号覆盖推送;
- `start.ps1` online 增加**预推送存在性检查**:推送前用 registry manifest HEAD /
  `docker manifest inspect $Registry/pandora/<svc>:$Tag` 查目标 tag 是否已存在:
  - 已存在且允许复用 → 直接 **throw**(不可变约定禁止覆盖);
  - 需 registry 读权限,属 Codex/registry ops(§11.1),脚本只发起校验、真读由 Codex 执行。

### 3.2 digest 固定(pin 到 `repo@sha256:...`)

- `docker push` 成功后解析该镜像的 registry digest(`docker push` 输出的
  `digest: sha256:...`,或 `docker buildx imagetools inspect` / `docker manifest inspect`);
- 把 **overlay(kustomize `images[].digest`)与 2 个 Fleet 的 `image`** 改写成
  `$Registry/pandora/<svc>@sha256:<digest>`(而非 `:$Tag`);
- 这样即便 `IfNotPresent`,节点缓存命中的判定基于 **digest**,digest 唯一 ⇒ 缓存命中 ⇔ 内容一致,
  彻底消除"同 tag 不同内容跑旧缓存"。

### 3.3 发布后 imageID 验证门(fail-closed)

- 20 个 Deployment `rollout status` 成功 + 2 个 Fleet GameServer Ready 后,回读:
  - 各 Pod `.status.containerStatuses[].imageID`;
  - 各 DS GameServer 底层 Pod 的 `imageID`;
- 断言全部 `imageID` 的 digest 部分 == 本次推送/pin 的 digest;任一不符 → **throw**
  (拒绝把跑着旧镜像的发布判成功);
- 需 kubectl 读集群状态,属 Codex ops;脚本负责组织断言逻辑与失败阻断。

### 3.4 离线包一致性(仍待验证)

- `docker save/load` 保存的是镜像配置与 manifest,但不能仅凭脚本推断目标 CRI 登记的 `imageID`
  必然等于在线 registry 顶层 manifest digest;
- 必须在真实气隙节点验证 `docker/ctr` 导入后,digest-pinned Pod 能在 `IfNotPresent` 下启动且
  `containerStatuses[].imageID` 命中发布 digest;若不成立,需改用 OCI archive + `ctr images import`
  或内网 registry;
- 在上述证据完成前,online digest pin 与离线 tar 是两条独立路线,不得把 online 的通过结论外推到离线。

## 4. 跨子系统改动范围(为何 gated)

一次完整落地横跨:

1. `start.ps1`:BuildPush 加不可变 tag 校验 + push 后解析 digest + 发布后 imageID 验证门;
2. `deploy/k8s/overlays/online/`:kustomize `images` 改用 digest(或由生成器注入);
3. `deploy/k8s/agones/20-fleet-battle.yaml` / `30-fleet-hub.yaml`:battle/hub DS image 改 digest
   (对应 start.ps1 的 `-BattleDsImage` / `-HubDsImage` 独立路径);
4. `export_images.ps1`:离线包 tag / digest 对齐;
5. **Codex/registry ops**:registry manifest 读、digest 解析、kubectl 回读 imageID —— §11.1 归 Codex+人;
6. 生产部署真实执行(apply/push)—— 归人。

属 §11.1 生产部署 / registry 领域 + 跨切部署流水线;且 §14 禁半成品——**imageID 验证门若无
digest pinning 就不完整**(无 digest 时不同节点 imageID 可能因拉取时刻不同而合法地不一致),
四件套必须一起上,不能只落 imageID 验证。故不单方 enact,起草此决策包待人拍板。

## 5. 现状缺口(引用)

- `start.ps1` 第 1423 行:`docker push $Registry/pandora/<svc>:$Tag`,可变 tag、允许覆盖、无
  存在性校验、无 digest 解析;
- overlay / 2 Fleet:`imagePullPolicy: IfNotPresent` + 可变 tag 引用,无 digest;
- 发布链路:`rollout status` / GameServer Ready 后**不回读 imageID**,无法发现跑旧缓存;
- `export_images.ps1`:离线包 tag 与在线 tag 未强制同一不可变标识。

## 6. 验收清单

- [x] **不可变 tag 客户端约束(代码)**:发布 tag 含 git SHA;BuildPush 要求 clean worktree、禁离线
      fallback,本轮严格重建的 20 镜像 provenance 必须等于 HEAD。鉴权/TLS/网络与 not-found 混合
      输出优先失败。
- [ ] **registry 服务端不可变性**:HEAD 预查有 TOCTOU,不能单独证明不可变。BuildPush 当前硬阻断,
      直到目标 registry 启用并验证 native immutable-tag/create-only 策略与发布锁。
- [x] **digest 固定(代码)**:push 输出与 registry 回读 digest 必须一致;运行时 sibling overlay 的
      20 个 Deployment 与 2 个 Fleet 全部使用 `repo@sha256:<digest>`。
- [x] **imageID 验证门(代码)**:rollout 后回读 Deployment Pod 与 Fleet Ready 池的 spec image、
      `imageID`、writer annotation,任一不符即 fail-closed;旧 Allocated GameServer 只排空、不强删。
- [x] **静态反证**:kubectl client 结构化解析 + helper/mutant 覆盖 tag、registry 输出、20 个
      Deployment 目标主容器、五 writer、Fleet 目标容器/两层 annotation、initContainer digest 诱饵、
      mutable image 与 signing secret 注入反例。
- [ ] **真实 online 验证**:尚未访问 registry/k8s,也未 push/apply;须先完成 DSTicket B/C 后再验。
- [ ] **离线包一致**:export_images.ps1 用同一不可变 tag / digest,`docker load` 后 imageID 命中、
      气隙 `IfNotPresent` 不触发拉取。
- [x] **不改 pull policy**:保留 `IfNotPresent`(离线模型硬要求),不引入 `Always`。
- [ ] **独立生产阻断**:DSTicket 必须拍板并完整实现 B(DS 只持公钥 keyset)或 C(只走 online
      authority);硬门完成前不会执行上述 online 发布代码。
- [ ] **生产 etcd 身份**:五 writer 与只读预检须统一支持 custom CA/mTLS/ACL 最小权限身份;
      `prod` 当前在明文/无身份 required 读取前硬阻断。
