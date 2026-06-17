// Package errcode 定义 Pandora 全局错误码与封装。
//
// 错误码段规划(docs/design/proto-design.md §4 / infra.md):
//
//	0           = OK
//	1-999       = 公共错(网络、超时、参数、权限)
//	1000-1999   = login
//	2000-2999   = player
//	3000-3999   = team
//	4000-4999   = match
//	5000-5999   = ds_allocator
//	6000-6999   = battle_result
//	7000-7999   = trade
//	8000-8999   = dialogue
//	9000-9999   = chat / friend / locator
//	10000-10999 = data_service
//	11000+      = 预留
//
// 字段编号永不复用,只 deprecate。
package errcode

import (
	"fmt"
)

// Code 是 Pandora 错误码的强类型。0 表示成功。
type Code int32

// 公共错(1-999)
const (
	OK Code = 0

	ErrUnknown         Code = 1
	ErrInternal        Code = 2
	ErrTimeout         Code = 3
	ErrInvalidArg      Code = 4
	ErrNotFound        Code = 5
	ErrAlreadyExists   Code = 6
	ErrPermissionDeny  Code = 7
	ErrUnauthorized    Code = 8
	ErrRateLimited     Code = 9
	ErrUnavailable     Code = 10
	ErrCanceled        Code = 11
	ErrInvalidState    Code = 12
	ErrServiceDisabled Code = 13 // RPC 被 Kill-Switch 临时关停(维护中,稍后可重试)
)

// login(1000-1999)
const (
	ErrLoginAccountNotFound  Code = 1001
	ErrLoginPasswordMismatch Code = 1002
	ErrLoginDeviceBanned     Code = 1003
	ErrLoginAccountBanned    Code = 1004
	ErrLoginTooManyDevices   Code = 1005
	ErrLoginTicketExpired    Code = 1010
	ErrLoginTicketInvalid    Code = 1011
	ErrLoginTicketReplayed   Code = 1012
)

// player(2000-2999)
const (
	ErrPlayerNotFound           Code = 2001
	ErrPlayerVersionMismatch    Code = 2002 // 乐观锁冲突
	ErrPlayerNicknameTaken      Code = 2003
	ErrPlayerHeroLocked         Code = 2010
	ErrPlayerHeroAlreadyOwn     Code = 2011
	ErrPlayerFeatureDisabled    Code = 2020 // 出战养成功能未开启(feature toggle 关闭)
	ErrPlayerInsufficientPoints Code = 2021 // 属性点不足
)

// team(3000-3999)
const (
	ErrTeamNotFound      Code = 3001
	ErrTeamFull          Code = 3002
	ErrTeamNotCaptain    Code = 3003
	ErrTeamAlreadyInTeam Code = 3004
	ErrTeamInviteExpired Code = 3005
	ErrTeamWrongState    Code = 3006
	ErrTeamConcurrent    Code = 3007 // WATCH/MULTI/EXEC 乐观锁重试耗尽
)

// match(4000-4999)
const (
	ErrMatchNotFound        Code = 4001
	ErrMatchAlreadyMatching Code = 4002
	ErrMatchConfirmTimeout  Code = 4003
	ErrMatchDeclined        Code = 4004
	ErrMatchTeamNotReady    Code = 4005
	ErrMatchConcurrent      Code = 4006 // WATCH/MULTI/EXEC 乐观锁重试耗尽
)

// ds_allocator / hub_allocator(5000-5999)
const (
	ErrDSNoAvailable      Code = 5001
	ErrDSAllocationFailed Code = 5002
	ErrDSPodNotFound      Code = 5003
	ErrDSHeartbeatTimeout Code = 5004
	ErrHubNoAvailable     Code = 5101
	ErrHubTransferFailed  Code = 5102
)

// battle_result(6000-6999)
const (
	ErrBattleResultDuplicate Code = 6001 // 幂等命中,实际不算错
	ErrBattleResultDecode    Code = 6002
	ErrBattleResultDBWrite   Code = 6003
)

// trade(7000-7999)
const (
	ErrTradeOrderNotFound Code = 7001
	ErrTradeOrderExpired  Code = 7002
	ErrTradeWrongState    Code = 7003
	ErrTradeInsufficient  Code = 7004
	ErrTradeLockFailed    Code = 7005

	// inventory(背包,同属 economy 域,复用 7000 段)
	ErrInventoryItemNotFound  Code = 7010 // 道具实例不存在 / 不属于该玩家
	ErrInventoryInsufficient  Code = 7011 // 道具数量不足(扣减/出售/使用)
	ErrInventoryItemNotUsable Code = 7012 // 该道具不可在大厅使用(战斗内道具走 GAS)
	ErrInventoryNotSellable   Code = 7013 // 该道具不可出售
	ErrInventoryLockFailed    Code = 7014 // 乐观锁重试耗尽(WATCH/MULTI/EXEC 冲突)
	// ErrInventoryIdempotencyConflict 同一 idempotency_key 复用到不同请求(op/item/count/gold 指纹不一致),
	// 拒绝静默当 no-op;相同指纹的重放返回首次执行的结果快照(不变量 §9.7)。
	ErrInventoryIdempotencyConflict Code = 7015
)

// dialogue(8000-8999)
const (
	ErrDialogueNotFound      Code = 8001
	ErrDialogueOptionInvalid Code = 8002
)

// chat / friend / locator(9000-9999)
const (
	ErrChatChannelInvalid Code = 9001
	ErrChatMessageTooLong Code = 9002
	ErrChatMuted          Code = 9003

	ErrFriendNotFound     Code = 9101
	ErrFriendAlreadyAdded Code = 9102
	ErrFriendBlocked      Code = 9103
	ErrFriendLimit        Code = 9104 // 好友数已达上限(AcceptFriend 接受时原子校验)

	ErrLocatorNotFound Code = 9201
	ErrLocatorConflict Code = 9202 // 玩家同时在两个 DS

	// push 服务(W3 ④,2026-06-05)
	ErrPushOfflineCorrupted Code = 9301 // 离线 ZSET 数据损坏(反序列化失败) / offline.Append 写 redis 失败(W3 ④ 二次修复)
	// ErrPushKafkaConsumerDown 由 W4 push 健康检查 / consumer group rebalance handler 触发上报,W3 ④ 仅占位。
	ErrPushKafkaConsumerDown Code = 9302 // kafka consumer 异常下线
)

// data_service(10000-10999)
const (
	ErrDataVersionMismatch Code = 10001
	ErrDataLockTimeout     Code = 10002
	ErrDataMigrate         Code = 10003
)

// Error 是带错误码的标准错误类型。
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("errcode=%d %s", e.Code, e.Msg)
}

// New 构造一个错误。msg 可以是 fmt.Sprintf 风格。
func New(code Code, msg string, args ...any) *Error {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	return &Error{Code: code, Msg: msg}
}

// As 从 error 中提取 Code,非 *Error 返回 ErrUnknown。
func As(err error) Code {
	if err == nil {
		return OK
	}
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ErrUnknown
}

// IsRetryable 判断该错误是否值得 client 重试。
//
// 可重试:网络抖动 / 临时不可用 / 限流 / DS 还没准备好
// 不可重试:参数错 / 权限错 / 业务状态机错
func IsRetryable(code Code) bool {
	switch code {
	case ErrTimeout, ErrUnavailable, ErrRateLimited, ErrServiceDisabled,
		ErrDSAllocationFailed, ErrHubTransferFailed:
		return true
	default:
		return false
	}
}
