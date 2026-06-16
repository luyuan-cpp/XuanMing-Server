// friend_test.go — FriendUsecase 业务逻辑单测(2026-06-15)。
//
// 用内存版 fakeRepo / fakePusher / fakeOnline 复刻 MySQL + kafka + locator 语义,无需真依赖。
// 覆盖:AddFriend / AcceptFriend / ListFriends / Block 正常路径 + 自加 / 拉黑 / 已是好友 / 非本人接受等错误路径。
package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	friendv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/friend/v1"
	"github.com/luyuancpp/pandora/services/social/friend/internal/conf"
	"github.com/luyuancpp/pandora/services/social/friend/internal/data"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

// fakeRepo 是 data.FriendRepo 的内存实现。
type fakeRepo struct {
	friends  map[uint64]map[uint64]int64 // player → friend → since_ms
	requests map[uint64]*data.FriendRequestRow
	blocks   map[uint64]map[uint64]bool // player → blocked → true

	// forceAcceptNotCompleted 模拟「预检后到事务取锁前请求被并发处理」:
	// AcceptRequest 返回 (false, nil),用于 P1 并发假成功回归测试。
	forceAcceptNotCompleted bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		friends:  map[uint64]map[uint64]int64{},
		requests: map[uint64]*data.FriendRequestRow{},
		blocks:   map[uint64]map[uint64]bool{},
	}
}

func (f *fakeRepo) AreFriends(_ context.Context, a, b uint64) (bool, error) {
	_, ok := f.friends[a][b]
	return ok, nil
}

func (f *fakeRepo) IsBlocked(_ context.Context, a, b uint64) (bool, error) {
	return f.blocks[a][b] || f.blocks[b][a], nil
}

func (f *fakeRepo) CountFriends(_ context.Context, playerID uint64) (int, error) {
	return len(f.friends[playerID]), nil
}

func (f *fakeRepo) findByPair(requester, target uint64) *data.FriendRequestRow {
	for _, r := range f.requests {
		if r.RequesterID == requester && r.TargetID == target {
			return r
		}
	}
	return nil
}

func (f *fakeRepo) CreateRequest(_ context.Context, newRequestID, requesterID, targetID uint64) (uint64, bool, error) {
	if existing := f.findByPair(requesterID, targetID); existing != nil {
		if existing.Status == requestStatusPending {
			return existing.RequestID, true, nil
		}
		existing.Status = requestStatusPending
		return existing.RequestID, false, nil
	}
	f.requests[newRequestID] = &data.FriendRequestRow{
		RequestID:   newRequestID,
		RequesterID: requesterID,
		TargetID:    targetID,
		Status:      requestStatusPending,
	}
	return newRequestID, false, nil
}

func (f *fakeRepo) GetRequest(_ context.Context, requestID uint64) (*data.FriendRequestRow, bool, error) {
	r, ok := f.requests[requestID]
	if !ok {
		return nil, false, nil
	}
	// 返回副本,避免测试改到内部状态
	cp := *r
	return &cp, true, nil
}

func (f *fakeRepo) addFriendEdge(a, b uint64) {
	if f.friends[a] == nil {
		f.friends[a] = map[uint64]int64{}
	}
	f.friends[a][b] = 1000
}

func (f *fakeRepo) AcceptRequest(_ context.Context, requestID, accepterID uint64, maxFriends int) (bool, error) {
	r, ok := f.requests[requestID]
	if !ok {
		return false, errcode.New(errcode.ErrFriendNotFound, "not found")
	}
	// 模拟并发:预检后到取锁前被 Block / 另一次 accept 处理(P1 回归用)
	if f.forceAcceptNotCompleted {
		return false, nil
	}
	if r.TargetID != accepterID {
		return false, errcode.New(errcode.ErrFriendNotFound, "not for accepter")
	}
	if r.Status != requestStatusPending {
		return false, nil // 已被并发处理,本次未真正完成
	}
	// 事务内权威 block 校验
	if f.blocks[accepterID][r.RequesterID] || f.blocks[r.RequesterID][accepterID] {
		return false, errcode.New(errcode.ErrFriendBlocked, "blocked")
	}
	if maxFriends > 0 {
		if len(f.friends[r.RequesterID]) >= maxFriends || len(f.friends[r.TargetID]) >= maxFriends {
			return false, errcode.New(errcode.ErrFriendLimit, "friend limit reached")
		}
	}
	r.Status = 2 // accepted
	f.addFriendEdge(r.RequesterID, r.TargetID)
	f.addFriendEdge(r.TargetID, r.RequesterID)
	return true, nil
}

func (f *fakeRepo) ListFriends(_ context.Context, playerID uint64) ([]data.FriendRow, error) {
	var out []data.FriendRow
	for fid, since := range f.friends[playerID] {
		out = append(out, data.FriendRow{FriendID: fid, SinceMs: since})
	}
	return out, nil
}

func (f *fakeRepo) Block(_ context.Context, playerID, targetID uint64) error {
	if f.blocks[playerID] == nil {
		f.blocks[playerID] = map[uint64]bool{}
	}
	f.blocks[playerID][targetID] = true
	delete(f.friends[playerID], targetID)
	delete(f.friends[targetID], playerID)
	if r := f.findByPair(playerID, targetID); r != nil && r.Status == requestStatusPending {
		r.Status = 3
	}
	if r := f.findByPair(targetID, playerID); r != nil && r.Status == requestStatusPending {
		r.Status = 3
	}
	return nil
}

// fakePusher 记录推送事件。
type fakePusher struct {
	events []*friendv1.FriendEvent
}

func (p *fakePusher) PushFriendEvent(_ context.Context, _ uint64, evt *friendv1.FriendEvent) error {
	p.events = append(p.events, evt)
	return nil
}

// fakeOnline 返回预置在线状态。
type fakeOnline struct {
	status map[uint64]data.OnlineStatus
}

func (o *fakeOnline) BatchOnline(_ context.Context, ids []uint64) map[uint64]data.OnlineStatus {
	out := map[uint64]data.OnlineStatus{}
	for _, id := range ids {
		if st, ok := o.status[id]; ok {
			out[id] = st
		}
	}
	return out
}

// newUC 构造带 fakes 的 usecase。
func newUC(repo data.FriendRepo, pusher FriendEventPusher, online data.OnlineStatusReader) *FriendUsecase {
	return NewFriendUsecase(repo, pusher, online, conf.FriendConf{MaxFriends: 200})
}

// ── 测试 ──────────────────────────────────────────────────────────────────────

func TestAddFriend_OK_PushesRequestReceived(t *testing.T) {
	repo := newFakeRepo()
	pusher := &fakePusher{}
	uc := newUC(repo, pusher, nil)

	reqID, err := uc.AddFriend(context.Background(), 100, 200, 999)
	if err != nil {
		t.Fatalf("AddFriend err: %v", err)
	}
	if reqID != 999 {
		t.Fatalf("want request_id 999, got %d", reqID)
	}
	if len(pusher.events) != 1 {
		t.Fatalf("want 1 push, got %d", len(pusher.events))
	}
	e := pusher.events[0]
	if e.GetToPlayerId() != 200 || e.GetByPlayerId() != 100 {
		t.Fatalf("push routing wrong: by=%d to=%d", e.GetByPlayerId(), e.GetToPlayerId())
	}
	if e.GetReason() != friendv1.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED {
		t.Fatalf("want REQUEST_RECEIVED, got %v", e.GetReason())
	}
}

func TestAddFriend_Self(t *testing.T) {
	uc := newUC(newFakeRepo(), &fakePusher{}, nil)
	_, err := uc.AddFriend(context.Background(), 100, 100, 1)
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("want ErrInvalidArg, got %v", err)
	}
}

func TestAddFriend_Blocked(t *testing.T) {
	repo := newFakeRepo()
	_ = repo.Block(context.Background(), 200, 100) // 200 拉黑了 100
	uc := newUC(repo, &fakePusher{}, nil)
	_, err := uc.AddFriend(context.Background(), 100, 200, 1)
	if errcode.As(err) != errcode.ErrFriendBlocked {
		t.Fatalf("want ErrFriendBlocked, got %v", err)
	}
}

func TestAddFriend_AlreadyFriends(t *testing.T) {
	repo := newFakeRepo()
	repo.addFriendEdge(100, 200)
	repo.addFriendEdge(200, 100)
	uc := newUC(repo, &fakePusher{}, nil)
	_, err := uc.AddFriend(context.Background(), 100, 200, 1)
	if errcode.As(err) != errcode.ErrFriendAlreadyAdded {
		t.Fatalf("want ErrFriendAlreadyAdded, got %v", err)
	}
}

func TestAddFriend_ReusePending(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo, &fakePusher{}, nil)
	id1, err := uc.AddFriend(context.Background(), 100, 200, 999)
	if err != nil {
		t.Fatalf("first AddFriend err: %v", err)
	}
	// 再次发起 → 复用已有 pending request_id,不新建
	id2, err := uc.AddFriend(context.Background(), 100, 200, 1234)
	if err != nil {
		t.Fatalf("second AddFriend err: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("want reuse request_id %d, got %d", id1, id2)
	}
}

func TestAddFriend_LimitReached_EarlyFail(t *testing.T) {
	repo := newFakeRepo()
	// requester 100 已有 2 个好友,上限设 2 → 再发请求提前失败
	repo.addFriendEdge(100, 11)
	repo.addFriendEdge(100, 12)
	uc := NewFriendUsecase(repo, &fakePusher{}, nil, conf.FriendConf{MaxFriends: 2})
	_, err := uc.AddFriend(context.Background(), 100, 200, 1)
	if errcode.As(err) != errcode.ErrFriendLimit {
		t.Fatalf("want ErrFriendLimit, got %v", err)
	}
}

func TestAcceptFriend_LimitReached_AtomicCheck(t *testing.T) {
	repo := newFakeRepo()
	uc := NewFriendUsecase(repo, &fakePusher{}, nil, conf.FriendConf{MaxFriends: 1})
	// requester 100 发请求时还没好友(提前校验过),挂起后 target 200 先攒满 1 个好友
	reqID, err := uc.AddFriend(context.Background(), 100, 200, 999)
	if err != nil {
		t.Fatalf("AddFriend err: %v", err)
	}
	repo.addFriendEdge(200, 77) // 200 已达上限 1
	// 接受时事务内对双方原子校验 → target 已满,拒绝
	if err := uc.AcceptFriend(context.Background(), 200, reqID); errcode.As(err) != errcode.ErrFriendLimit {
		t.Fatalf("want ErrFriendLimit at accept, got %v", err)
	}
	// 好友边不应建立
	if ok, _ := repo.AreFriends(context.Background(), 100, 200); ok {
		t.Fatal("friendship must not be created when limit hit")
	}
}

func TestAcceptFriend_OK_PushesAccepted(t *testing.T) {
	repo := newFakeRepo()
	pusher := &fakePusher{}
	uc := newUC(repo, pusher, nil)

	reqID, _ := uc.AddFriend(context.Background(), 100, 200, 999)
	pusher.events = nil // 清掉 REQUEST_RECEIVED

	if err := uc.AcceptFriend(context.Background(), 200, reqID); err != nil {
		t.Fatalf("AcceptFriend err: %v", err)
	}
	// 双向好友边已建立
	if ok, _ := repo.AreFriends(context.Background(), 100, 200); !ok {
		t.Fatal("want 100-200 friends")
	}
	if ok, _ := repo.AreFriends(context.Background(), 200, 100); !ok {
		t.Fatal("want 200-100 friends")
	}
	// 接受通知发给发起方 100
	if len(pusher.events) != 1 {
		t.Fatalf("want 1 push, got %d", len(pusher.events))
	}
	e := pusher.events[0]
	if e.GetToPlayerId() != 100 || e.GetByPlayerId() != 200 {
		t.Fatalf("accept push routing wrong: by=%d to=%d", e.GetByPlayerId(), e.GetToPlayerId())
	}
	if e.GetReason() != friendv1.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_ACCEPTED {
		t.Fatalf("want REQUEST_ACCEPTED, got %v", e.GetReason())
	}
}

func TestAcceptFriend_NotTarget(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo, &fakePusher{}, nil)
	reqID, _ := uc.AddFriend(context.Background(), 100, 200, 999)
	// 第三者 300 试图接受 → 找不到可接受请求
	err := uc.AcceptFriend(context.Background(), 300, reqID)
	if errcode.As(err) != errcode.ErrFriendNotFound {
		t.Fatalf("want ErrFriendNotFound, got %v", err)
	}
}

func TestAcceptFriend_NoRequest(t *testing.T) {
	uc := newUC(newFakeRepo(), &fakePusher{}, nil)
	err := uc.AcceptFriend(context.Background(), 200, 555)
	if errcode.As(err) != errcode.ErrFriendNotFound {
		t.Fatalf("want ErrFriendNotFound, got %v", err)
	}
}

// TestAcceptFriend_ConcurrentlyProcessed_NoFakeSuccess 回归 P1:
// 预检看到 pending,但 AcceptRequest 事务内发现请求已被并发处理(Block 改 rejected /
// 另一次 accept)→ 返回 accepted=false。biz 必须不推送 accepted、且不假成功(返回错误)。
func TestAcceptFriend_ConcurrentlyProcessed_NoFakeSuccess(t *testing.T) {
	repo := newFakeRepo()
	pusher := &fakePusher{}
	uc := newUC(repo, pusher, nil)

	reqID, _ := uc.AddFriend(context.Background(), 100, 200, 999)
	pusher.events = nil                 // 清掉 REQUEST_RECEIVED
	repo.forceAcceptNotCompleted = true // 模拟两步之间被并发处理

	err := uc.AcceptFriend(context.Background(), 200, reqID)
	if errcode.As(err) != errcode.ErrFriendNotFound {
		t.Fatalf("want ErrFriendNotFound (no fake success), got %v", err)
	}
	if len(pusher.events) != 0 {
		t.Fatalf("must NOT push accepted on concurrent processing, got %d", len(pusher.events))
	}
	// 好友边不应建立
	if ok, _ := repo.AreFriends(context.Background(), 100, 200); ok {
		t.Fatal("friendship must not exist when accept did not complete")
	}
}

func TestListFriends_FillsOnlineStatus(t *testing.T) {
	repo := newFakeRepo()
	repo.addFriendEdge(100, 200)
	repo.addFriendEdge(100, 300)
	online := &fakeOnline{status: map[uint64]data.OnlineStatus{
		200: {Online: true, LastSeenMs: 5000},
		// 300 不在 map → 离线
	}}
	uc := newUC(repo, &fakePusher{}, online)

	infos, err := uc.ListFriends(context.Background(), 100)
	if err != nil {
		t.Fatalf("ListFriends err: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("want 2 friends, got %d", len(infos))
	}
	byID := map[uint64]*friendv1.FriendInfo{}
	for _, fi := range infos {
		byID[fi.GetPlayerId()] = fi
	}
	if !byID[200].GetIsOnline() || byID[200].GetLastSeenMs() != 5000 {
		t.Fatalf("friend 200 online status wrong: %+v", byID[200])
	}
	if byID[300].GetIsOnline() {
		t.Fatalf("friend 300 should be offline: %+v", byID[300])
	}
}

func TestListFriends_NilOnlineReader(t *testing.T) {
	repo := newFakeRepo()
	repo.addFriendEdge(100, 200)
	uc := newUC(repo, &fakePusher{}, nil) // online reader 弱依赖缺失
	infos, err := uc.ListFriends(context.Background(), 100)
	if err != nil {
		t.Fatalf("ListFriends err: %v", err)
	}
	if len(infos) != 1 || infos[0].GetIsOnline() {
		t.Fatalf("want 1 offline friend, got %+v", infos)
	}
}

func TestBlock_RemovesFriendshipAndCancelsRequest(t *testing.T) {
	repo := newFakeRepo()
	repo.addFriendEdge(100, 200)
	repo.addFriendEdge(200, 100)
	uc := newUC(repo, &fakePusher{}, nil)

	if err := uc.Block(context.Background(), 100, 200); err != nil {
		t.Fatalf("Block err: %v", err)
	}
	if ok, _ := repo.AreFriends(context.Background(), 100, 200); ok {
		t.Fatal("friendship 100-200 should be removed")
	}
	if ok, _ := repo.AreFriends(context.Background(), 200, 100); ok {
		t.Fatal("friendship 200-100 should be removed")
	}
	if blocked, _ := repo.IsBlocked(context.Background(), 100, 200); !blocked {
		t.Fatal("100 should have blocked 200")
	}
	// 拉黑后不能再加好友
	_, err := uc.AddFriend(context.Background(), 100, 200, 1)
	if errcode.As(err) != errcode.ErrFriendBlocked {
		t.Fatalf("want ErrFriendBlocked after block, got %v", err)
	}
}

func TestBlock_Self(t *testing.T) {
	uc := newUC(newFakeRepo(), &fakePusher{}, nil)
	err := uc.Block(context.Background(), 100, 100)
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("want ErrInvalidArg, got %v", err)
	}
}
