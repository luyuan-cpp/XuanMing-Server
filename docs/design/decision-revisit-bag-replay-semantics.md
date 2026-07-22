# decision-revisit:背包域重放语义 / 堆叠权威 / 存量迁移(2026-07-22 用户拍板)

> 状态:**已拍板**(2026-07-22 架构评审三轮问答后用户拍板"全部实现")。
> 覆盖对 `bag-domain.md` 初版(2026-07-21)若干已落文档语义的修订;
> 修订同步回写 bag-domain.md(§3.1/§5.2/§7/§10),本文件保留决策依据与验收标准。

## 0. 触发背景(旧问题)

架构评审(2026-07-22)确认初版设计存在四个真实缺口:

1. **整理-重放缺口**:FMyBag 整理(MergeAndCompact)合并零头释放格子属 checkpoint 类
   (不进 journal);若整理后、checkpoint 前,一笔依赖该空格的 grant 已 journal ACK,
   DS 崩溃后从旧 checkpoint 重放该 grant 会 BagFull——而初版 §3.1 写"重放遇到容量不符
   fail-closed 拒载",等于玩家资产被自己的持久化流水挡在场外(违反不变量 20)。
2. **格位寻址重放缺口**:若 journal 扣减绑定"段+格位",非 journal 的个人态操作(卸下装备
   等)回档后,已 ACK 的扣减(卸下→交易卖出)在重放时会找不到物品。
3. **服务端无限合并堆**(已另行修复,见 bag-domain.md §5.2):与格子 UI 容量语义矛盾 +
   uint32 回绕风险。
4. **FMyBag 幽灵格**:RemoveItems/DrainItemStacks 扣空留 Size=0 堆占格,污染容量判定
   与 checkpoint。

另有两项此前留白的产品决策一并拍板:邮件领取默认目标段、旧 player_items 存量迁移去向。

## 1. 拍板决策清单

### D1 重放语义:数据完整性 fail-closed 不变;容量冲突改"资产守恒 + 布局容忍"

- journal 缺口 / 截断 / 指纹不符 = 数据完整性问题,维持 INC-20260722-003 的拒载不动。
- **容量不符 = 布局回档的预期物理现象**,不再拒载:重放 grant 放不下时溢出进临时格
  (bag_type 3);临时格也满则该背包进入**超容态——只出不进**,低于容量自动恢复,
  全程告警计数。溢出量有界:最多为 checkpoint 周期内的 journal 尾部(秒级窗口)。
- 可证明的守恒不变量:随身组个人态操作(用药/穿脱/整理)**不创造资产**(资产只经
  journaled 入账进组),故回档只会让"组内可扣总量"变多——journaled 扣减必然仍可满足,
  journaled 入账必然可放置(给定溢出容忍)。重放永不因业务状态失败。

### D2 随身组 journal 组级寻址

随身组(0/2/3)的 journal op 语义一律以**随身组整体**为寻址粒度(instance_id /
config+count),不绑段与格位;重放按组解析(卸下的装备在装备栏被扣走,资产结局正确)。
后端驻留段 op 不变(段本身就是权威事务域)。契约落 bag.proto 注释(字段形状不变)。

### D3 邮件领取默认进身上背包(评审建议"进仓库"被落地事实修正)

评审阶段建议默认进仓库(链路最短);**但 phase 2 接线(2026-07-22 并行落地,PROGRESS
续9)已按 bag-domain.md §7 主形态把"进身上"的三段式链完整建成并测绿**(UE
UMyBagPersistenceComponent 预留→journal→Mark + mail 意图落库 + 托管行消除)。为避免
拆除刚建成的工作链,**默认定型为进身上背包**;"进仓库"(纯后端闭环,无预留)机制具备,
保留为产品后备形态,切换 = 改 journal entry 的 bag_type 目标 + 免预留,零新机制。
本条与初版 §7 一致,不构成翻案;记录在此只为闭合评审遗留问题。

### D4 checkout 失败按 WAIT 语义降级

LoadBag 失败不得成为无兜底的进场硬门,也不得静默卡死:有界重试 + §9.23 可见 WAIT,
短故障=进场稍慢,长故障=停在可重试 UI;不清 session。背包域故障的爆炸半径不得放大为
"全服进不了场"。

### D5 存量迁移:player_items / player_item_instance 全部迁仓库,身上背包从空开始

- **不走邮件**(邮件寿命钳到 claim 保留期,过期即丢资产,不能当迁移容器)。
- 仓库允许迁移一次性**超容落位**,随后"只出不进"(与 D1 同一套超容语义,机制只做一份;
  sectionAddItems 容量门只拦新开格,扣减/转出不受容量影响,天然满足)。
- 幂等:`bag_migration` 表(player_id PK)一玩家一行,迁移作业可断点续跑;
  行是永久幂等闸(同 leaderboard_settlement 豁免类,§9.24 登记不清理)。
- **时序(expand→migrate→contract)**:迁移作业**只能在旧写路径冻结后运行**——
  GrantItems/UseItem/SellItem/escrow 调用方全部切到 bag journal(phase 2/3)后,
  player_items/player_item_instance 静止,再批量迁移 + 总量对账(config 计数 + 实例集
  相等),最后切读路径。实现先落码,**配置门 `bag.legacy_migration_enabled` 默认关**,
  contract 阶段才开。
- **与邮件 transfer 托管链的割接互锁**:实例权威迁 bag 后,EscrowOutInstances /
  ClaimTransferInstances /ReleaseTransferEscrow 的实例源需同一割接窗口改指 bag 域;
  在途 mail_transfer_escrow 行照常(托管行独立存在,领取入包路径届时走 bag journal)。

### D6 FMyBag 幽灵格修复

RemoveItems/DrainItemStacks 扣空即删格(Items + PosToGuid 同步清理),对齐服务端
sectionRemoveItems 与 inventory deductItemTx(2026-07-22 拍板"扣空删行")语义。

### D7 整理后即时 checkpoint(优化项,非正确性依赖)

MergeAndCompact 等释放容量的操作后触发一次异步 checkpoint,缩小 D1 的溢出窗口;
正确性仍由 D1 保证,不得把即时 checkpoint 当作前置条件(那是 F3 方案,已否)。

## 2. 已否决的替代方案

| 方案 | 否决理由 |
|---|---|
| F1 全量 op-log(整理/用药/穿脱全进 journal) | FMyBag 迭代 TMap 天然非确定,重放仍可能撞容量;为确定性重写迭代序 + journal 流量翻倍,复杂度不举证(§15) |
| F3 依赖容量的写之前强制同步 checkpoint | 热路径引入同步快照;"依赖"边界无法精确判定 |
| 全部逻辑搬 Go、一人一 PB 权威 | 拾取/用药/穿脱等待 Go RTT;Go 故障整背包不可用(用户明确否决);一人一 PB 的正确用法是 checkpoint blob(现设计已是) |
| 存量迁身上背包 / 迁邮件 | 身上需合成布局 + 超容无解;邮件有寿命钳,过期丢资产 |

## 3. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 重放溢出物品进临时格,玩家可见布局变化 | 溢出量有界(秒级尾部);告警计数;UI 提示"崩溃恢复整理" |
| 超容态长期滞留(玩家不取) | 只出不进 + 低于容量自动恢复;运营可查告警指标 |
| 迁移窗口双份展示(bag 副本 vs 旧表) | 配置门默认关;只在旧写路径冻结后运行(D5 时序);现状 GetInventory 本就未加载进 FMyBag,无体验回退 |
| 迁移作业与 transfer 托管链割接错序 | D5 互锁条目;割接 checklist 进 phase 3 |
| bag_migration 行永存 | 一玩家一行,被玩家数有界;§9.24 豁免登记 + dbcheck |

## 4. 迁移成本

- Go:sectionAddItems 已按 §5.2 重写(2026-07-22 落码);本次增 bag_migration 表 +
  迁移作业(配置门默认关)。bag.proto 仅注释修订,字段形状不变(cpp pb 同步列 Codex 清单)。
- UE:幽灵格修复立即可做;重放容忍 / 组级寻址 / 整理后 checkpoint 属 phase 2 journal
  客户端(尚未存在),建骨架时按本契约实现,不产生返工。

## 5. 验收标准

1. **资产守恒 E2E 硬断言**:拾取→整理→交易→任意插桩点 kill DS→重进,config 总量 +
   实例集 = journal 账面;重放溢出走临时格,永不拒载。
2. 组级扣减重放:卸下→交易→崩溃→重放,从装备栏解析扣除成功。
3. 超容段只出不进:超容时扣减/转出照常,新开格拒;低于容量后恢复可入。
4. 幽灵格回归:扣空后格子释放,GetOccupiedGridCount 与实际占格一致。
5. 迁移:幂等(重跑不翻倍)、断点续跑、总量对账(计数 + 实例集相等)、超容落位只出不进、
   配置门默认关时零行为变化。
6. checkout 失败注入:LoadBag 超时/拒绝 → 进场 WAIT → 恢复 → 自动收敛,不清 session。
