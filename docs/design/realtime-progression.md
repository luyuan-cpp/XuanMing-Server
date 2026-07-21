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
> 4. **Go 不可用不影响战斗**:上报失败只缓冲重试,战斗照常;进度晚到不丢(尾窗风险见 §9)。
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
  计数,缓冲增长按怪物种类有界;拾取事件天然低频。硬上限(默认 4096 条)满载时立即触发一次
  发送;仍满则丢最老事件并打告警计数(残余风险入 §9)。
- **收尾**:RequestFinishBattle 前先 flush 缓冲(有界等待,不阻塞结算主流程);
  `ReportResult` 携带 `final_progress_seq` 供对账(§5)。

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
  (Lv15 行 UpgradeExp=0),走配置表热更管线;两侧漂移 = 显示 bug,导表流水线要保证一致。
  **曲线变更纪律(2026-07-21 审计)**:曲线是入账即生效的不可逆持久数值,当前副本本地
  配置无版本绑定 —— 变更必须全 fleet 同配置一起换(先关金丝雀分流),禁止不同曲线副本
  混跑;生产在正式数值确认前保持 exp_curve 为空(功能关);接入 configtable 热更管线
  (单源 + version + 原子切换)后此纪律由管线保证。
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
  (缺口 = DS 崩溃 / 网络丢失的尾窗事件,不自动补,见 §9 残余风险)。
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

## 9. 残余风险(明示)

- **尾窗丢失**:DS 崩溃时本地缓冲里未发出的事件(≈ 一个批间隔,默认 ≤1s)永久丢失,
  只告警不自动补。接受理由:补偿需要 DS 崩溃前持久化事件日志,违背"Battle DS 全内存"形态,
  成本远超 ≤1s 产出的价值。
- **缓冲溢出丢弃**:极端刷怪速率超硬上限时丢最老事件(有计数告警);上限默认 4096,配置可调。
- **表同源纪律**:等级经验表 / 怪物经验表 / 品质字段依赖导表流水线保证 Go 与 UE 一致,
  漂移会造成显示与权威不符(不会造成入账错误 —— 权威只在 Go)。

## 10. 交接清单

- proto 生成 / cpp pb 同步:Codex(`tools/scripts/proto_gen.ps1`,commit 标 `[proto]`)。
- battle_result / player 若新增依赖:`go mod tidy` 由 Codex(AGENTS.md §11.1);
  预计涉及 services/battle/battle_result、services/account/player。
- UE 编译 / 联调:用户本人(Live Coding 占用,AI 不代跑)。
- 拍板后:§1 修订稿合入 ds-arch.md §0.5/§0.6;决策登记 pandora-arch.md §11;
  `j_玩家等级经验.xlsx` + 怪物经验列导出到服务端配置表管线。
