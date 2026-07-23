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

**关闭档不是"无防护"**:非空 sjti 的现行性复核、Login 的 MySQL-first 定序 +
Redis 条件写、fenceLoginDelivery 交付终检、Transfer 前后终检、ACK 后置复核+回滚
均不受开关控制,始终生效。开关只决定「对**不带新字段的旧流量**是放行还是硬拒」。

## 2. 票据最大 TTL 怎么取

排空后必须等满"仍在外面流通的最旧票据"的寿命再开 require:

- DSTicket v2(RS256):默认 120s,**上限 180s**(`pkg/auth/dsticket.go`)。
- legacy HS256 DSTicket:`login.ds_ticket_ttl`,默认 **5min**(`pkg/auth/jwt.go`)。

部署内若两种签发器并存(v2 未全量),取 **5min**;v2 全量后取 **180s**。
拿不准就等 5min——多等没有代价,少等会硬拒尚未过期的合法票。

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

### 阶段 C:排空旧版本 + 等满票据最大 TTL

1. 确认无旧版本 Pod:`kubectl -n pandora get pods -o wide` 对照镜像 digest;
   Hub DS fleet 同样确认全部滚到新版(旧 DS 不发 sjti)。
2. 等满一个票据最大 TTL(§2:混用 5min / v2-only 180s),让存量空 sjti 票自然过期。
3. 观察以下信号**为零**后才进入阶段 D:
   - login 日志 `ticket_missing_session_binding_compat_allow`
   - hub_allocator 日志 `hub_admission_missing_sjti_tolerated`(兼容档告警)
   - login 日志 `session_generation_persist_failed`(若有,说明 MySQL 定序权威不稳,先修)

### 阶段 D:开启 require(逐服务,可分批)

1. `login.session_generation_enforce: true` → 滚动重启 login。
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

## 5. hub-allocator `Recreate` 与不停服约束

`deploy/k8s/services/services.yaml` 中 hub-allocator 显式 `strategy: Recreate`,
与「不停服更新」硬约束(PROGRESS.md 2026-07-01)存在张力,这是**记录在案的取舍**:

- hub-allocator 是 assignment/capacity ledger 的**单写者**(replicas=1)。
  RollingUpdate 即使 replicas=1 也会短暂并行旧+新二进制;旧写者不理解后继租约,
  会重新打开重连竞态(正是本事故要关死的窗口)。
- Recreate 的代价是秒级控制面不可用:期间 AssignHub/Transfer 短暂失败可重试,
  **在场玩家不受影响**(DS 会话与已签票据继续有效)。
- 终局方向:leader 选举 + 租约 fencing 的多副本写者(pkg/leader 已有地基),
  届时才能切回 RollingUpdate。在那之前,把秒级控制面窗口换成写者互踢竞态是
  净损失,不做。

## 6. 存量库检查(dbcheck)

- login 启动期:`CheckTables(player_roles, player_session_generations)` +
  `CheckColumns(player_session_generations: sess_jti, generation)`,缺失拒启。
- friend 启动期:`CheckTables(friendships, friend_requests, blocks,
  friend_player_guards, friend_pair_guards)`,缺失拒启。
- 全新库:`deploy/mysql-init/*.sql` / `deploy/tidb-init/*.sql` 已含最终结构;
  既有库:按阶段 A 迁移。两者幂等,可重复执行。
