# 实时成长入账通道(玩家经验 + 战斗掉落即时到账)设计

> 状态:**已拍板(2026-07-20),实施中(2026-07-21)**。
> - §1 修订稿已合入 `ds-arch.md` §0.5 ③ / §0.6;
> - proto(battle/player)、player 服务、battle_result 服务、SQL(mysql-init + tools/migrate)、
>   Envoy AddExperience 403 拦截已落地并通过 build/test(本仓库);
> - UE 侧(进度上报器 / Loot 掉落广播 / 客户端经验适配)在 Pandora-Client 仓库跟进,
>   剩余接线与验收见交接记录(PROGRESS.md 2026-07-21 条目)。
>
> **2026-07-21 发布前审计修订**(两个混版 P0 + 一批 P1,详见当日审计记录):
> 1. 经验推送改走**独立 topic `pandora.player.experience`**(原设计复用 pandora.player.update
>    + event_type header;player 旧副本消费该 topic 不看 header,混跑窗口会把经验事件按
>    MMR 事件误解码污染段位,§9.21)。push 服务补订阅;§2/§4.3/§7 已同步修订。
> 2. 实时通道**默认关闭**(`progress_enabled` 缺省 false,§14.2):battle_result 全 fleet
>    升级后才允许启用,否则旧代码副本结算不感知水位表 → 混版窗口双发掉落(P0)。
>    每场模式以水位行存在性固化:killswitch 中途关闭不影响已开流对局(防丢奖)。
> 3. 拾取出箱**每事实一行**(Seq=事实自身 seq),合法掉落不再截断;单事实 count 夹紧 ≤46。
> 4. 新增**单场累计上限**(total_exp/total_items 随水位同事务 CAS 累计,超限整批拒)。
> 5. 未知 fact 类型**整批拒**(原"跳过并推进水位"= 新 DS 新事实被静默 ACK 永久丢失)。
> 6. 进度出箱失败行**指数退避**(next_attempt_at_ms/attempt_count,防队首阻塞)。
> 7. `PlayerProfile` 经验字段改用 50/51 编号(不收窄 12-49 预留段,buf breaking 门禁绿)。
> 8. player/battle_result 启动加经验/进度 **schema gate**;pandora_player 000002 exp 列
>    改条件加列(fresh-init 兼容)+ ALGORITHM=INSTANT;exp_history 加 7 天保留期后台清理。
>
> **2026-07-21 MMO 化再拍板(拾取 ACK 门控,掉落零丢失;用户拍板)**:产品形态从 MOBA 转向
> MMO,"打到即所得"升级为**局中已得掉落绝对不丢**的硬需求,原 §9"尾窗丢失明示接受"作废。
> 方案:UE 侧把"捡到"的定义反转为**入包 = 已持久化**——拾取权威点先认领锁定掉落物
> (他人不可拾、暂停自动清理),事实经本通道上报,服务端**水位 + 出箱同事务提交**后回执,
> 回执确认才入战斗背包并销毁掉落物;事实确定未被应用时释放认领,物品回地面可重拾。
> 新不变量:**战斗背包 ⊆ 后端已入账**,DS 任意时刻崩溃都不存在"玩家已见入包但未持久化"
> 的物品。曾评估的 DS 本地 WAL 方案被否:Agones 临时 Pod 的本地盘在 Pod 替换 / 节点宕机
> 时消失,加 PV + 回捞基建成本高且仍达不到零丢失。Go 侧零功能改动(ReportProgress 的
> ACK 本就是持久化确认);约束 ④ 措辞相应修订(见 §1);§9 残余风险改写。
> 认领组不拆批 / 回执恰好一次 / 释放-重拾防复制等 UE 侧契约见 Pandora-Client
> `PandoraBattleProgressReporter.h` 头注释。

## 0. 需求与结论

**需求 1:玩家等级经验**
- 击杀怪物、完成任务后**即时**增加经验;经验满升级,支持一次连升多级;最高 Lv.15,满级显示 MAX 不再累加。
- 客户端登录 / 重连 / 经验变化时刷新经验条;升级时播放升级表现(客户端 UI 已预留:
  `UMyMainView::SetExperienceDisplay` / `PlayLevelUpPresentation`,配置表 `CfgPlayerLevelExp`)。

**需求 2:高品质掉落广播**
- 玩家获得金品质及以上物品时,**本场战斗同队玩家**可见"某玩家获得了某物品";多条依次展示。
  紫及以下不广播;普通聊天不在范围(客户端 UI 已预留:`EnqueuePublicBroadcast` 串行播放队列)。

**结论(架构分工)**
| 关注点 | 归属 | 理由 |
|---|---|---|
| 经验数值权威 / 等级结算 / 持久化 | Go `player` 服务 | 大厅态持久化;§9.6 DS 不可信 |
| 击杀 / 拾取事实的实时上报 | Battle DS → Go `battle_result`,**异步批量** | 本文档新增的第三通道(§1) |
| 经验换算(怪→经验)、掉落白名单校验 | Go `battle_result` | 与"MMR 在 battle_result 算"同构 |
| 经验条刷新 / 升级表现触发 | 独立 topic `pandora.player.experience` → push → 客户端(§4.3) | 复用既有推送管线(混跑安全见 §4.3) |
| 掉落广播(同队可见、即时) | **Battle DS 组播,Go 零参与** | 纯瞬时表现,ds-arch §0.1;可见域(同场同队)只有 DS 手里现成 |

## 1. ds-arch §0.5 / §0.6 契约修订稿(待合入)

红线原文**不动**:"DS 战斗中绝不同步调用大厅服务"。修订是给 §0.5 的合法通道清单
(现为"开局快照 + 局后上报")**新增第三条**:

> **③ 局中异步事实上报(realtime progression,2026-07-20)**
> Battle DS 可在战斗中把**事实事件**(击杀了怪物 X / 拾取了白名单内物品 Y)异步批量上报
> `battle_result.ReportProgress`。约束(逐条硬性):
> 1. **绝不阻塞 tick**:上报在独立线程 / 异步任务,fire-and-forget + 本地有界缓冲重试;
>    战斗逻辑不等待、不读取任何回包结果(ACK 只用于清缓冲)。
> 2. **DS 只报事实不报数值**:不上报"加多少经验 / 发什么奖";换算与发放全在 Go
>    (经验换算 + 掉落白名单在 battle_result,等级曲线在 player)。
> 3. **每事件幂等**:事件键 = `(match_id, seq)`,seq 每场单调;at-least-once 重放零副作用。
> 4. **Go 不可用不影响战斗,但暂停拾取入账**(2026-07-21 MMO 拍板修订):上报失败只缓冲
>    重试,战斗照常;门控拾取(入包=已持久化)在通道不可用期间保持认领等待或留在地面,
>    恢复后自动继续——这是 MMO 的标准语义(背包服务在线才叫捡到),不构成阻塞 tick。
> 5. **单一权威路径**:一场对局的发放要么全走本通道,要么全走局后结算,服务端强制二选一(§5)。
>
> 复杂度举证(CLAUDE.md §15.4):简单方案"局后一次性入账"无法满足已确认的产品需求
> "打到即所得、DS 崩溃不作废"(PvE 产出跨局持久,与 MOBA 局内金币随局清零语义不同)。
> 新增组件:ReportProgress RPC + 进度水位表 + 进度出箱;失败模式与验证见本文档 §5 / §9。
> 回退路径:killswitch 关闭本通道后,DS 停止流式上报,自动退回局后结算路径。

§0.6 的"一句话红线"补一句话:"战斗中对大厅服务的**异步事实上报**(§0.5 ③)不在禁列,
但必须满足 ③ 的五条约束"。

## 2. 链路总览

```
【经验】
DS: 怪物死亡(权威判定)
 ├─(即时)本地飘字/击杀表现(GameplayCue,零延迟,纯表现)
 └─(异步)进度事件入本地缓冲 → 批量 ReportProgress(match_id, events[seq…])
      → battle_result: Guard+active 鉴权 → 水位去重 → 怪物表换算经验
         → 同一事务: 推进水位 + 写 progress 出箱
      → 出箱 worker: player.AddExperience(delta, key="progress:{match_id}:{seq}")
      → player: 幂等入账 → 等级曲线结算(连升多级/Lv15 封顶/满级 no-op)
         → 同一事务写推送出箱(event_type=EXPERIENCE)
      → kafka pandora.player.experience(独立 topic,§4.3)→ push → 客户端
         SetExperienceDisplay(levels_gained>0 → PlayLevelUpPresentation)

【任务经验】(纯 Go,无 DS 参与)
任务完成判定点(现有 ClaimReward / 未来 campaign) → player.AddExperience(key="quest:{player}:{quest}")
  → 同上推送链路

【掉落】
DS: RecordDroppedItem / 拾取权威点
 ├─(即时)品质≥金 → 对同队在场玩家 ClientRPC 广播(player_id,item_config_id,count)
 │      → 客户端适配层: 去重/本地化/显示名解析/品质颜色(CfgItem) → EnqueuePublicBroadcast
 └─(异步)ItemPickupFact 进同一 ReportProgress 流
      → battle_result: 白名单校验 → 出箱 → inventory.GrantInstances / GrantItems
         (key="progress:{match_id}:{seq}") → 即时到包

【登录/重连兜底】
登录 / 重连 → player.GetProfile(扩展经验字段)刷权威快照;
push 离线 ZSET(5min)补推经验事件,超窗由快照覆盖。
```

## 3. 事件流协议(幂等 / 顺序 / 水位)

- **流标识**:battle 场景 stream = `match_id`(一场一 DS 进程,崩溃即 ABANDONED,无重建歧义)。
- **seq**:DS 侧每场从 1 单调递增;**单飞行批 + 批内升序 + 失败原批重发**(不重排、不跳号)。
- **服务端去重**:`battle_progress_stream(match_id PK, last_applied_seq, updated_at_ms)`;
  批内 `seq <= last_applied_seq` 跳过,其余按序应用,**水位推进与 progress 出箱写入同一 MySQL 事务**。
  响应回 `acked_seq`,DS 据此清本地缓冲。
- **下游幂等**:出箱 worker 调 `player.AddExperience` / `inventory.GrantInstances`,
  幂等键 = `progress:{match_id}:{seq}`,复用两服务既有 `uk(player_id, idempotency_key)` 模式;
  RPC 超时 / UNKNOWN 保留出箱行重试,明确成功才删行(与 battle_result 既有出箱纪律一致)。
- **DS 本地缓冲有界**(§9.18 同类纪律):击杀事实按 `(player_id, monster_config_id)` 批内聚合
  计数,缓冲增长按怪物种类有界;拾取事件天然低频。硬上限(默认 4096 条)满载时丢最老
  未发认领组并释放认领(物品回地面可重拾)+ 告警计数(残余风险入 §9)。
- **拾取 ACK 门控**(2026-07-21 MMO 拍板):拾取事实入队即尝试发送(守单飞行批 + 退避门,
  定时器兜底重试),`acked_seq` 覆盖整组事件后 DS 才提交入包并销毁掉落物。**认领组不拆批**
  (一次拾取拆出的多条事件同批发送同批终结,杜绝"部分入账"中间态);认领回执恰好一次:
  确认 → 入包销毁;确定未应用(未发送即被丢弃 / 整批 ErrInvalidArg-ErrUnauthorized 拒收 /
  通道停流)→ 释放认领回地面。停流后释放的物品重拾走回退路径仅入战斗背包不再产生持久化
  事实,与水位抑制协同保证**恰好一次发放**(任一侧成功即归原认领玩家)。
- **收尾**:RequestFinishBattle 前先 flush 缓冲(有界等待,不阻塞结算主流程);
  `ReportResult` 携带 `final_progress_seq` 供对账(§5;门控语义下缺口是纯审计信号,
  不再意味着已得物品丢失)。

## 4. 服务端设计

### 4.1 battle_result:新增 `ReportProgress`(DS 回调)

- 鉴权 / 授权完全复用 `ReportResult` 的 DS 回调链(Guard + Redis active match 校验 +
  roster 内 player_id 校验);非 active 对局 / 非 roster 玩家一律拒。
- 职责:水位去重 → 事实换算(怪物表 → 经验值;拾取 → 掉落白名单过滤)→ 写 progress 出箱。
- 新表:`battle_progress_stream`(水位)、`battle_progress_outbox`(出箱:目标服务、payload、
  幂等键;发布纪律同 `battle_drop_outbox`)。
- 配置:怪物经验表(monster_config_id → exp),走 §9.15 配置表热更管线;
  复用现有 `drop_whitelist`。
- 反作弊上限(§9.6 缓解,配置驱动):单场单玩家击杀计数上限 / 经验上限 / 掉落件数上限,
  超限拒收 + 告警;`pkg/killswitch` 一键关闭整个通道(回退局后结算)。

### 4.2 player:经验存储与等级结算

- `players` 表加 `exp` 列(级内经验;`level` 列已存在)。
- 新 RPC `AddExperience`(**内部直连,不经 Envoy 暴露**,带玩家 JWT 的调用一律拒 —— 同
  GrantItems 惯例;内网调用方身份鉴权与 GrantInstances 同项目级"内网可信 + Envoy 边界"
  约定,不单独加服务级授权):幂等入账 → 按等级经验表循环进位(天然支持连升多级)→
  Lv.15 封顶,满级后 delta no-op(不累加、不发事件)→ 同一事务写推送出箱。
- 等级经验配置表:与客户端 `j_玩家等级经验.xlsx` / `CfgPlayerLevelExp` **同源导出**
  (Lv15 行 UpgradeExp=0),服务端产物为 `configtable/dist/player_level_exp.json`。player 启动前
  强制加载并校验等级连续、`ID == level`、累计经验与末级唯一终点;YAML `exp_curve` 已删除,
  不再维护第二份曲线。`experience_enabled` 仅是功能放行开关,不是数值源。
  **曲线变更纪律(2026-07-22 修订)**:曲线是入账即生效的不可逆持久数值;
  `configtable.Store` 的原子切换只保证**单进程**请求读取一致,当前单地址 reload 工具不提供
  多副本同时切换。正式数值确认前生产保持 `experience_enabled=false`;以后改曲线必须先关闭
  入账入口并完成全 fleet 版本门禁/滚动收敛,禁止不同曲线副本同时接写。
- 幂等键留存:复用属性点授予的 `uk(player_id, idempotency_key)` 表模式;
  留存期配置化(默认 ≥7 天,覆盖出箱最长重试窗)后台清理。
- 升级联动钩子:升级授予属性点 / 天赋点不在本次需求,但结算点天然是
  `GrantAttributePoints/GrantTalentPoints(key="levelup:{player}:{level}")` 的挂点,后续接入零改造。

### 4.3 推送(独立 topic pandora.player.experience,2026-07-21 审计修订)

**不复用 pandora.player.update**:player 旧副本消费该 topic 做幂等 UpdateMMR 时不看
event_type header,Stable/Canary 混跑窗口里经验事件会被按 `PlayerUpdateEvent` 误解码
(字段 2/3 恰好对上 match_id/mmr_delta),静默污染 MMR(审计 P0;§9.21 事件双向兼容)。
经验事件走独立 topic `pandora.player.experience`(key=player_id,push 订阅透传;
player 旧副本不订阅,混跑零风险)。规则沉淀:**player.update 永远单事件类型,
player 域新增推送事件一律开新 topic**(见 pkg/kafkax/topics.go)。

event_type 枚举保留(客户端按 (topic, event_type) 选型;0 永远 = 旧 `PlayerUpdateEvent`):

```proto
enum PlayerPushEventType {
  PLAYER_PUSH_EVENT_TYPE_LEGACY_UPDATE = 0;  // 既有 PlayerUpdateEvent(MMR)
  PLAYER_PUSH_EVENT_TYPE_EXPERIENCE    = 1;  // PlayerExperienceEvent
}

message PlayerExperienceEvent {
  uint64 player_id     = 1;
  int32  level         = 2;  // 权威当前等级
  int64  exp_in_level  = 3;  // 级内经验(客户端用 CfgPlayerLevelExp 补 RequiredExp)
  bool   is_max_level  = 4;  // 满级 → UI 显示 MAX 满条
  uint32 levels_gained = 5;  // 本次入账升了几级;>0 客户端播升级表现(连升只播终级一次)
  int64  ts_ms         = 6;
}
```

push 服务只需把 `pandora.player.experience` 加进订阅列表(消费侧通用透传,零逻辑改动)。
客户端适配层按"(level, 累计经验) 单调不回退"处理乱序 / 补推重放:落后于当前展示快照的
事件直接丢弃;升级表现按事件触发,重放去重由 ts_ms 兜底。事件是全量权威快照 + 客户端
单调去重,因此 player 推送出箱多副本发布无需 claim/fencing(重复投递无害)。

### 4.4 单点入口清单(经验来源)

| 来源 | 调用方 | 幂等键 |
|---|---|---|
| 击杀怪物 | battle_result progress 出箱 worker | `progress:{match_id}:{seq}` |
| 完成任务 | 任务完成判定点(现有 ClaimReward 奖励含经验档 / 未来 campaign) | `quest:{player_id}:{quest_id}`(或活动实例维度) |
| 运营 / GM 补发 | gm 通道 | `gm:{operation_id}` |

## 5. 单一权威路径与结算对账

- **发放二选一,服务端强制**:结算落库时若该 match 存在 `battle_progress_stream` 行
  (= 本场走了实时通道),`ReportResult` 里的 `dropped_item_config_ids` **只作对账,不再发放**;
  反之(旧版 DS / killswitch 关闭)走既有 `battle_drop_outbox` 路径。判定依据是服务端自己的
  水位表,不信 DS 声明 —— 恶意 DS 两头报也只会发一次。
- **对账**:`ReportResult.final_progress_seq` vs `last_applied_seq`;有缺口打告警指标
  (缺口 = DS 崩溃 / 网络丢失的尾窗事件,不自动补;2026-07-21 拾取 ACK 门控后,
  缺口对应的物品从未入包,降级为纯审计信号,见 §9)。
  `PlayerStats` 可选带聚合审计字段(怪物击杀计数),仅入库审计,不参与发放。
- **ABANDONED 语义修订**:DS 崩溃 → 段位照旧回滚 / mmr_delta=0,**但已入账的经验与掉落不回滚**
  ("打到即所得"是本设计的目的;经验 / PvE 掉落非对抗性计分,不构成 §9.4 补偿语义破坏)。
- **金丝雀 / 新旧共存**(§9.21,2026-07-21 审计修订):一场对局固定一台 DS,新 DS 流式
  上报、旧 DS 走结算路径,服务端按水位表自动分流。**battle_result 自身的混版窗口必须用
  `progress_enabled`(缺省 false)闸住**:旧代码副本结算不感知水位表、不抑制结算掉落,
  若通道在混版窗口开着,同场"实时已发 + 旧副本结算再发"= 双发(P0)。发布顺序:迁移全绿
  → 全 fleet 升级 → 置 true 滚动下发。killswitch 关闭只拒新对局开流,**已有水位的对局
  继续收流到结算**(每场模式以水位行固化,防"半实时"丢奖);结算幂等重放分支同样收口
  水位(旧副本首笔落库后,新副本重试会补打终局标记,封死迟到进度)。

## 6. proto 改动清单(`[proto]`,同步 UE 仓库)

1. `battle/v1/battle.proto`:
   - 新增 `ReportProgress` RPC + `BattleProgressEvent{seq, player_id, oneof{MonsterKillFact{monster_config_id,count}, ItemPickupFact{item_config_id,count}}, ts_ms}` +
     `ReportProgressRequest{match_id, events[]}` / `ReportProgressResponse{code, acked_seq}`。
   - `ReportResultRequest` 加 `final_progress_seq`;`PlayerStats` 可选加审计聚合字段。
2. `player/v1/player.proto`:
   - `PlayerProfile` 加 `exp_in_level = 50` / `is_max_level = 51`(2026-07-21 审计修订:
     不收窄 12-49 预留段 —— 收窄会触发 buf breaking RESERVED 门禁,50/51 完全等效且门禁绿);
   - 新增 `AddExperience` RPC(内部);新增 `PlayerPushEventType` + `PlayerExperienceEvent`。
3. 掉落广播**不进 proto**:UE NetDriver ClientRPC(§0.1 表现层),不碰 Go。

## 7. UE 侧分工(用户编译验证)

**Battle DS**
- 进度上报器(有界缓冲 + 批内聚合 + 单飞行批 + 退避重试 + 结算前 flush;挂在既有
  DS 后端调用通道上,独立于 tick);怪物死亡 / 拾取权威点产事件。
  2026-07-21 MMO 拍板追加:拾取认领登记(seq 组 → 回执委托)、入队即发、认领组不拆批、
  按 `acked_seq` 前缀提交 / 确定未应用释放(`PandoraBattleProgressReporter`)。
- 拾取权威点 ACK 门控(`AMyDropItemActor` + `APandoraBattleGameMode`):背包容量**预留**
  (`UMyBagComponent::ReserveSpace`,reserve-then-commit——预留在单写者游戏线程内原子,
  活跃预留计入一切容量判定,消"预检通过→回执时被并发写源挤满"的 TOCTOU,§22 禁先查再存)
  → 认领锁定(暂停 LifeSpan,他人不可拾)→ 上报 → 回执确认后预留转正入包
  (`CommitReservation`,必成)+ 结算审计快照 + 高品质播报 → 销毁;
  释放(`ReleaseReservation`)则恢复 LifeSpan 留在地面。预留数学与恰好一次回归测试:
  Pandora-Client `PandoraTests/MyBagReservationTest.cpp`。
  回退路径(通道停流 / 本地联调无 player_id / 死亡掉落再分配 persist=false)保持旧
  "立即入包 + NotifyItemPickedUp"行为,服务端水位强制二选一防双发。
  **经济模式开关**:`UPandoraBattleSettlementRule::AreDropsPersistent()`(默认 true)。
  未来 MOBA 类玩法 override false ⇒ 掉落随局清零:拾取即入战斗背包、零后端交互、
  不产生任何持久化事实——门控是"跨局持久"这一经济语义的实现,不是拾取链的固定形态,
  玩法回摆 MOBA 无需改拾取链。
- 掉落广播:拾取权威点判 `CfgItem.Quality ≥ 金` → 同队在场玩家 ClientRPC
  (载荷 `player_id + item_config_id + count`,显示名 / 文案 / 颜色全客户端本地解析)。
- 击杀本地表现(飘字等)照旧 GameplayCue,与上报解耦。

**客户端**
- push 适配:按 PushFrame `(topic="pandora.player.experience", event_type=1)` 选型
  → `SetExperienceDisplay`(RequiredExp 查 `CfgPlayerLevelExp`),
  `levels_gained>0` → `PlayLevelUpPresentation`;单调不回退去重。
- 登录 / 重连(RecoveryCoordinator 既有时机):`GetProfile` 刷权威快照。
- 广播适配:接 DS ClientRPC → 本地化 / 显示名(PlayerState)/ 品质颜色 → `EnqueuePublicBroadcast`。

## 8. 验收矩阵(§9.16 / §9.19 纪律)

1. 幂等:同批重发 / 跨批重叠 seq / 出箱 worker 重试 → 经验与掉落只入账一次。
2. 连升多级:一次大额经验跨 3 级 → level 正确、事件 levels_gained=3、客户端只播一次终级表现。
3. 封顶:Lv15 后加经验 no-op,不发事件;`GetProfile` is_max=true → UI MAX。
4. Go 侧故障:battle_result / kafka / player 任一不可用 → 战斗零影响,DS 缓冲重试,恢复后补入账。
5. DS 崩溃:已 ACK 事件全部保住;未上报尾窗丢失有告警;ABANDONED 不回滚经验 / 掉落;段位补偿照旧。
6. 双路径防重:新 DS 流式 + 结算重放 `dropped_item_config_ids` → 不双发;旧 DS 无水位 → 结算路径正常发。
7. 断线 / 重连:经验推送离线补推 5min 内到达;超窗登录快照覆盖;乱序 / 重放不回退经验条。
8. 广播:同队即时可见、敌队不可见、紫及以下不广播、多条串行播放;掉落瞬间断线的队友收不到(接受,瞬时表现)。
9. killswitch:关闭通道后新对局回退局后结算路径,进行中对局不受影响。

## 9. 残余风险(明示;2026-07-21 MMO 拍板后改写)

- **尾窗丢失(已消除,原"明示接受"条目作废)**:拾取改 ACK 门控后,入包 = 已持久化,
  DS 任意时刻崩溃都不存在"玩家已见入包但未持久化"的物品;未确认认领的物品从玩家视角
  从未捡到(仍在地面),对局 ABANDONED 后随对局消失,与"地上没人捡的掉落"同语义,
  不构成已得物品丢失。`final_progress_seq` 对账缺口降级为纯审计信号。
- **缓冲溢出**:满载时丢最老未发认领组并**释放认领**(物品回地面可重拾,不丢已得物品);
  上限默认 4096,配置可调。
- **认领悬置窗口**:通道长时间不可用时,已认领(已发送未确认)的掉落物保持锁定直至恢复
  或终局——不可被他人拾取(防"迟到应用 + 重拾"复制),战斗本身不受影响。
- **入账上限触顶的封顶语义**(2026-07-22 审计二次对齐实现):seq 单场硬上限 / 单场累计 /
  单玩家累计是**额度用尽**语义,不是"整场关闸"——**含超限额度的那批**被 ErrInvalidArg
  拒收并告警(progress_cap_rejected;proto 契约 = DS 丢批告警继续,不是 ErrInvalidState
  停流);后续**不含超限额度**的批(如 items 触顶后的纯 exp 批、别的玩家的批)仍正常
  入账。被拒拾取按 ACK 门控释放认领走战斗背包回退。水位行已存在 ⇒ 结算掉落仍被抑制,
  超限部分不入账——这是反作弊封顶的预期行为,上限配置(缺省 100000 / 500)必须
  显著高于合法玩法单场产出。
- **未知事实停流(混版违纪,2026-07-22 审计明示)**:新 DS 事实类型早于 battle_result
  全 fleet 升级(违反 Go 先行纪律)→ ErrInvalidState 停流,并**持久化停流标记**
  (stopped_at_ms,000008;后续任何批含只含已知事实的批一律拒;标记写失败返
  ErrUnavailable 原批重试直到落库;水位 CAS 带 stopped=0 条件,停流与在途正常批无竞态)。
  后果与封顶停流同语义:已 ACK 部分保持有效;停流后拾取释放认领走战斗背包回退,
  **水位>0** 时结算掉落保持抑制,本场剩余实时奖励**永久丢失,无结算兜底**
  (progress_unknown_fact_stream_stopped 错误日志告警留证)。**首批即停流(水位=0,
  零实时入账)无抑制**,结算路径正常发放全部产出(零双发面,丢失语义不适用)。
  通道关闭拒开流同样落标记(固化本场 legacy 模式,中途重开配置不得晚开流)。
  不为违纪场景做兜底,发布纪律 Go 先行是唯一防线(proto ReportProgress 注释与
  biz/progress.go 同步维护本语义)。
- **门控入包表现损耗**:认领玩家在回执到达前掉线 / 死亡 / 背包被并发塞满时,发放不回滚
  (物归认领玩家,局后持久背包可见),仅损失局内战斗背包表现;掉落物一律销毁防重拾双发。
- **表同源纪律**:等级经验表 / 怪物经验表 / 品质字段依赖导表流水线保证 Go 与 UE 一致,
  漂移会造成显示与权威不符(不会造成入账错误 —— 权威只在 Go)。

## 10. 交接清单

- proto 生成 / cpp pb 同步:Codex(`tools/scripts/proto_gen.ps1`,commit 标 `[proto]`)。
- battle_result / player 若新增依赖:`go mod tidy` 由 Codex(AGENTS.md §11.1);
  预计涉及 services/battle/battle_result、services/account/player。
- UE 编译 / 联调:用户本人(Live Coding 占用,AI 不代跑)。
- 拍板后:§1 修订稿合入 ds-arch.md §0.5/§0.6;决策登记 pandora-arch.md §11;
  `j_玩家等级经验.xlsx` + 怪物经验列导出到服务端配置表管线。
