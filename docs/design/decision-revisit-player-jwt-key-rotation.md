# decision-revisit:玩家 SessionToken / DSTicket 不停服密钥轮换

> 状态:**待人拍板**(2026-07-11 当前工作树已有部分代码接线,但未形成可投产闭环；本文只做审查与方案提案)。
> 关联:`pkg/auth/jwt.go`、login / hub_allocator / matchmaker 的 `JWTConf` 与 `main.go`、
> `tools/scripts/gen_cluster_config.ps1`、外部边缘 Envoy JWKS、
> `docs/design/decision-revisit-ds-key-rotation.md`、CLAUDE.md §9 不变量 16。

## 1. 当前状态与残留问题

当前工作树已出现以下**部分能力**:

- login / hub_allocator / matchmaker 的 `JWTConf` 已有 `additional_secrets`,三个 `main.go` 也传入
  `auth.Config.AdditionalSecrets`；
- `pkg/auth` verifier 已具备多密钥候选能力；hub_allocator 在自身同时可见的两组配置上做了
  玩家/DS key-set 不相交检查。

但这**不等于玩家链路已支持轮换**:

- SessionToken / DSTicket 签发仍未写与主密钥对应的 `kid`；
- 当前生成器随后加入了 `additional_secrets` 注入和双 key JWKS，但这是未经本决策批准的部分接线；
  Online Edge 探测仍不能证明 primary/additional 同时被接受；
- 外部 Edge 没有获批的 K1/K2 重叠发布管线；
- hub_allocator 的本地检查看不到其它服务可能漂移的完整配置集合，尚不能替代统一部署 gate。
- UE Dedicated Server 当前会在在线 `VerifyDSTicket` 前用 HS256 本地验 DSTicket，因而期待
  `PANDORA_DS_TICKET_SECRET`。这不是普通“少挂一个 Secret”：把玩家签名 HMAC 交给不可信 DS，就赋予
  它伪造任意玩家/对局票据的能力；当前 SessionToken/DSTicket 还共用玩家 secret，暴露面更大。
  Battle/Hub Fleet 目前未注入该值，真实生产 key 会全拒票；但**禁止**以向 Fleet 下发 HMAC 签名密钥
  作为修复。

因此直接改主密钥仍会在新旧签发方、后端 verifier 与边缘 Envoy 之间形成拒绝窗口，违反不停服
滚动更新要求。现有部分接线在决策拍板和全链路验收前不得标成“已实现玩家轮换”；生产脚本现已
临时拒绝 additional_secrets，只允许非生产验证，避免把半链路带上线。

另有独立 P0:玩家候选密钥集与 DS 回调候选密钥集必须完全不相交。只比较两把主密钥不够；若玩家
旧/新密钥误入 `ds_auth.additional_secrets`,客户端令牌可能被拿来伪造 DS 回调。

## 2. 待选方案

### 方案 A:继续 HS256,补齐多密钥全链路

- 三份玩家 `JWTConf` 增加仅校验用的额外密钥并接入各自 `auth.Config`；
- SessionToken / DSTicket 按签发主密钥写稳定 `kid`；
- Envoy JWKS 在重叠窗口同时发布 K1/K2,每把 key 使用与后端一致的 `kid`；
- 按“先扩 verifier/Edge → 再翻签发主密钥 → 等最后一枚 K1 令牌过期 → 清 K1”推进。

优点是改动较小。缺点是 `kty=oct` JWKS 本身包含签名密钥，所有持有 Envoy 验签配置的组件也拥有
签名能力；密钥分发面仍较大。

### 方案 B:玩家面迁到非对称签名(保留 DS 离线验签时的生产推荐)

- login / hub_allocator / matchmaker 持有签名私钥；Envoy 与只验签服务只拿公钥 JWKS；
- 用 `kid` 同时发布旧/新公钥完成重叠轮换；DS 回调面继续使用独立密钥体系；
- 需要确定算法、私钥托管/注入方式以及三类签发服务是否共用同一签名身份。

优点是验签侧泄露不能伪造玩家令牌，符合 `pkg/auth/jwt.go` 已记录的生产方向。缺点是迁移与运维成本
更高，且三类签发方的私钥权限边界需要人拍板。

其中 DSTicket 至少应与 SessionToken 使用独立 issuer/audience/keyset；UE DS 只接收 DSTicket 公钥 JWKS，
不能拿任何签名私钥/HMAC。Fleet 引用 revisioned immutable **public keyset**，新旧公钥重叠轮换；签发服务
的私钥由独立 Secret/KMS 身份持有。

### 方案 C:生产只做在线权威验票，DS 不持有玩家验签 key

- UE `PreLoginAsync` 把原始 DSTicket 连同 active DS credential 发给 login；在 online 响应成功前不信任
  本地解析出的任何 claim，也不执行 InitNewPlayer 副作用；
- login 已执行签名/exp/active/projection/assignment/roster/JTI 全链验证，响应返回权威 claims；
- local-off-v1 可保留开发占位离线验签，Agones/online 路径不读取 `PANDORA_DS_TICKET_SECRET`。

优点是彻底不向 DS 分发玩家签名能力，且 online admission 本来就是 Redis Model-B 必经门。缺点是 login/
DS 网关成为每次玩家入场的硬依赖，需明确超时、容量与故障预算；丢失本地廉价签名预筛，恶意流量必须由
:8444 限流/配额吸收。该方案会改变 UE 当前“本地先验签”的设计，也必须人拍板后再改。

## 3. 两种方案都必须满足的 P0 门

1. 部署输入必须同时拿到“玩家 primary+additional”与“DS primary+additional”完整集合；trim 后的
   空项、集合内重复项、两集合任一交集都启动/生成失败，不能静默过滤。
2. 只在生成器比较不够:消费两类密钥的进程须做启动期本地断言；其它服务还需由统一部署预检或
   admission/配置控制面保证同一不变量，避免手工 YAML 绕过。
3. `kid` 只是选键提示,不能成为信任依据；未知/重复 `kid`、错误算法、错误 issuer/audience 一律拒绝。
4. Edge 与后端接受集合必须可观测、可对账；任何阶段都不允许先切签发方再补 verifier。
5. 清退 K1 的等待期以“最后一次 K1 签发时间 + 实际最大令牌 TTL + 缓冲”为准。配置只有默认值，
   不能假设固定上限。

## 4. 风险与迁移成本

- 三个签发服务和外部 Edge 必须按阶段协同，任一步漂移都可能造成全量 401。
- SessionToken 默认寿命长于 DSTicket；阶段驻留时间和 Redis session 生命周期必须一起核对。
- HS256 多 key 会扩大共享秘密暴露面；非对称方案会引入私钥托管、算法迁移和回滚复杂度。
- 生产边缘网关不在本仓库，不能仅凭生成了 JWKS 就声称 Edge 已切换；仍需真实负向/正向探测。

## 5. 人需拍板

- [ ] 选择方案 A(HS256 多 key 过渡)还是方案 B(直接迁非对称签名)。
- [ ] 对 UE DS 验票单独选择 B（DSTicket 公钥离线验签）或 C（仅 online authority）；生产不得把
      玩家 HMAC/私钥注入 Fleet。方案 A 只可用于不向不可信 DS 分发 key 的 verifier/Edge 过渡。
- [ ] 确定签名 key 的权属、托管、轮换触发人与审计来源。
- [ ] 确定完整玩家/DS key-set 不相交的权威配置面与不可绕过的部署 gate。
- [ ] 确定各阶段最短驻留窗口与紧急回滚策略。

## 6. 验收标准(拍板后实现)

- [ ] 新旧副本 × K1/K2 玩家令牌 × Edge/后端 verifier 的阶段矩阵全部通过。
- [ ] 玩家 key-set 与 DS key-set 任一交集、空额外项、重复/未知 `kid` 均启动失败。
- [ ] 签发密钥翻转前,Edge 与全部 verifier 已可证明接受新 key；清退前旧 token 已自然过期。
- [ ] 单密钥默认配置行为兼容,滚动升级与回滚均不要求停服。
- [ ] 真实边缘链路完成无 token、错误 key、当前 key 三段探测并留存 key 指纹审计记录。
- [ ] UE DS 路径证明只持有公开验签材料或完全不持 key；攻陷 DS 不能签出可被 login/其它 DS 接受的
      SessionToken/DSTicket。
