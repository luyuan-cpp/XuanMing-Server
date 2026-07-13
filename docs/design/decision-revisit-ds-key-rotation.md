# decision-revisit:DS 回调令牌不停服密钥轮换(审核 P1 #3)

> 状态:**待人拍板**(2026-07-11 由 Claude 起草并实现,能力已全量单测通过;是否投产用轮换流程需人确认)。
> 关联:`docs/design/decision-revisit-ds-callback-auth.md`(DS 回调令牌认证本体)、
> `pkg/auth/jwt.go`、`pkg/config/config.go`(`DSAuthConf`)、`pkg/middleware/dsauth.go`、
> CLAUDE.md §9 不变量 16(不停服滚动更新)、AGENTS.md §7。

## 1. 旧问题

DS 回调令牌认证落地时(见 `decision-revisit-ds-callback-auth.md`),密钥模型是**单密钥**:

- 全集群共用一把 `ds_auth.secret`;
- 校验侧 `auth.Verifier` 只认这一把;
- 令牌头无 `kid`,校验侧无从判断令牌用哪把密钥签。

后果:**密钥无法不停服轮换**。要把 K1 换成 K2,滚动升级期间新旧副本共存:

- 已滚到 K2 的签发方(allocator)签出的令牌,发给仍在 K1 的校验方 → `signature invalid` → 401;
- 反之亦然。

无论先滚签发方还是先滚校验方,都会出现一段「签发密钥 ≠ 校验密钥」的窗口,导致 DS 回调
(结算 / 心跳 / 定位)大面积 401。这直接违反 CLAUDE.md §9 不变量 16「任何依赖『先停服』
才能上线的设计一律拒」——单密钥轮换实质要求停服换密钥。

## 2. 新方案(已实现,待拍板投产流程)

标准做法:**校验侧多密钥(签发侧仍单密钥)+ 令牌 `kid` 路由**,配合三段式滚动。

### 2.1 数据结构

- `auth.Config` 新增 `AdditionalSecrets [][]byte`:**仅用于校验**的额外可接受密钥,不用于签发。
- `config.DSAuthConf` 新增 `additional_secrets []string`(yaml `additional_secrets`)。
- `pkg/middleware/dsauth.go` 的 verifier / guard 构造器把 `additional_secrets` 透传给 `auth.Config`;
  signer 构造器**不变**(签发永远只用主 `secret`)。
- 每把额外密钥仍须 ≥32 字节;默认空 → 单密钥,行为与历史逐字节一致(见 §4 兼容性)。

### 2.2 kid 路由

- `SignDSCallback` 在令牌头写 `kid = keyFingerprint(主密钥)`,其中
  `keyFingerprint = hex(SHA256(secret)[:8])`(不泄露密钥,仅作定位)。
- 校验侧 keyfunc:
  - 令牌带 `kid` 且命中某把候选密钥指纹 → 只用那把校验(快路径,精确);
  - 无 `kid` / 未知 `kid`(如轮换前签发的历史令牌) → 用 golang-jwt v5 的
    `jwt.VerificationKeySet` **依次尝试**主密钥 + 全部 `additional_secrets`;
  - 候选只有一把时,直接返回该密钥(与历史单密钥路径完全一致,零行为漂移)。
- **kid 只打在 DS 回调令牌上**。玩家 SessionToken / DSTicket 不打 kid——它们经 Envoy
  `jwt_authn`(JWKS `kid=pandora-dev`)校验,擅自改 kid 会破坏边缘网关校验。DS 回调令牌
  不经 Envoy jwt_authn(只由 `DSCallbackGuard` 校验),加 kid 头对边缘无影响。

### 2.3 三段式不停服轮换流程(K1 → K2)

前置:各服务 `ds_auth.secret = K1`,`additional_secrets = []`。

1. **阶段①(扩校验面)**:各服务 `additional_secrets` 加入 K2(`secret` 仍 K1),滚动重启。
   - 此时:签发仍用 K1;校验接受 {K1, K2}。旧令牌(K1 签)全程有效。
2. **阶段②(翻主密钥)**:各服务 `secret = K2`,`additional_secrets = [K1]`,滚动重启。
   - 共存窗口:已滚副本 = 主 K2 + 额外 K1;未滚副本 = 主 K1 + 额外 K2。
   - 无论令牌由 K1 还是 K2 签,两类副本都接受 → **无 401 断档**(单测 `TestRotationPhase2Coexistence` 覆盖)。
   - 需从“最后一次用 K1 签发”起等待所有 K1 令牌自然过期后再进阶段③。代码**没有 TTL 上限**:
     默认 battle=4h、hub=24h；启动校验只规定 battle ≥ max(1h,`battle_ttl+15m`)、hub ≥1h，
     `local+enforce` 的 hub 另要求 ≥12h。因此等待窗口必须读取本次实际配置，不能写死成“≤4h/≤24h”。
3. **阶段③(清退旧密钥)**:各服务 `additional_secrets = []`(仅剩主 K2),滚动重启。
   - K1 签的令牌自此被拒(单测 `TestRotationPhase3OldKeyRejectedAfterCleanup` 覆盖)。

每阶段内部都是标准滚动重启,任意副本可随时被杀被替换,满足不变量 16。

**运维顺序硬规则(与 A#4 令牌代际门控联动)**:每个阶段必须**先滞完 hub_allocator 全部副本、
再观察 DS 令牌轮换生效**。原因:滚动窗口内若仍有不带令牌代际门控的旧版 allocator 副本存活,
它们收到持旧令牌的 Hub 心跳会直接把分片 warming→ready,绕过新版的 `current_token_exp_ms`
代际比对;只有 allocator 全部换成带门控的版本后,“旧令牌心跳不得转 ready”才真正兼容。

### 2.4 部署侧机制门(审核 P1 #1/#6/#7,投产前必补)

§2.3 是**逻辑正确性**;要在真实 k8s 发布里不翻车,部署脚本(`start.ps1` / `gen_cluster_config.ps1`)
还须补三道**机制门**。当前均为 TODO,批准投产轮换前逐项补齐并纳入 §5 验收:

1. **跨代全集不相交门(读现网 Secret,而非只比本次 env)**:轮换发布时,部署侧必须从目标集群
   **读取当前生效 Secret**(旧集合),与本次待发布新集合合并后断言:
   - `(old_player ∪ new_player) ∩ (old_ds ∪ new_ds) = ∅`——玩家面 / DS 面跨面永不相交(任一面泄露
     不得伪造另一面);**新 DS key 不得等于任一旧 player key,反之亦然**;
   - 阶段②翻主密钥须满足 `new_active_signing ∈ old_verify_set`(新签发密钥已在旧副本校验集合里)
     且 `new_verify_set ⊇ {old_active_signing}`(旧签发密钥仍被新副本接受)——**禁止 primary 直接
     K1→K2**:未先扩校验面就换签发密钥,滚动期旧、新副本互拒令牌 401。
   - 现状缺口:`start.ps1` 只比较**本次 env 变量**,不读现网 Secret,上述**跨代**关系无法校验。
     `gen` 侧已做本次四把 key 的 pairwise 不相交(**单代内**正确),缺的正是跨代 `(old∪new)` 的集群侧门。

2. **Secret 发布事务边界(内容 hash + resourceVersion CAS + 发布锁 + 失败回滚)**:同名可变 Secret
   覆盖前先读现网 `resourceVersion` 与内容 hash,以 **CAS(resourceVersion 未变才写)**更新,防并发
   发布互相覆盖;发布用**集群级锁**(k8s Lease / etcd lease)串行化;覆盖后 Fleet / overlay / rollout
   任一步失败 → **回滚到覆盖前 Secret 内容**(保留 pre-image),杜绝"新 Secret + 旧 workload"半发布态。
   现状缺口:第 141/1420 行覆盖后只核对 key 名,无 hash / CAS / 锁 / 回滚。

3. **迁移前 revision 回退安全(ConfigMap→Secret 迁移的回滚下界)**:迁移删旧 ConfigMap 时虽豁免零副本
   历史 ReplicaSet,但 `kubectl rollout undo` 回退到**迁移前 revision** 时旧模板仍引用已删 ConfigMap →
   Pod 挂载失败。二选一(投产前定):**(a)** 保留迁移前 N 个 revision 引用的 ConfigMap,直到这些
   revision 被 rollout history 自然淘汰再删;**(b)** 封版迁移边界:显式声明"不可回退到迁移前 revision",
   打标 rollout history 并在文档写明回退下界。现状缺口:注释仅称其为"回退边界",未真正保留回退能力,
   须明确落其一。

## 3. 风险

- **配置面复杂度**:轮换需人工三段推进配置,阶段②后必须从最后一次 K1 签发起等旧令牌 TTL
  过完才进阶段③,否则会拒掉尚未过期的旧令牌。TTL 仅有上述下限、没有上限；运维必须读取
  本次实际 `battle_token_ttl` / `hub_token_ttl` 排期(阶段②驻留 ≥ 实际 max(TTL)+缓冲)。
- **误配**:`additional_secrets` 里放了错误 / 过短密钥,构造 verifier 时 `auth.Config.Validate`
  直接报错(≥32 字节校验),属 fail-closed,不会静默降级。
- **密钥数量**:候选密钥每多一把,无 kid 令牌的最坏校验成本 +1 次 HMAC。DS 回调令牌均带 kid,
  走精确快路径;仅历史无 kid 令牌走遍历,轮换期短暂存在,影响可忽略。
- **gen_cluster_config 注入**:当前工作树已有 `-SecretAdditional` / `-DsSecretAdditional` 与双 key
  JWKS 的部分管线及校验；但整体决策仍待人拍板，Online/`-Prod` 现暂时 fail-closed 拒绝
  additional，避免未经批准的轮换阶段投产。批准后还须补 Edge/服务阶段化验收再移除生产门。

## 4. 兼容性 / 迁移成本

- **默认零行为变化**:`additional_secrets` 缺省为空 → 候选只有主密钥 → keyfunc 走单密钥直返
  路径,签发照旧,校验照旧。现网无需任何动作即保持现状。
- **令牌头新增 kid**:纯附加头,旧校验方(尚未部署本次改动的副本)对未知头忽略,仍用其单密钥
  校验;新校验方对无 kid 旧令牌走遍历。滚动升级本次改动本身也不需要停服。
- **无 proto / 存储变更**:不碰 Redis pb、不碰 MySQL、不碰 kafka,与不变量 17 无关。
- **无新增第三方依赖**:仅用已在 `pkg/go.mod` 的 `golang-jwt/jwt/v5 v5.2.2`(其
  `VerificationKeySet` 自 v5.2.0 起提供)。**无需 `go mod tidy`**。

## 5. 验收标准

- [x] 单密钥场景行为与历史一致(`TestRotationSingleKeyUnchanged`)。
- [x] 阶段①旧令牌被扩展校验面接受(`TestRotationPhase1OldTokenAcceptedByExtendedVerifier`)。
- [x] 阶段②新旧副本 × 新旧令牌四组合全接受(`TestRotationPhase2Coexistence`)。
- [x] 阶段③清退后旧密钥令牌被拒(`TestRotationPhase3OldKeyRejectedAfterCleanup`)。
- [x] 无关密钥令牌一律被拒(`TestRotationUnknownKeyRejected`)。
- [x] DS 回调令牌带 kid = 主密钥指纹(`TestRotationKidHeaderPresent`)。
- [x] `pkg/auth`、`pkg/config`、`pkg/middleware` 及五个消费服务
  (hub_allocator / ds_allocator / battle_result / player_locator / data_service)全部 build 通过。
- [x] `additional_secrets` 的非生产生成/校验/双 key JWKS 部分管线已接，冒烟通过。
- [ ] **部署侧跨代全集门(§2.4-1)**:轮换发布读现网 Secret,校验 `(old∪new)` 玩家面/DS 面跨面不相交 +
      阶段② `new_signing∈old_verify` 且禁止 primary 直切;有脚本自检 / 单测。
- [ ] **Secret 发布事务(§2.4-2)**:内容 hash + resourceVersion CAS + 集群发布锁 + 失败回滚 pre-image。
- [ ] **revision 回退安全(§2.4-3)**:保留迁移前 revision 的 ConfigMap 至淘汰,或封版并明确回退下界(二选一落地)。
- [ ] **待人拍板**:是否批准现有 HS256 additional 方案及生产三阶段流程；批准前生产生成器保持拒绝。
