// Package biz 是 friend 服务的业务逻辑层(2026-06-15)。
//
// 职责(docs/design/go-services.md §2.4):
//   - 好友请求 / 接受 / 列表 / 拉黑
//   - 好友图落 pandora_social(MySQL 强依赖,data.FriendRepo)
//   - 好友请求 / 接受经 kafka pandora.friend.event → push 推送给接收方(弱依赖)
//   - ListFriends 经 player_locator 填在线状态(弱依赖,查不到按离线)
//
// 关键规则:
//   - 不能加自己;互相拉黑则不能加好友(ErrFriendBlocked)
//   - 已是好友再加 → ErrFriendAlreadyAdded
//   - AcceptFriend 只有请求的 target 本人可接受(R5 player_id 来自 JWT ctx)
//   - 推送原则 2:好友请求发给 target,接受通知发给 requester,均不发给操作者自己
package biz

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	friendv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/friend/v1"

	"github.com/luyuancpp/pandora/services/social/friend/internal/conf"
	"github.com/luyuancpp/pandora/services/social/friend/internal/data"
)

// FriendEventPusher 把好友事件发到 kafka(main.go 注入 kafkax 适配器;弱依赖,nil 时静默跳过)。
// toPlayerID 是接收方(= evt.to_player_id),kafka key 用它(不变量 §9)。
type FriendEventPusher interface {
	PushFriendEvent(ctx context.Context, toPlayerID uint64, evt *friendv1.FriendEvent) error
}

// FriendUsecase 是 friend 服务业务逻辑核心。
type FriendUsecase struct {
	repo   data.FriendRepo
	pusher FriendEventPusher       // 弱依赖,可为 nil
	online data.OnlineStatusReader // 弱依赖,可为 nil
	cfg    conf.FriendConf
}

// NewFriendUsecase 构造。pusher / online 允许为 nil(弱依赖未配置时降级)。
func NewFriendUsecase(repo data.FriendRepo, pusher FriendEventPusher, online data.OnlineStatusReader, cfg conf.FriendConf) *FriendUsecase {
	if cfg.MaxFriends <= 0 {
		cfg.MaxFriends = 200
	}
	return &FriendUsecase{repo: repo, pusher: pusher, online: online, cfg: cfg}
}

// AddFriend 发起好友请求。requester / target 由 service 从 JWT ctx + 请求体得到。
// newRequestID 是 service 用 snowflake 预生成的请求 ID(复用历史请求时会被丢弃)。
func (u *FriendUsecase) AddFriend(ctx context.Context, requesterID, targetID, newRequestID uint64) (uint64, error) {
	if requesterID == 0 || targetID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "requester / target required")
	}
	if requesterID == targetID {
		return 0, errcode.New(errcode.ErrInvalidArg, "cannot add self as friend")
	}

	// 互相拉黑则不能加好友
	blocked, err := u.repo.IsBlocked(ctx, requesterID, targetID)
	if err != nil {
		return 0, err
	}
	if blocked {
		return 0, errcode.New(errcode.ErrFriendBlocked, "blocked between %d and %d", requesterID, targetID)
	}

	// 已是好友
	already, err := u.repo.AreFriends(ctx, requesterID, targetID)
	if err != nil {
		return 0, err
	}
	if already {
		return 0, errcode.New(errcode.ErrFriendAlreadyAdded, "already friends: %d-%d", requesterID, targetID)
	}

	// 提前失败:requester 好友已满就不用发请求(非权威校验,真正的原子上限
	// 在 AcceptRequest 事务内对双方再校一次)。
	if u.cfg.MaxFriends > 0 {
		cnt, cerr := u.repo.CountFriends(ctx, requesterID)
		if cerr != nil {
			return 0, cerr
		}
		if cnt >= u.cfg.MaxFriends {
			return 0, errcode.New(errcode.ErrFriendLimit,
				"friend limit reached: %d (max %d)", requesterID, u.cfg.MaxFriends)
		}
	}

	requestID, _, err := u.repo.CreateRequest(ctx, newRequestID, requesterID, targetID)
	if err != nil {
		return 0, err
	}

	// 推送原则 2:好友请求通知发给接收方 target
	u.pushEvent(ctx, targetID, &friendv1.FriendEvent{
		ByPlayerId: requesterID,
		ToPlayerId: targetID,
		RequestId:  requestID,
		Reason:     friendv1.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED,
		TsMs:       nowMs(),
	})
	return requestID, nil
}

// AcceptFriend 接受好友请求。player 必须是请求的 target 本人(R5)。
func (u *FriendUsecase) AcceptFriend(ctx context.Context, playerID, requestID uint64) error {
	if playerID == 0 || requestID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player / request_id required")
	}

	req, found, err := u.repo.GetRequest(ctx, requestID)
	if err != nil {
		return err
	}
	// 预检(fail-fast,非权威):请求不存在 / 不是发给本人 / 已非 pending → 直接报找不到,
	// 避免无谓开事务。真正的权威校验(target / block / 上限 / 状态)在 AcceptRequest 事务内做。
	if !found || req.TargetID != playerID || req.Status != requestStatusPending {
		return errcode.New(errcode.ErrFriendNotFound, "no acceptable request: %d", requestID)
	}

	// 权威:target 校验 + block 校验 + 上限校验 + 状态更新 + 建边,全在一个事务里(原子)。
	// accepted=false 表示预检后到事务取锁前,请求已被 Block / 另一次 accept 并发处理 →
	// 本次未真正完成 pending→accepted,绝不能推送 accepted(否则假成功,P1)。
	accepted, err := u.repo.AcceptRequest(ctx, requestID, playerID, u.cfg.MaxFriends)
	if err != nil {
		return err
	}
	if !accepted {
		return errcode.New(errcode.ErrFriendNotFound, "no acceptable request: %d", requestID)
	}

	// 推送原则 2:接受通知发给发起方 requester
	u.pushEvent(ctx, req.RequesterID, &friendv1.FriendEvent{
		ByPlayerId: playerID,
		ToPlayerId: req.RequesterID,
		RequestId:  requestID,
		Reason:     friendv1.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_ACCEPTED,
		TsMs:       nowMs(),
	})
	return nil
}

// ListFriends 列好友(客户端可见结构 FriendInfo)。
//
// nickname 留空:由客户端按 player_id 向 player 服务批量解析展示名(CLAUDE.md §5.8
// 最小数据单位,friend 服务不持有昵称真源,避免跨库 join)。
// is_online / last_seen_ms 经 locator 填充,弱依赖查不到按离线。
func (u *FriendUsecase) ListFriends(ctx context.Context, playerID uint64) ([]*friendv1.FriendInfo, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}

	rows, err := u.repo.ListFriends(ctx, playerID)
	if err != nil {
		return nil, err
	}

	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.FriendID)
	}

	var online map[uint64]data.OnlineStatus
	if u.online != nil && len(ids) > 0 {
		online = u.online.BatchOnline(ctx, ids)
	}

	infos := make([]*friendv1.FriendInfo, 0, len(rows))
	for _, r := range rows {
		fi := &friendv1.FriendInfo{
			PlayerId: r.FriendID,
			SinceMs:  r.SinceMs,
		}
		if st, ok := online[r.FriendID]; ok {
			fi.IsOnline = st.Online
			fi.LastSeenMs = st.LastSeenMs
		}
		infos = append(infos, fi)
	}
	return infos, nil
}

// Block 拉黑 target(同时删好友关系 + 取消两人之间 pending 请求,见 data.Block)。
func (u *FriendUsecase) Block(ctx context.Context, playerID, targetID uint64) error {
	if playerID == 0 || targetID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player / target required")
	}
	if playerID == targetID {
		return errcode.New(errcode.ErrInvalidArg, "cannot block self")
	}
	return u.repo.Block(ctx, playerID, targetID)
}

// pushEvent 弱依赖推送:pusher 为 nil 或发送失败只 warn,不影响主流程成功。
func (u *FriendUsecase) pushEvent(ctx context.Context, toPlayerID uint64, evt *friendv1.FriendEvent) {
	if u.pusher == nil {
		return
	}
	if err := u.pusher.PushFriendEvent(ctx, toPlayerID, evt); err != nil {
		plog.With(ctx).Warnw("msg", "friend_event_push_failed",
			"to_player_id", toPlayerID, "reason", evt.GetReason().String(), "err", err)
	}
}

// nowMs 返回当前毫秒时间戳。
func nowMs() int64 {
	return time.Now().UnixMilli()
}

// requestStatusPending 在 data 包定义为常量;biz 这里复用其语义值(1=pending)。
// 为避免跨包导出 data 内部常量,这里就地声明同值常量(与 proto FriendRequestStatus 对齐)。
const requestStatusPending = 1
