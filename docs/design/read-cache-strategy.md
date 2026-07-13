# MySQL 服务读缓存策略(Redis cache-aside 补齐计划)

> 状态:**部分落地**(2026-07-12)——P0 `guild` 读缓存已实现上线路径(见 §3 P0 guild);`mail` / `friend` 仍**设计待拍板**。服务级决策,索引挂 `pandora-arch.md` §11 / 各 social 服务 README。
> 触发:扩容评审时确认「除事务权威外,其余 MySQL 服务上量后是否需要 Redis 缓存」。
> 前置:`scale-cellular-20m.md`(单 Cell ~40 万 CCU 天花板)、`player-data-actor-serial.md`(缓存层定位)、`zero-downtime-update.md`(不变量 16/17)、`CLAUDE.md §9`。
> 边界:**本文只覆盖缓存补齐,不动分片/单元化**。分片是水平扩容(scale-cellular-20m.md),缓存是单实例读放大挡箭牌,两者正交、都要,不互相替代。

---

## 0. 一句话结论

- 现状:`data_service` 与 `guild` 已挂 Redis 旁路缓存;其余 MySQL 服务(player / inventory / friend / chat / mail)仍直连 MySQL。
- 上量后**不是「都加」也不是「都不加」**,判据是 **读热度 × 重复读命中率 × 数据是否多人共享**,与「是否需要分布式事务」无关。
- **补缓存计划**(多人读同一份 / 登录必拉 / 全服读同几行):`guild` 已落地;`friend`、`mail(系统 + 公会邮件)`仍待拍板。
- **保持直连、靠分片扩**(事务权威 / 冷数据 / 已有加速层):`inventory`、`chat`、`auction`、`account/login`。
- 全部走 **cache-aside + 写后删**,MySQL 永远是事实源;缓存读写失败不影响权威写入,删除失败必须告警;删除失败或并发迟到回填可能在短 TTL 内读到旧值,由 TTL 自愈。

---

## 1. 现状盘点(截至 2026-07-12 的代码事实)

| 服务 | 权威存储 | 是否已有 Redis 缓存 | 现状说明 |
|---|---|---|---|
| data_service | MySQL(pandora_player,版本乐观锁数据) | 🧪 cache-aside 开发实现已闭环，**未正式上线** | Ping 失败降级直连；当前仅本地开发/minikube 验证 |
| account/player | MySQL(pandora_player) | ❌ 裸连 | 玩家档案 / 领奖记录 |
| economy/inventory | MySQL(pandora_trade) | ❌ 裸连 | `FOR UPDATE` + 幂等流水,事务权威 |
| economy/auction | MySQL(pandora_auction)+ Redis 订单簿 | ✅(订单簿already) | 读加速层已在,成交权威在 MySQL |
| social/friend | MySQL(pandora_social) | ❌ 裸连 | 好友 / 申请 / 黑名单 |
| social/guild | MySQL(pandora_social) | ✅ cache-aside(2026-07-12 落地) | 公会资料多人共享热 key,已补 Redis 旁路缓存 |
| social/chat | MySQL(pandora_social) | ❌ 裸连 | 私聊历史 |
| social/mail | MySQL(pandora_social) | ❌ 裸连 | 系统 / 公会 / 个人邮件 |
| account/login | MySQL(pandora_account)+ Redis | ✅(session/防重放) | 账号行只登录读一次 |

---

## 2. 判据:什么时候缓存真的有收益

缓存不是「越多越好」,给事务路径乱加缓存反而制造一致性 bug。用三个维度判断:

| 维度 | 问题 | 高收益信号 |
|---|---|---|
| 读热度 | 这行数据每秒被读多少次? | 全服反复读、登录必读 |
| 重复读命中率 | 同一个 key 短时间内被读几次? | 多人读同一份 / 重连风暴重复读 |
| 共享度 | 是一玩家私有还是多人共享? | 多人共享(热 key)收益最大 |

- **多人读同一份**(guild 资料、sys_mail):命中率数量级提升,MySQL 热行 / 热索引直接被挡掉 —— **必加**。
- **登录必拉的私有数据**(好友列表):稳态命中率一般,但**重连风暴**下同批 key 被反复读,是缓存救命的核心场景(与 data_service 做 cache-aside 同理)—— **该加**。
- **冷数据 / 翻页读**(chat 历史):频率低、每次不同 key、MySQL 索引扛得住 —— 缓存收益低,**不加**。
- **事务权威 + 写重于读**(inventory 余额):缓存收益低、失效一致性风险高,瓶颈优先用**分片**解决 —— **不加**(读侧是「开背包读一次 + 变更推送」,重复读少)。

> 关键澄清:「每玩家登录读一次」的私有数据(档案、背包、邮件游标)对**稳态**帮助有限——每次都是不同 key,靠不了缓存,靠分片。缓存真正救的是**多人共享热 key**和**重连风暴的重复读**。

### 2.1 TiDB / 分片 vs Redis:不是替代关系

TiDB 或 MySQL 分片解决的是「单 MySQL 容量 / 并发 / 跨节点 SQL 事务」问题;Redis 解决的是「热点读挡板 / 极低延迟 / 特殊数据结构 / 临时状态」问题。两者可以减少彼此的压力,但不是同一层能力。

- **TiDB / 分片能替代的复杂度**:为绕开单机 MySQL 上限而做的手工分库分表、应用侧路由、部分纯读压缓存。应用仍以 SQL / MySQL 协议访问,容量和普通读写并发靠多节点摊开。
- **Redis 仍要保留的场景**:同一个 key 被全服反复读写、亚毫秒级读写、session / ticket / locator、限流、排行榜、订单簿、在线状态、Pub/Sub、临时队列。
- **判断口径**:如果 Redis 只是给 MySQL 擦读压力,且访问 key 能被 TiDB / 分片自然摊开,可以评估减少;如果 Redis 承担的是热点内存层、状态机或专用数据结构,不能因为上 TiDB 就删。
- **Pandora 当前结论**:`friend` / `chat` 的社交库迁 TiDB 是为了降低跨人关系和容量扩展复杂度;`guild` / `mail` 的多人共享热 key 仍按本文补 cache-aside;`auction` 订单簿、`leaderboard` 实时榜、`player_locator` 在线位置这类 Redis 权威或加速层不因 TiDB 取消。

---

## 3. 补齐计划(按优先级)

### P0 —— social/guild(热 key 读放大,收益最大)（✅ 2026-07-12 落地）

- 热点:`GetGuild` / `GetMyGuild` 被全公会成员反复读;公会资料是典型多人共享热 key。
- 缓存对象(已实现 `services/social/guild/internal/data/cache.go`):
  - `pandora:guild:info:{guild_id}` → guildv1.Guild proto 快照(魔数 + 字段位图头,防滞升级投毒),cache-aside + 写后删。
  - `pandora:guild:member:{player_id}` → guild_id 反查(魔数 + 8B),入会 / 退会 / 踢人 / 解散时删;不做负缓存。
- 不缓存:`ListMembers` / `ListPendingRequests`(已 cursor 分页,成员变更频繁,列表缓存失效面大)。
- 不缓存 `GetMember` 的 role(权限敏感,只走权威 MySQL;repo 事务内 FOR UPDATE 复核)。
- 一致性:公会资料改动(审批增员 / 退会 / 踢人 / 转让 / 解散)在 MySQL 事务成功后删对应 key;删失败仅告警,靠短 TTL(默认 60s)兜底。
- 接线:`node.redis_client` 弱依赖(Ping 失败降级直连 MySQL),`guild.cache_ttl` 默认 60s。

### P0 —— social/mail(系统 / 公会邮件全服读同几行)

- 热点:`ListSysSince` / `ListGuildSince` —— 全服玩家登录都读**同一批 `sys_mail` 行**(mail.md §2.1「一份数据 + 玩家游标」),MySQL 热行必被打爆。
- 缓存对象:
  - `pandora:mail:sys:list` → 当前有效系统邮件列表(MailRow 集合),整体缓存;发新系统邮件 / 邮件过期时失效。
  - `pandora:mail:guild:{guild_id}:list` → 该公会当前有效群发邮件。
- **不缓存个人邮件**(`ListPersonal` 是写扩散、per-player 私有、翻页读)与领取状态(`player_mail_claim` 幂等必须打 MySQL)。
- 失效:系统 / 公会邮件是低频写、超 `end_ms` 过期,适合「短 TTL(如 30~60s)+ 发布时主动删」;可复用 `config-table-hotreload.md` 的 etcd version 通知模式做跨实例失效(只传版本号,不传邮件体)。

### P1 —— social/friend(登录必拉 + 重连风暴)

- 热点:`ListFriends` 登录必拉;`ListIncomingRequests` / `ListBlocks` 次高频。在线状态高频刷新**本就该只放 Redis**(player_locator),不落 MySQL。
- 缓存对象:`pandora:friend:list:{player_id}` → FriendRow 列表快照(受不变量 §18 `max_friends=200` 硬上限兜住,单 key 体积可控)。
- 失效:AcceptFriend / DeleteFriend 事务成功后删该玩家及对端两个 key(双向关系,两边都要删)。
- 收益场景:重连风暴下同批玩家秒级重登,好友列表重复读被缓存挡掉。

### 暂不加(保持直连 MySQL)

| 服务 | 不加理由 | 到瓶颈时的解法 |
|---|---|---|
| economy/inventory | 事务权威(不变量 §9.7),`FOR UPDATE` + 幂等流水;读侧重复少 | **分片**(`player_id % N`),不是缓存余额 |
| social/chat | 私聊历史冷数据、翻页拉取、频率低,索引扛得住 | 可选缓存「最近 N 条」,非必须 |
| economy/auction | Redis 订单簿已是读加速层,成交权威在 MySQL,已闭环 | 已闭环 |
| account/login | 账号行只登录读一次,瓶颈在 bcrypt 非 DB;session/防重放已 Redis | 无需 |
| account/player / data_service | data_service 已 cache-aside;player 私有数据靠分片 | 分片 |

---

## 4. 统一实现约定(所有新增缓存遵守)

对齐 data_service 现有 cache-aside 实现(`services/data/data_service/internal/data/cache.go`):

1. **MySQL 是唯一事实源**,Redis 是弱一致旁路缓存。缓存 miss / 反序列化失败 → 回落 MySQL,不报错给上层。
2. **写路径:先写 MySQL(事务),后删缓存(cache-aside)**。删失败只告警不回滚,靠 TTL 最终失效。**不做 write-behind 脏写回 Redis**,避免与不变量 §16 排空冲突。
3. **缓存 value 存 proto bytes 快照,不存 `*StorageRecord` 原样**;面向客户端仍走最小视图组装(不变量 §14)。
4. **只缓存客户端可见结构或其原料,不缓存幂等键 / 审计字段**。
5. **Redis 弱依赖**:Ping 失败降级为直连 MySQL(`cache=nil`),不阻塞服务启动(对齐 data_service)。
6. **key 用 hashtag 括业务 ID 保 slot 一致性**(如 `pandora:guild:info:{guild_id}`),兼容 Redis Cluster / 单元化。
7. **不引入新中间件**(不接 Apollo / Nacos);跨实例失效复用 etcd version 键(config-table-hotreload.md 模式),不存数据体。

---

## 5. 不变量影响(对照 CLAUDE.md §9)

| 不变量 | 是否成立 | 说明 |
|---|---|---|
| 14 客户端最小视图 | ✅ | 缓存存快照,response 仍服务端组装最小视图 |
| 16 不停服滚动更新 | ✅ | 缓存纯旁路,SIGTERM 无脏数据回写;进程死缓存自然失效 |
| 17 Redis pb 兼容演进 | ✅ | 缓存 value 是可丢弃快照(非权威存储),但仍按加字段 / reserved 演进,禁改编号 |
| 18 累积列表上限 | ✅ | friend/guild 列表已有写入侧上限 + 读取侧分页,缓存不放大上限 |

---

## 6. 落地纪律

- 本文**分服务落地**:P0 `guild` 已按本文实现(cache-aside + 写后删,`services/social/guild`);`mail` / `friend` 仍待拍板,拍板后另起任务,按 `AGENTS.md §4` 直接执行,Claude 写代码 + 补 go.mod,`go mod tidy` 由 Codex 执行(§11.1)。
- 对齐 `scale-cellular-20m.md §7`:缓存补齐应在**阶段 1 单 Cell 满载压测**前完成,并用 `stress-discipline.md` 的对比表验证「加缓存后 MySQL 热行 QPS / P99 下降」,没有对比表不许声明「缓解了热点」。
- 顺序:P0(guild / mail)→ P1(friend);inventory / chat 明确不加,不为「统一」乱加。
