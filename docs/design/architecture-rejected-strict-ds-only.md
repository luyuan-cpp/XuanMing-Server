# Pandora 架构决策记录:为什么不让 Hub DS 兼任业务网关

> **状态**:已决策(2026-06-03)
> **决策**:**不采用** "Hub DS 兼任业务网关" 方案
> **采用**:业务请求走独立通道(具体方案:见 `gateway-decision.md` — 待写)
> **作用**:本文档保留作为反面教材,供未来 AI / 新开发者理解为什么没选这条路

## 一、被否决的方案("严格 A":客户端只连 DS)

### 1.1 方案描述

让客户端只有**一条** UE NetDriver 长连接,所有通信都通过这条连接到 Hub DS / Battle DS:

```
                ┌────────────────────────────────┐
                │  Client(UE)                   │
                │   只有 1 条 NetDriver 连接     │
                └────────────┬───────────────────┘
                             │
                ┌────────────▼───────────────────┐
   登录前 →     │  AnyHubDS(任意一个 Hub DS)    │
   连任意 hub   │  - 玩家未鉴权 → 走"登录流程"   │
   DS 走登录    │    Hub DS 调 login(gRPC)取   │
                │    票据 + 决定玩家最终去哪个   │
                │    Hub                         │
                │  - 玩家已鉴权 → 走"业务流程"   │
                │    所有业务请求走 ServerRPC,   │
                │    Hub DS 转 gRPC 调 go 服务   │
                └─┬──────────────────────────────┘
                  │
                  │ Hub DS 内部
                  │ 1. gRPC client × 13(连各 go 服务)
                  │ 2. kafka consumer(订阅推送 topic)
                  │ 3. ServerRPC 业务路由
                  │ 4. ClientRPC 推送下发
                  │
    ┌─────────────┼──────────────┬──────────────┬───────────────┐
    ▼             ▼              ▼              ▼               ▼
 login(go)    team(go)    matchmaker(go)   chat(go)    ... 13 个
    ▲             ▲              ▲              ▲               ▲
    │             │              │              │               │
    └─────────────┴──────────────┴──────────────┴───────────────┘
                          │
                          │ 异步推送 produce
                          ▼
                  ┌──────────────────┐
                  │ kafka topics     │
                  │  pandora.team.*  │
                  │  pandora.match.* │
                  │  pandora.chat.*  │
                  │  pandora.player.*│
                  │  pandora.system.*│
                  └──────────────────┘
                          ▲
                          │ Hub DS 消费
                          │
                   (上面那个 Hub DS)
```

### 1.2 关键性质

- Client 只连 DS(NetDriver),**没有任何 HTTP / WebSocket / gRPC 直接到 go**
- 进入战斗:Hub DS 把 battle ticket 发给客户端(ClientRPC),客户端断开 Hub DS 重连 Battle DS
- 推送:全部 kafka → Hub DS 消费 → ClientRPC

## 二、为什么否决:6 个不可接受的后果

### 后果 1:Hub DS 必须承担网关全部职责

Hub DS 实际上变成了"网关 + 大厅同步"二合一:

1. 接收所有客户端业务请求(组队/商店/匹配/聊天/交易/好友/对话/段位查询/...)
2. 转发到对应 go 服务(gRPC client 维护 13 条连接)
3. 消费 kafka(订阅 5~6 个推送 topic)
4. 把推送转成 ClientRPC

**这是大厂(腾讯王者、Riot LoL、Epic Fortnite、Roblox)都不做的方式**。所有大厂都用独立网关 / 独立业务通道,没有把网关功能塞进 DS 的先例。

### 后果 2:Hub DS 崩了 → 所有功能挂掉

Hub DS 一旦崩溃或重启,以下功能**全部不可用**:
- 登录(因为登录入口也是 DS)
- 商店浏览 / 购买
- 好友 / 邮件 / 战绩查询
- 组队 / 邀请
- 聊天

**没有任何独立通道兜底**。玩家会感受到 "整个游戏挂了",而不是 "大厅同步暂时不可用,但其它功能还能用"。

### 后果 3:Hub DS 重启 = 玩家所有功能不可用

热更新场景:

| 场景 | 严格 A 影响 | 独立网关影响 |
|---|---|---|
| 业务方(team/chat)改协议重启 | DS 不重启,业务挂掉(因为玩家通过 DS 调业务,DS 的 gRPC client 要重连)| ✅ 业务服重启,网关自动重连,玩家无感 |
| Hub DS 改 GAS / 地图 / Replication 重启 | **玩家整个游戏挂了**(包括战绩查询、商店、聊天)| ✅ 玩家断 DS,但业务网关那条连接还在,仍可用业务功能 |
| 战斗服(Battle DS)改技能数值重启 | 不影响大厅 | 不影响大厅 |

**大厂用独立网关的核心好处之一就是 DS 重启不影响业务 UI**。严格 A 放弃这个好处。

### 后果 4:Hub DS 的代码量翻 2~3 倍

UE C++ 必须额外实现:
- **gRPC client × 13**(连 13 个 go 服务)— 需要在 UE 内嵌 grpc-cpp 或装 UE gRPC 插件
- **kafka consumer**(订阅 5+ topics)— 需要在 UE 内嵌 librdkafka
- **ServerRPC 路由层**(把客户端 ServerRPC 分发到 13 个 go 服务的 gRPC 调用)
- **ClientRPC 推送层**(把 kafka 事件转成给玩家的 ClientRPC)
- **业务网关层**(限流 / 鉴权 / 错误码转换 / 重试)

**UE 是游戏引擎不是网关框架**,写这些是**逆 UE 之道**。能写,但每行代码都跟 UE 设计哲学拧着干。

D5-D6 的工作量从 "UE DS 骨架 + Agones 接入"(原计划 2 周)膨胀到 "UE DS 骨架 + Agones + 业务网关层 + kafka 消费 + gRPC 客户端池"(估 4~5 周)。

### 后果 5:500 人 Hub PvP 性能预算被打破

之前的 500 人 PvP 性能预算只考虑了 Replication / GAS。
严格 A 下,Hub DS 还要扛:

- **500 玩家 × 1~10 业务请求/秒 ≈ 5000 RPS** gRPC 客户端调用
- **5+ kafka topic 持续消费**
- **推送 fanout 到 500 玩家的 ClientRPC**

这些都是 UE tick 之外的额外开销。**500 人目标可能要打折到 300 人甚至更少**。

考虑到 Pandora 项目最重要的卖点之一就是 "500 人大厅自由 PvP",这条后果是**致命的**。

### 后果 6:登录入口的死锁问题

严格 A 下,Client 启动时:
- 想连 Hub DS,但**不知道 DS 地址**(还没登录)
- 死锁

唯一解法:用 DNS / 配置文件硬编码一个 "登录入口 DS"(如 `login.pandora.com:7777`)。
但这意味着:
- 必须有一台 / 一组 **专门做登录的 Hub DS**(LoginHub)
- 玩家登录完后被 LoginHub "重定向" 到正确的 Hub DS,Client 断开重连
- LoginHub 是单点故障(虽然可以做主备 / 多机房)
- LoginHub 容量决定了登录峰值上限

## 三、为什么"严格 A"看起来诱人但实际不可行

严格 A 看起来有这些好处:

1. **Client 极简**:UE 项目只用一个 NetDriver,无 gRPC / WebSocket / HTTPS 库,二进制小
2. **协议统一**:所有客户端通信都是 UE NetDriver,没有跨协议 marshal/unmarshal
3. **鉴权简化**:JWT 票据校验只在 DS 入口做一次,后续都在受信网络内
4. **客户端开发体验好**:UE 蓝图玩家只要会 Replicated / RPC,不用学 HTTP / WebSocket

但这些好处**都不足以抵消上面 6 个后果**,尤其是:
- 后果 2 + 3:故障域过大,Hub DS 崩 = 全游戏挂
- 后果 5:500 人目标被打破(项目核心卖点)
- 后果 4:UE C++ 代码量爆炸,单人开发节奏崩溃

## 四、行业参考:大厂都怎么做

经过对比 5 家头部产品,**没有任何一家**让 "客户端只走一条连接到 DS,业务请求也全走这条":

| 厂家 | 大厅 | 业务请求(组队/商店/好友) | 战斗 |
|---|---|---|---|
| 王者荣耀(腾讯) | 大区接入网关(C++)| → 网关 | → 战斗服 |
| LoL / Wild Rift(拳头)| HTTPS REST | → REST 后端 | → 战斗服 |
| Apex / Fortnite(Epic)| REST + WS | → REST 后端 | → 战斗服 |
| Fortnite Hub Island | UE Hub DS | **→ REST 后端(独立通道)** | → 战斗服 |
| Roblox | DS | **→ HTTP 后端(独立通道)** | (同 DS 或新 DS) |

**Fortnite Hub Island** 跟 Pandora 设计最接近(大厅是 UE Dedicated Server),它的做法是:
- 玩家在 Hub 时跟 UE DS 长连接(游戏内同步走 DS)
- **业务请求(商店 / 任务列表)走独立 HTTPS 到后端**
- DS 只管同步,不兼任业务网关

## 五、Pandora 决策:走独立业务通道(类大厂方案)

参考大厂方案,Pandora 的最终架构是:

```
Client(UE)
  ├── UE NetDriver ────────→ Hub DS / Battle DS    ← 仅游戏内同步
  │                                                  (移动/技能/HP/buff/AOI/Replication/GAS)
  ├── gRPC unary ──────────→ login(go)             ← 登录瞬间
  └── 其它业务通道(WebSocket / gRPC / REST,见 gateway-decision.md)
```

具体业务通道选型(WebSocket gateway / 客户端直连各业务 / push 服务专用通道)在另一份文档 `gateway-decision.md` 详述。

**核心不变量**:
- **Client ↔ DS 的游戏内同步**:UE NetDriver 直连,中间无任何跳转(GAS / Replication 标准用法)
- **Client ↔ 业务**:独立通道,不经过 DS
- **Hub DS 崩 ≠ 业务挂**(故障域隔离)

## 六、本文档地位

- **不是当前架构方案**(当前方案见 `pandora-arch.md` 和 `gateway-decision.md`)
- **保留作为反面教材**,防止下一会话 AI 或新人重新提出 "客户端只连 DS" 这种看似简洁的方案
- **决策行**:已写入 `pandora-arch.md` §11

任何 AI 想重新讨论 "客户端只连 DS / Hub DS 兼任网关" 之前,**必须先读完本文档 6 个后果**。
