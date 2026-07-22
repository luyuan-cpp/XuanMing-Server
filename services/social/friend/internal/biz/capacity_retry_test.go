package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/social/friend/internal/conf"
)

// 好友申请收件箱达到硬上限后，新请求必须被拒绝；同一 pending 请求的重试仍应
// 复用原 request_id，不能因为收件箱已满而把幂等重试误判成新写入。
func TestAddFriend_IncomingInboxFullStillAllowsPendingRetry(t *testing.T) {
	repo := newFakeRepo()
	pusher := &fakePusher{}
	uc := NewFriendUsecase(repo, pusher, nil, conf.FriendConf{
		MaxFriends:          200,
		MaxIncomingRequests: 1,
	})
	ctx := context.Background()

	firstID, err := uc.AddFriend(ctx, 100, 900, 1001)
	if err != nil {
		t.Fatalf("first AddFriend err: %v", err)
	}
	if firstID != 1001 {
		t.Fatalf("first request_id=%d want=1001", firstID)
	}

	if _, err := uc.AddFriend(ctx, 200, 900, 2001); errcode.As(err) != errcode.ErrFriendRequestLimit {
		t.Fatalf("full inbox code=%d want=%d err=%v", errcode.As(err), errcode.ErrFriendRequestLimit, err)
	}
	if got := len(repo.requests); got != 1 {
		t.Fatalf("full inbox must not create another request: rows=%d want=1", got)
	}
	if got := len(pusher.events); got != 1 {
		t.Fatalf("rejected request must not emit push: events=%d want=1", got)
	}

	retryID, err := uc.AddFriend(ctx, 100, 900, 9999)
	if err != nil {
		t.Fatalf("pending retry err: %v", err)
	}
	if retryID != firstID {
		t.Fatalf("pending retry request_id=%d want original=%d", retryID, firstID)
	}
	if got := len(repo.requests); got != 1 {
		t.Fatalf("pending retry must not create another row: rows=%d want=1", got)
	}
}
