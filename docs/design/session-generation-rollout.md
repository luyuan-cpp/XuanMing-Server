# 会话代际 / 票据 sjti 绑定分阶段发布手册(session-generation rollout)

> 2026-07-23,INC-20260722-004(顶号/会话劫持)R7/R8 收口配套。
> 本文是 login `session_generation_enforce`、login `require_ticket_sjti`、
> hub_allocator `session_gate.require_ticket_sjti` 三个分阶段开关的**发布顺序权威**。
> 违反顺序的后果不是"降级",而是确定性误拒合法玩家(登录/选角/进大厅整体不可用)。

## 0. 背景:为什么必须分阶段

R7/R8 引入了两类新的会话安全机制,它们都要求"写入方先全量就位,校验方才能强制":

1. **MySQL 会话代际定序**(`player_session_generations.generation`):
   Login 先在 MySQL 原子分配单调代际(fail-closed),再对 Redis 做「仅更高代际可
   覆盖」的条件写;SetRole 强制档在同一 MySQL 事务内 `FOR UPDATE` 复核代际。
   —— 旧版本 Login Pod **不写代际**:混版窗口内经旧 Pod 登录的玩家,MySQL 行是
   陈旧的;此时开 SetRole 强制复核会把这些合法会话全部误拒。
2. **票据 sjti 会话绑定**(DSTicket 的 `sjti` claim):
   签发面(matchmaker READY 批签 / hub_allocator Assign/Transfer/迁移重签)把玩家
   当前会话 jti 签进票据;兑换点(login `VerifyDSTicket`、hub_allocator
   `AcknowledgeAdmission`)复核 sjti 是否仍是会话权威当前一代。
   —— 旧版本签发面**持续签空 sjti 票**(不是只有存量票!):混版窗口内硬拒空
   sjti 会令经旧签发面拿票的玩家全部进不了战斗/大厅。

因此审核结论(R8)的顺序硬约束是:

```
迁移 → 全 fleet emit/双写但不强制 → 排空旧版本并等满票据最大 TTL → 最后开启 require
```

## 1. 涉及的开关与默认值

| 开关 | 位置 | 默认 | 关闭档语义 | 强制档语义 |
| --- | --- | --- | --- | --- |
| `login.session_generation_enforce` | login yaml | `false` | Login 双写代际(emit),SetRole 只做 Redis precommit 复核 | SetRole 同事务 `FOR UPDATE` 复核 MySQL 代际,确定性挡旧会话 |
| `login.require_ticket_sjti` | login yaml | `false` | VerifyDSTicket 对空 sjti 票**告警放行**(`ticket_missing_session_binding_compat_allow`);非空 sjti 始终强制复核 | 空 sjti 硬拒 `ErrUnauthorized` |
| `session_gate.require_ticket_sjti` | hub_allocator yaml | `false` | AcknowledgeAdmission 对空 sjti**告警放行**;非空 sjti 始终强制复核 | 空 sjti 硬拒 |

三个开关相互独立、可分别激活;但都遵守同一顺序纪律。

**代码默认 vs 模板默认(R9 复审 P0-1)**:上表"默认"列是**代码零值**(未配置时
false,兼容旧库/dev 裸跑)。而 prod/dev **配置模板已全部改为 `true`**(安全默认
fail-closed):全新部署按模板直接强制;只有「从不带会话代际的旧版本升级」才允许
按本手册阶段序临时置 false,并尽快改回。login/hub_allocator 启动期对
enforce=true 但依赖未就位(迁移未跑/权威未配)会 fail-fast 拒启。

**关闭档不是"无防护"**:非空 sjti 的现行性复核、Login 的 MySQL-first 定序 +
Redis 条件写、fenceLoginDelivery 交付终检、Transfer 前后终检、ACK 后置复核+回滚
均不受开关控制,始终生效。开关只决定「对**不带新字段的旧流量**是放行还是硬拒」。

## 2. 等待窗口怎么取:票据 TTL ≠ 会话 TTL(R9 复审 P0-3)

两类开关的等待窗口**不同**,不能统一按票据 TTL 算:

### 2.1 sjti 票据门(`require_ticket_sjti` 两处):等票据最大 TTL

排空后必须等满“仍在外面流通的最旧票据”的寿命再开 require:

- DSTicket v2(RS256):默认 120s,**上限 180s**(`pkg/auth/dsticket.go`)。
- legacy HS256 DSTicket:`login.ds_ticket_ttl`,默认 **5min**(`pkg/auth/jwt.go`)。

部署内若两种签发器并存(v2 未全量),取 **5min**;v2 全量后取 **180s**。
拿不准就等 5min——多等没有代价,少等会硬拒尚未过期的合法票。

### 2.2 代际强制门(`session_generation_enforce`):等**会话完整生命周期(24h)**

这是 R9 复审指出的漏算项,单独强调:

- Redis 会话(`pandora:sess`)的权威寿命 = **session JWT TTL = 24 小时**,
  与票据 TTL 无关。经**旧版 login Pod**登录的玩家,MySQL 代际行缺失或陈旧,
  但其 Redis 会话在排空旧 Pod 之后仍可存活长达 24h。
- 若只等票据 TTL(180s/5min)就开 `session_generation_enforce`,SetRole 的
  MySQL `FOR UPDATE` 复核会把这些**合法在线会话**全部确定性误拒,直到
  玩家重登。

因此 `session_generation_enforce` 的前置条件二选一:

1. **自然等满**:旧版 login Pod 全部排空后,再等满一个完整 session TTL
   (当前 24h)再开强制档;或
2. **主动收敛**:运维确认或清理所有无 MySQL 代际行的存活会话
   (强制全量重登窗口/停服维护期刷会话),确认后立即开。

判据(确定性,不依赖观测):按「最后一个旧版 login Pod 终止时刻 + 24h」计算。
注意:非强制档(emit-only)下 SetRole **不执行** MySQL 代际复核,不存在"代际
不匹配告警"可观测——不能靠日志判断窗口是否走完,只能按时间或主动收敛判定。

## 3. 发布顺序(runbook)

### 阶段 A:schema 迁移(先于任何二进制)

1. 对 `pandora_account` 执行
   `tools/migrate/migrations/pandora_account/000003_session_generations.up.sql`
   (建 `player_session_generations` / 补 `generation` 列;幂等)。
2. 对 `pandora_social` 执行
   `tools/migrate/migrations/pandora_social/000006_friend_guard_tables.up.sql`
   (好友守卫行表;与本手册同批收口,friend 新版本启动期检查依赖它)。
3. 校验:login / friend 新版本启动期有 `CheckTables` + `CheckColumns`
   fail-fast,缺表/缺列直接拒启并打印本节迁移路径——所以**必须先迁移再发二进制**。

### 阶段 B:全 fleet emit / 双写,不强制(所有开关保持 false)

1. 滚动发布 login / matchmaker / hub_allocator / push 新版本,以及 Hub DS(UE)
   新版本(转发 sjti 到 Hub ACK field 9)。所有 yaml 保持:
   `session_generation_enforce: false`、`require_ticket_sjti: false`(两处)。
2. 该状态下:新 Login 写代际、新签发面签 sjti、新兑换点对空值告警放行——
   与旧版本任意混版都兼容(旧读者不执行新门,新读者对旧流量放行)。
3. hub-allocator 是 `Recreate` 单写者(见 §5),发布时有秒级不可用窗;其余服务
   RollingUpdate 无中断。

### 阶段 C:排空旧版本 + 等满对应窗口(R9 复审 P0-3 修正)

1. 确认无旧版本 Pod:`kubectl -n pandora get pods -o wide` 对照镜像 digest;
   Hub DS fleet 同样确认全部滚到新版(旧 DS 不发 sjti)。
2. **分开两个窗口**(§2):
   - sjti 票据门:等满票据最大 TTL(混用 5min / v2-only 180s),存量空 sjti
     票自然过期后即可进入阶段 D 的第 2/3 步。
   - 代际强制门:等满完整 session TTL(**24h**)或按 §2.2 主动收敛并验证,
     才能执行阶段 D 的第 1 步。**票据窗口满不代表会话窗口满。**
3. 观察以下信号**为零**后才进入对应开关的阶段 D 步骤:
   - login 日志 `ticket_missing_session_binding_compat_allow`
   - hub_allocator 日志 `hub_admission_missing_sjti_tolerated`(兼容档告警)
   - login 日志 `session_generation_persist_failed`(若有,说明 MySQL 定序权威不稳,先修)

### 阶段 D:开启 require(逐服务,可分批)

1. `login.session_generation_enforce: true` → 滚动重启 login。
   **前置:§2.2 的 24h 会话窗口/主动收敛已满足**,仅票据 TTL 满不够。
2. `login.require_ticket_sjti: true` → 滚动重启 login。
3. `session_gate.require_ticket_sjti: true` → 重启 hub_allocator(Recreate)。
4. 每步之间观察误拒率(`ticket_missing_session_binding_rejected`、
   `session_superseded_rejected` 突增即回退该开关,回退无副作用——开关只影响门,
   不影响写入路径)。

### 回滚

任一开关出问题:把该开关改回 false 滚动重启即可,数据无迁移依赖。
二进制回滚到不写代际的旧版:必须**先**把 `session_generation_enforce` 关掉,
否则旧 Pod 登录的会话会被新 Pod 的 SetRole 误拒。

## 4. 已知诚实边界(不是漏洞,是明确取舍)

- **migrate 重签空 sjti**:hub_allocator 系统迁移重签时,玩家已登出(会话权威无
  记录)会签空 sjti 票。该票在 require 档兑换点必拒;玩家重登后 login 按新归属重
  发新票,推送对象本就不存在。不构成绕过(见 `migrateResignSessionJTI` 注释)。
- **dev 裸跑(sessions/sessGate 未配)**:所有现行性门跳过——无权威可比。生产
  配置由启动期校验强制(hub prod `session_gate.require: true` 漏配拒启)。
- **签发器本身不拒空 sjti**:结构性锁死放在兑换点(login/hub 两个 require 门),
  而非签发器——签发器无法区分"dev 无权威"与"漏传",在签发点硬拒会破坏 dev 部署
  与 migrate 已登出场景。兑换点是所有票据的必经收敛点,守住它即守住能力边界。
- **检查后交付窗口**:fenceLoginDelivery / VerifyDSTicket 终检通过与响应写出之间
  仍有进程内窗口;窗口内交付的是"已再次被轮换"的凭据,后续任何过门请求都会被拒,
  不构成持续能力(见 login.go 注释)。

## 5. hub-allocator `Recreate` 与不停服红线——**未解决冲突,状态 OPEN**(R9 复审 P0-7)

`deploy/k8s/services/services.yaml` 中 hub-allocator 显式 `strategy: Recreate` +
replicas=1,与「不停服更新」硬约束(PROGRESS.md 2026-07-01)**直接冲突**。
R9 复审认定这不能再以“记录在案的取舍”关闭——本节如实升级为**待决冲突**,
需要业务方对“发布窗口控制面秒级不可用”与“不停服红线”之间做显式裁决:

### 5.1 为什么现在不能直接改 RollingUpdate

- hub-allocator 是 assignment/capacity ledger 的**单写者**;dsauthfence V3 的
  激活契约就是单 Hub 写者(测试 `TestV3ActivationRequiresSingleHubWriter`
  已把该前提锁死)。
- RollingUpdate 即使 replicas=1 也会短暂并行旧+新二进制;旧写者不理解
  后继租约,会重新打开重连竞态(正是本事故要关死的窗口)。把秒级控制面
  窗口换成写者互踢竞态是净损失。

### 5.2 影响面(诚实表述)

- Recreate 发布窗口内 AssignHub/TransferToLine/AcknowledgeAdmission 控制面
  短暂失败(客户端可重试);**在场玩家不受影响**(DS 会话与已签票据继续有效)。
- 但这仍是“发布必然产生可观测不可用窗口”,不满足不停服红线的字面要求。

### 5.3 终局方案草图(succession fencing,待排期)

1. **跨 Pod 继任租约**:基于 pkg/leader(etcd lease)选举当前写者;新 Pod
   启动后先竞选,拿到租约才打开写路径。
2. **每次继任单调 fencing token**:租约变更时分配单调递增的 succession 代际,
   所有 ledger 写入(Redis Lua/MySQL 条件写)携带并比较该代际,旧写者的
   迟到写被存储层确定性拒绝(与会话代际同构)。
3. **dsauthfence V3 契约同步改造**:单写者前提放宽为“单活跃代际写者”,
   测试契约同步更新。
4. 上述完成前,`Recreate` 是安全下界,**不得**单独把 strategy 改回
   RollingUpdate(会重新引入本事故的竞态窗口)。

## 6. 存量库检查(dbcheck)

- login 启动期:`CheckTables(player_roles, player_session_generations)` +
  `CheckColumnSpecs(player_session_generations: player_id/sess_jti/generation
  含类型与可空性对照,R9 复审 P2)`,缺失/形状不符拒启。
- friend 启动期:`CheckTables(friendships, friend_requests, blocks,
  friend_player_guards, friend_pair_guards)`,缺失拒启。
- 全新库:`deploy/mysql-init/*.sql` / `deploy/tidb-init/*.sql` 已含最终结构;
  既有库:按阶段 A 迁移。两者幂等,可重复执行。
