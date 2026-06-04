# Pandora 协议顺序规则

> **状态**:已决策(2026-06-03)
> **问题来源**:用户在 D3 末尾发现 RPC response 与 kafka push 乱序问题
> **作用**:固化 4 个协议设计原则,所有业务 proto + 客户端代码必须遵守

## 1. 乱序问题(必须先理解)

### 1.1 时序示例

```
Client → gateway(WebSocket)→ matchmaker.ConfirmMatch
                                  │
                                  │ ① 处理(几 ms)
                                  │ ② 发 kafka pandora.match.progress { stage=ALLOCATING }
                                  │ ③ 返回 gRPC response { ok }
                                  │
                          ┌───────┴────────┐
                          │                │
                          ▼                ▼
            gateway 进程内部:        kafka broker
              业务 goroutine             │
              收到 gRPC response         │ ④ 持久化 + 推到消费者(几 ms ~ 几十 ms)
              立刻 ws.Send(response)     │
                          │              ▼
                          │        gateway 进程内部:
                          │          kafka 消费 goroutine
                          │          收到消息
                          │          ws.Send(push)
                          ▼              │
                   client 收到 RESPONSE  ▼
                   (~50ms)         client 收到 PUSH
                                   (~100ms)
```

**结果**:Client 先收到 Response,后收到 Push。

### 1.2 为什么会这样

- gRPC response 走的是 **业务 goroutine → ws** 路径,几 ms 完成
- kafka push 走的是 **业务 goroutine → kafka broker → 消费 goroutine → ws** 路径,几十 ms 完成
- 这两条路径在 gateway 进程里是**两个独立 goroutine**,Go runtime 调度不保证顺序
- TCP 单连接也救不了:**gateway 把 Response 写进 ws 流的瞬间,Push 还在 kafka broker 里没出来**

### 1.3 单 WebSocket(B1)能不能解决?

**不能**。这是**应用层语义问题**,不是 TCP 顺序问题:
- TCP 保证字节流顺序 ✅
- 但 gateway **写 ws 的顺序** = "Response 先写 + Push 后写"
- 单连接只能保证"写进去什么顺序客户端就按什么顺序收"
- 不能保证"两个独立异步通道的写入操作顺序"

**B0 三连接 vs B1 单连接**:
- B0 多个 push 之间的顺序:WebSocket TCP 保证 ✅
- B1 多个 push 之间的顺序:WebSocket TCP 保证 ✅
- B0 response 与 push 之间的顺序:无保证 ❌
- B1 response 与 push 之间的顺序:无保证 ❌

**所以单 WebSocket 在乱序问题上不比三连接更优**(B1 的优势是资源、状态清晰,不是顺序)。

## 2. 真正的 bug 案例

### 2.1 案例 A:快速连点 + UI 状态机错乱

```
Client 玩家 A 快速点击 3 次:
  1. CreateTeam(队伍 1)
  2. LeaveTeam(队伍 1)
  3. CreateTeam(队伍 2)

如果 team 服务**违反原则 2**(发 push 给发起方自己):

理想顺序事件流:
  Response 1: ok, team_id=T1
  Push 1:     team.update, T1 created
  Response 2: ok
  Push 2:     team.update, T1 disbanded
  Response 3: ok, team_id=T2
  Push 3:     team.update, T2 created

实际事件流(因为 gRPC 比 kafka 快):
  Response 1: ok, team_id=T1                  ← 立刻
  Response 2: ok                              ← 立刻
  Response 3: ok, team_id=T2                  ← 立刻
  Push 1:     team.update, T1 created         ← 几十 ms 后
  Push 2:     team.update, T1 disbanded       ← 同上
  Push 3:     team.update, T2 created         ← 同上

UI 状态机:
- 收到 Resp 1:UI 显示 T1
- 收到 Resp 2:UI 清空
- 收到 Resp 3:UI 显示 T2
- 收到 Push 1:UI 又显示 T1(回退!闪烁)
- 收到 Push 2:UI 又清空(回退!闪烁)
- 收到 Push 3:UI 又显示 T2
```

**结果**:界面闪烁,过渡态可能引发其它逻辑误触发。

### 2.2 案例 B:ConfirmMatch UI 状态错乱

```
玩家点"确认参战"按钮 → 客户端发 ConfirmMatch 到 gateway
↓
matchmaker:
  - 写 redis 记录玩家已确认
  - 检查是否所有人都已确认(假设是)
  - 发 kafka pandora.match.progress { stage=ALLOCATING, key=玩家 A }
  - 返回 RPC response { ok }

如果客户端**违反原则 3**(根据 RPC response 切 UI 状态):

错误的客户端代码:
  OnConfirmMatchResponse(resp) {
    UI.ShowText("匹配成功!");  ← 错!
  }
  OnPushMatchProgress(push) {
    UI.UpdateStage(push.stage);
  }

实际表现:
  - Response 来:UI 显示"匹配成功!"(50ms)
  - Push 来:  UI 显示"正在拉起战斗服..."(100ms)← 比"成功"还后!

玩家看到:
  "确认中..." → "匹配成功!" → "正在拉起战斗服..." → "准备进入战斗..."
                ↑ 这一步是 bug,本应跳过
```

### 2.3 案例 C:第三方玩家收到错误顺序事件

```
玩家 A:Invite(B) → response ok
玩家 A:CancelInvite(B) → response ok
↓ 但 push 到 B 的延迟可能不同

B 看到:
  Push: A 邀请你
  Push: A 撤销了邀请

但如果两个 push 走不同 kafka partition(不同 key 设计错误):
  Push: A 撤销了邀请   ← B 一脸懵:"撤销什么?"
  Push: A 邀请你       ← B 看到邀请

⚠️ 这里同 key=B,kafka 保证 partition 内有序,所以这种情况**不会**发生
   除非 partition key 设计错(如用 team_id),才会乱。
```

**结论**:多个 push 之间在 kafka 同 partition 内有序,只要 key 设计正确(`key=收件人 player_id`),不会乱。

## 3. 4 个协议设计原则

### 原则 1:**Response 必须同步返回完整业务结果**

⭐ 适用范围:**立即完成**型 RPC(如 CreateTeam / GetProfile / GetMMR)

这类 RPC 的语义是"立刻办完",response 必须含完整结果,**客户端不需要等 push 才能渲染 UI**。

```proto
// ✅ 正确
service TeamService {
  rpc CreateTeam(CreateTeamReq) returns (CreateTeamResp);
}
message CreateTeamResp {
  ErrCode code = 1;
  Team    team = 2;   // ⭐ 完整 Team,客户端拿到 response 就能渲染
}

// ❌ 错误(违反原则 1)
message CreateTeamResp {
  ErrCode code    = 1;
  string  team_id = 2;   // 只返 ID,客户端要等 push 才能拿完整数据
}
```

#### 3.1 "设计 smell" 详解(为什么发起方不该收自己的 push)

"smell" 是工程术语,意思是**代码看起来不优雅,虽然能跑但暴露设计有问题**。

具体到 Pandora:**发起方既看 RPC response 又收自己的 push,意味着同一份信息走了两条路给同一个人**。

**反例**(违反原则 2):

```
玩家 A 点 CreateTeam:

✅ 干净设计
A 收 RPC response: { team: Team{id=T1, members=[A]} }
A 没收 push(因为他是发起方,他自己已经知道结果了)

❌ 设计 smell
A 收 RPC response: { team: Team{id=T1, members=[A]} }
A 同时收 push: team.update { Team{id=T1, members=[A]} }  ← 一模一样的信息

A 的客户端代码要写:
  收到 response → UI 显示 T1
  收到 push → 检查"是不是我自己刚发的请求引起的?是的话忽略,不是的话再处理"
                                            ↑ 这句话就是 smell
```

**5 条 smell 表现**:

1. **冗余**:同一份数据走两条路(response + push),浪费协议设计
2. **去重逻辑复杂**:客户端要判"这 push 是不是我自己引起的"
3. **状态机难推理**:同一事件触发两次回调,顺序还不保证
4. **流量浪费**:多发一次 kafka 消息 + 多走一次 stream 帧
5. **测试难**:要专门测"重复事件不引起 UI 错乱",回归用例多

**性能数据 vs 卫生数据**:

即使 Pandora 切到 Kratos + gRPC server stream(2026-06-04 决策),延迟差从"kafka 几十 ms"降到"stream 几 ms",**视觉上看不出闪烁了**,但这些 smell 仍存在 — 代码维护痛、测试用例膨胀、新人接手难。

**所以原则 2 是架构卫生(architectural hygiene),不是性能优化**。

**正确做法对照表**:

| RPC | response 给 caller | push 给谁 | 是否 push 给 caller |
|---|---|---|---|
| CreateTeam(单人队伍)| 完整 Team | (无第三方)| ❌ 不发 |
| Invite(A→B)| ok + invite_id | B(被邀请方)| ❌ 不发给 A |
| LeaveTeam(A 离开)| ok | 剩余队员 C/D/E | ❌ 不发给 A |
| Kick(A 踢 B)| ok | B + 剩余队员 | ❌ 不发给 A |
| StartMatch(已受理型) | match_id | 队伍所有人 | ⚠️ **例外**:发(原则 3) |
| ConfirmMatch(已受理型)| ok | 队伍所有人 | ⚠️ 同上例外 |
| SendMessage(私聊)| message_id | 接收方 B | ❌ 不发给 A |

**例外的合法性**:已受理型 RPC 的 stage 变化必须靠 push,发起方也必须收(否则他不知道 stage 已变),这个例外在原则 3 显式标注。

### 原则 2:**kafka push 不发给请求发起方,只发给"第三方玩家"**

⭐ 这是**最重要**的原则,违反它必出 bug。

| RPC | 谁会收到 push | 谁不会收到 push |
|---|---|---|
| Invite(A 邀请 B) | B(被邀请方)| A(发起方,看 response)|
| LeaveTeam(A 离开)| 其它队员 C/D/E | A(发起方,看 response)|
| ConfirmMatch(A 确认)| 其它 9 个匹配玩家 | A(发起方,看 response;但 stage 异步变化是例外,见原则 3)|
| SendMessage(A 发聊天)| 接收方 B / 频道订阅者 | A(发起方,看 response)|
| AddFriend(A 加 B)| B(接收方)| A(发起方,看 response)|

**实现要点**:
- 业务服务发 kafka 时,**循环排除 caller_player_id**
- ctx 里必须能拿到 caller_player_id(gateway 鉴权时注入)
- code review 时强制盯一下 "发 kafka 的循环里有没有排除 caller"

```go
// ✅ 正确
func (s *TeamService) Invite(ctx, req *InviteReq) (*InviteResp, error) {
    caller := ctx.Value("player_id").(int64)  // = req.captain_id 通常
    
    // ... 写 redis 记录邀请 ...
    
    // 只 push 给被邀请的 B,不 push caller
    kafka.Send("pandora.team.update", key=req.target_player_id, payload=...)
    
    return &InviteResp{Ok: true}, nil
}

// ❌ 错误
func (s *TeamService) Invite(ctx, req *InviteReq) (*InviteResp, error) {
    // 错!给所有相关玩家都发,包括 caller
    for _, p := range []int64{req.captain_id, req.target_player_id} {
        kafka.Send("pandora.team.update", key=p, payload=...)
    }
    return &InviteResp{Ok: true}, nil
}
```

### 原则 3:**异步状态机变化必须走 push,RPC response 只表示"已受理"**

⭐ 适用范围:**已受理**型 RPC(如 StartMatch / ConfirmMatch / CreateOrder)

某些业务**本质就是异步**:
- 匹配:玩家发 StartMatch 后,撮合是几秒~几分钟的过程,中间状态(QUEUEING / FOUND / CONFIRM / ALLOCATING / READY)**只能走 push**
- 战斗结算:DS 战斗完后才发 result,客户端不能"调一个 RPC 等结果"

这种 RPC 的语义是:
- **Response 只表示"已收到请求,会处理"**(不能驱动 UI 状态)
- **真正的状态变化全靠 push**(包括发起方自己也收 push)

```proto
// ✅ 正确(StartMatch 是已受理型)
rpc StartMatch(StartMatchReq) returns (StartMatchResp);
message StartMatchResp {
  ErrCode code     = 1;
  string  match_id = 2;   // 已入队,后续 stage 走 push
}
```

⚠️ **原则 3 跟原则 2 冲突的特例**:匹配进度 push **必须**给发起方自己也发,因为他没有别的方式知道 stage 变化。

**这是协议设计上的"已知例外"**,要在 proto 注释里显式标注。

### 原则 4:**Proto 注释必须显式标注 RPC 语义**

每个 RPC 必须在 proto 注释里写明是"立即完成"还是"已受理":

```proto
// CreateTeam: 立即完成(synchronous),response 含完整 Team
// kafka push 不发给发起方(他看 RPC response 即可)
rpc CreateTeam(CreateTeamReq) returns (CreateTeamResp);

// StartMatch: 已受理(accepted),后续 stage 变化走 push
// ⚠️ 例外:matchmaker 的 push 给所有队员包括发起方自己(原则 3 例外)
rpc StartMatch(StartMatchReq) returns (StartMatchResp);
```

让客户端开发者一看注释就知道:
- "立即完成" → 客户端 OnResponse 里直接切 UI 状态
- "已受理" → 客户端 OnResponse 里只显示 loading,等 push 切状态

## 4. Pandora 现有 RPC 的语义分类

### 4.1 立即完成型(原则 1)

| RPC | response 内容 | push? |
|---|---|---|
| login.Login | session_token + hub_ds_addr + hub_ticket | 不发(发起方看 response)|
| login.IssueDSTicket | ticket | 同上 |
| login.VerifyDSTicket | claims | 同上 |
| player.GetProfile | PlayerProfile | 不发 |
| player.GetMMR | mmr | 不发 |
| player.UpdateNickname | ok | 可能给好友 push |
| player.UnlockHero | ok | 不发 |
| team.CreateTeam | Team | 不发(单人队伍无第三方)|
| team.GetTeam | Team | 不发 |
| friend.ListFriends | []FriendInfo | 不发 |
| trade.ListMyOrders | []Order | 不发 |
| dialogue.StartDialogue | DialogueState | 不发 |
| chat.PullHistory | []ChatMessage | 不发 |
| ds.AllocateBattle | ds_addr + tickets | 不发(matchmaker 内部调) |
| hub.AssignHub | hub_ds_addr + ticket | 不发 |
| Heartbeat(各)| command | 不发(DS 内部用)|

### 4.2 已受理型(原则 3)

| RPC | response 内容 | push 给谁 |
|---|---|---|
| match.StartMatch | match_id | 队伍所有人(含发起方,例外)|
| match.CancelMatch | ok | 队伍所有人(含发起方,例外)|
| match.ConfirmMatch | ok | 队伍所有人(含发起方,例外)|
| trade.CreateOrder | order_id | 双方(发起方 + 对方,因状态机要 push 推进)|
| trade.ConfirmOrder | ok | 同上 |

### 4.3 涉及第三方的立即完成型(原则 2 严格执行)

| RPC | response 给发起方 | push 给第三方(不含发起方)|
|---|---|---|
| team.Invite | ok | B(被邀请方)|
| team.AcceptInvite | Team(完整队伍)| 其它队员 |
| team.LeaveTeam | ok | 剩余队员 |
| team.Kick | ok | 被踢者 + 剩余队员 |
| team.SetReady | ok | 其它队员 |
| friend.AddFriend | request_id | B(被加方)|
| friend.AcceptFriend | ok | 申请方 |
| chat.SendMessage | message_id | 接收方 / 频道订阅者 |
| dialogue.ChooseOption | DialogueState | 不发(单玩家会话)|

## 5. 客户端代码强制约定

### 5.1 OnResponse 处理规则

**立即完成型 RPC**:
```cpp
OnCreateTeamResponse(resp) {
    if (resp.code == OK) {
        UI.ShowTeam(resp.team);   // ✅ 直接渲染
    } else {
        UI.ShowError(resp.code);
    }
}
```

**已受理型 RPC**:
```cpp
OnStartMatchResponse(resp) {
    if (resp.code == OK) {
        UI.ShowText("匹配中...");   // ✅ 只显示 loading,不切状态
        currentMatchId = resp.match_id;
    } else {
        UI.ShowError(resp.code);
    }
    // ⚠️ 不要根据 response 切"匹配成功"UI
}

OnPushMatchProgress(push) {
    UI.UpdateStage(push.stage);   // ✅ stage 由 push 驱动
    if (push.stage == READY) {
        ConnectToBattleDS(push.battle_ds_addr, push.battle_ticket);
    }
}
```

### 5.2 UI 状态机原则

- **立即完成型**:Response 直接驱动 UI 状态切换
- **已受理型**:Response 只表示"已发出",UI 进入"等待"态;状态切换全靠 push
- **永远不要**:同时根据 Response 和 Push 切 UI(必出乱序 bug)

### 5.3 客户端去重(应对 at-least-once)

kafka 是 at-least-once 推送,push 可能重复。客户端按 envelope 时间戳 + ID 去重:

```cpp
OnPushReceived(envelope) {
    // 去重检查
    if (envelope.ts_ms <= lastSeenTs[envelope.topic]) {
        // 比上次看到的还旧,丢弃
        return;
    }
    lastSeenTs[envelope.topic] = envelope.ts_ms;
    
    Dispatch(envelope);
}
```

## 6. 服务端代码强制约定

### 6.1 发 kafka 时排除 caller

每个发 push 的业务服务,**强制使用以下模板**:

```go
// pkg/push/helper.go(W2 时实现)
func PushToPlayers(ctx context.Context, topic string, recipients []int64, payload proto.Message) error {
    callerID := GetCallerPlayerID(ctx)  // 从 ctx 拿,没有就是 0
    
    for _, recipientID := range recipients {
        if recipientID == callerID {
            continue   // ⭐ 强制排除 caller
        }
        kafka.Send(topic, recipientID, payload)
    }
    return nil
}
```

### 6.2 异步业务必须显式声明"我要给 caller 也 push"

如果是 match / trade 这种"已受理"型 RPC,需要给发起方也 push,**必须用单独函数**:

```go
// pkg/push/helper.go
func PushToAllIncludingCaller(ctx context.Context, topic string, recipients []int64, payload proto.Message) error {
    // ⚠️ 这个函数仅用于已受理型 RPC 的 stage 推进
    // 使用前必须确认 RPC 是"已受理"语义
    for _, recipientID := range recipients {
        kafka.Send(topic, recipientID, payload)
    }
    return nil
}
```

代码 review 强制要求:**调 PushToAllIncludingCaller 必须在注释里说明对应的 RPC 是已受理型**。

## 7. 反模式禁令

- ❌ **不要**让发起方既看 RPC response 又收自己触发的 push(原则 2)
- ❌ **不要**根据"已受理"型 RPC 的 response 切 UI 状态机(原则 3)
- ❌ **不要**省略 RPC 的语义注释(原则 4)
- ❌ **不要**让 RPC response 只返 ID 不返完整数据(立即完成型应该返完整数据)
- ❌ **不要**用 stream RPC 解决推送问题(go-zero 不支持,改 kafka push)
- ❌ **不要**指望 TCP 单连接能解决 RPC response 和 kafka push 的乱序(它们是两个 goroutine 写 ws,Go 调度不保证顺序)
- ❌ **不要**写"等 response 和 push 都到了再处理 UI"的复杂同步逻辑(直接选对一种语义即可)

## 8. 工程检查清单

### 8.1 写新 RPC 前

- [ ] 确定 RPC 语义:立即完成 / 已受理(原则 4)
- [ ] proto 注释里写明语义
- [ ] response message 是否符合原则 1(立即完成型必须返完整数据)
- [ ] 是否需要 push?发给谁?是否含 caller(原则 2)?

### 8.2 服务端代码 review

- [ ] 发 kafka 的循环里**显式排除 caller_player_id**(或调用 `PushToPlayers` 帮助函数)
- [ ] 已受理型 RPC 调用 `PushToAllIncludingCaller` 时,代码注释写明"已受理型 RPC,对应 §4.2"

### 8.3 客户端代码 review

- [ ] 立即完成型 RPC:OnResponse 直接更新 UI
- [ ] 已受理型 RPC:OnResponse 只显示 loading,UI 状态机由 push 驱动
- [ ] OnPushReceived 有时间戳去重(应对 at-least-once)

### 8.4 测试

- [ ] 单元测试:覆盖"快速连点同一 RPC"场景,验证 UI 状态收敛
- [ ] 集成测试:验证 push 到达延迟(p99 < 200ms)
- [ ] 故障注入:kafka 延迟升到 1s 时验证 UI 不乱

## 9. 与 gateway 设计的关系

`gateway-decision.md` 描述了客户端连接架构(B1 单 WebSocket / 还是 B0 三连接),**那是基础设施层**;
本文档描述协议语义层 — **乱序问题靠协议规则解决,不靠基础设施**。

无论选 B0 还是 B1,**4 个原则都必须遵守**。

## 10. 历史演化

| 日期 | 事件 |
|---|---|
| 2026-06-03 上午 | 用户问"go-zero 不支持 stream 怎么推送" |
| 2026-06-03 中午 | 走错严格 A 路线,被否决(`architecture-rejected-strict-ds-only.md`)|
| 2026-06-03 下午 | 选 B0 三连接,设计 push 服务 |
| 2026-06-03 傍晚 | 用户提出"WebSocket 双工合并",改 B1 单 WebSocket(`gateway-decision.md`)|
| 2026-06-03 晚上 | 用户提出**乱序问题** | 发现这是协议设计问题不是架构问题 |
| 2026-06-03 晚上 | **本文档落地**,固化 4 个原则 |

## 11. 决策行(写入 pandora-arch.md §11)

- 2026-06-03:RPC response 与 kafka push 乱序问题确认 = 协议设计问题(非架构问题)
- 2026-06-03:固化 4 个协议原则 — Response 完整 / 不发 push 给 caller / 已受理显式 / proto 注释标注
- 2026-06-03:服务端 `PushToPlayers` / `PushToAllIncludingCaller` helper 必须强制使用(W2 时实现)
- 2026-06-03:客户端 UI 状态机原则 — 立即完成型按 response,已受理型按 push
