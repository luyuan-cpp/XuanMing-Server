// Package kafkax — push 推送 topic 常量(W3 ④,2026-06-05)。
//
// 集中在一处定义 6 个推送 topic 名,push 服务消费侧 + 未来业务服 producer 共享,
// 防止字符串拼写漂移。每个 topic 对应一个 proto Event message(payload bytes 反序列化)。
//
// 不变量(docs/design/protocol-ordering-rules.md):
//   - 玩家相关 topic 的 kafka key = strconv.FormatUint(player_id, 10),保证同玩家事件保序
//   - system.notify / chat.world 是广播类,key 可空(由 push 服务 Broadcast 处理)
//   - 业务 producer 必须用 PushToPlayers helper 排除 caller_player_id(原则 2)
//
// W3 ④ 仅订阅 proto 已就绪的 3 个(team.update / match.progress / chat.private);
// 其余 3 个(player.update / friend.event / system.notify)等对应业务服上线时补
// Event message + 把 topic 名加进 etc/push-dev.yaml。
package kafkax

// Push topic 名常量。
//
// 命名规则:`pandora.<domain>.<event_kind>`(小写 + 点分,跟 mysql/redis key 一致)。
const (
	// TopicTeamUpdate — proto: pandora.team.v1.TeamUpdateEvent
	// key=player_id;原则 2:不发给发起方
	TopicTeamUpdate = "pandora.team.update"

	// TopicMatchProgress — proto: pandora.match.v1.MatchProgressEvent
	// key=player_id;**原则 3 例外**:stage 异步变化必须发给所有人(含发起方)
	TopicMatchProgress = "pandora.match.progress"

	// TopicChatWorld — proto: pandora.chat.v1.ChatPushEvent
	// 全服广播(key 暂留空,push 服务侧 Broadcast 路由,W3 ④ 暂不订阅)
	TopicChatWorld = "pandora.chat.world"

	// TopicChatTeam — proto: pandora.chat.v1.ChatPushEvent
	// key=player_id;原则 2:只发收件方
	TopicChatTeam = "pandora.chat.team"

	// TopicChatPrivate — proto: pandora.chat.v1.ChatPushEvent
	// key=player_id;原则 2:只发接收方
	TopicChatPrivate = "pandora.chat.private"

	// TopicPlayerUpdate — proto: pandora.player.v1.PlayerUpdateEvent(W3+ 补)
	// key=player_id;玩家档案变更通知(MMR/昵称/英雄池)
	TopicPlayerUpdate = "pandora.player.update"

	// TopicFriendEvent — proto: pandora.friend.v1.FriendEventEvent(W3+ 补)
	// key=player_id;原则 2:发给接收方
	TopicFriendEvent = "pandora.friend.event"

	// TopicSystemNotify — proto: pandora.system.v1.SystemNotifyEvent(W3+ 补)
	// 广播类(key 可空);系统公告 / 邮件红点 / 运营推送
	TopicSystemNotify = "pandora.system.notify"
)

// 非推送 topic(服务间事件,push 不订阅;W4 ③,2026-06-06)。
//
// 这两个不是给客户端推送用的,而是后端服务间异步事件:
//   - battle.result:战斗 DS 上报结算 → battle_result 幂等落库(key=match_id,不变量 §9)
//   - ds.lifecycle:ds_allocator 发 DS 生命周期事件(W4 ③ 仅 abandoned)→ battle_result 补偿
const (
	// TopicBattleResult — proto: pandora.battle.v1.BattleResult
	// key=match_id;at-least-once,消费者(battle_result)幂等落库(不变量 §2)
	TopicBattleResult = "pandora.battle.result"

	// TopicDSLifecycle — proto: pandora.ds.v1.DSLifecycleEvent
	// key=match_id;W4 ③ ds_allocator 心跳超时发 ABANDONED → battle_result 写补偿记录(不变量 §4)
	TopicDSLifecycle = "pandora.ds.lifecycle"
)

// PushTopics 是 push 服务默认订阅的 topic 集合(W3 ④ 启用的 3 个)。
//
// 后续 player.update / friend.event / system.notify Event message 落地后,
// 在对应业务服 PR 里把常量加进本切片,push etc yaml 同步加 topics。
var PushTopics = []string{
	TopicTeamUpdate,
	TopicMatchProgress,
	TopicChatPrivate,
}
