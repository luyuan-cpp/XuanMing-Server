package gm

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	gmv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/gm/v1"
)

func newTestService(t *testing.T) (*Service, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewService(rdb, log.DefaultLogger), func() {
		_ = rdb.Close()
		mr.Close()
	}
}

func addItemReq(matchID uint64, playerID uint64, configID uint32, count int32) *gmv1.SendCommandRequest {
	return &gmv1.SendCommandRequest{
		MatchId: matchID,
		Payload: &gmv1.SendCommandRequest_AddItem{AddItem: &gmv1.AddItemCommand{
			PlayerId: playerID,
			ConfigId: configID,
			Count:    count,
		}},
	}
}

// TestSendThenPoll_FIFO 验证入队 → 轮询取出,顺序为 FIFO,且取即出队。
func TestSendThenPoll_FIFO(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	// 先发 3 条(config 10001/10002/10003)。
	for _, cid := range []uint32{10001, 10002, 10003} {
		resp, err := s.SendCommand(ctx, addItemReq(42, 1001, cid, 1))
		if err != nil {
			t.Fatalf("SendCommand: %v", err)
		}
		if resp.GetCode() != commonv1.ErrCode_OK || resp.GetIdempotencyKey() == "" {
			t.Fatalf("SendCommand code=%v id=%q", resp.GetCode(), resp.GetIdempotencyKey())
		}
	}

	poll, err := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 42})
	if err != nil {
		t.Fatalf("PollCommands: %v", err)
	}
	if poll.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("PollCommands code=%v", poll.GetCode())
	}
	if len(poll.GetCommands()) != 3 {
		t.Fatalf("want 3 commands, got %d", len(poll.GetCommands()))
	}
	// FIFO:先发的 10001 先出。
	wantOrder := []uint32{10001, 10002, 10003}
	for i, c := range poll.GetCommands() {
		if c.GetAddItem().GetConfigId() != wantOrder[i] {
			t.Fatalf("cmd[%d] config=%d want %d", i, c.GetAddItem().GetConfigId(), wantOrder[i])
		}
	}

	// 再轮询应为空(取即出队)。
	poll2, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 42})
	if len(poll2.GetCommands()) != 0 {
		t.Fatalf("second poll want 0, got %d", len(poll2.GetCommands()))
	}
}

// TestPoll_IsolatedByMatch 验证不同 match_id 队列互相隔离。
func TestPoll_IsolatedByMatch(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := s.SendCommand(ctx, addItemReq(1, 1001, 10001, 5)); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	// 另一局轮询应为空。
	poll, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 2})
	if len(poll.GetCommands()) != 0 {
		t.Fatalf("match 2 want 0, got %d", len(poll.GetCommands()))
	}
	// 本局能取到。
	poll1, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 1})
	if len(poll1.GetCommands()) != 1 || poll1.GetCommands()[0].GetAddItem().GetCount() != 5 {
		t.Fatalf("match 1 unexpected: %+v", poll1.GetCommands())
	}
}

// TestSendCommand_Validation 验证非法入参被拒。
func TestSendCommand_Validation(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		name string
		req  *gmv1.SendCommandRequest
	}{
		{"zero_match", addItemReq(0, 1001, 10001, 1)},
		{"zero_player", addItemReq(1, 0, 10001, 1)},
		{"zero_config", addItemReq(1, 1001, 0, 1)},
		{"zero_count", addItemReq(1, 1001, 10001, 0)},
		{"bad_bag", &gmv1.SendCommandRequest{
			MatchId: 1,
			Payload: &gmv1.SendCommandRequest_AddItem{AddItem: &gmv1.AddItemCommand{
				PlayerId: 1001, ConfigId: 10001, Count: 1, BagType: 9,
			}},
		}},
		{"nil_payload", &gmv1.SendCommandRequest{MatchId: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := s.SendCommand(ctx, tc.req)
			if err != nil {
				t.Fatalf("SendCommand err: %v", err)
			}
			if resp.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
				t.Fatalf("want ERR_INVALID_ARG, got %v", resp.GetCode())
			}
		})
	}
}

// TestPollMax_Clamped 验证一次拉取条数夹取到 maxPollMax。
func TestPollMax_Clamped(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < maxPollMax+10; i++ {
		if _, err := s.SendCommand(ctx, addItemReq(7, 1001, 10001, 1)); err != nil {
			t.Fatalf("SendCommand: %v", err)
		}
	}
	poll, _ := s.PollCommands(ctx, &gmv1.PollCommandsRequest{MatchId: 7, Max: 1000})
	if len(poll.GetCommands()) != maxPollMax {
		t.Fatalf("want %d (clamped), got %d", maxPollMax, len(poll.GetCommands()))
	}
}

// TestAckCommand 验证 Ack 回报的基本校验与成功路径。
func TestAckCommand(t *testing.T) {
	s, cleanup := newTestService(t)
	defer cleanup()
	ctx := context.Background()

	if resp, _ := s.AckCommand(ctx, &gmv1.AckCommandRequest{MatchId: 0, IdempotencyKey: "x"}); resp.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
		t.Fatalf("zero match want ERR_INVALID_ARG, got %v", resp.GetCode())
	}
	if resp, _ := s.AckCommand(ctx, &gmv1.AckCommandRequest{MatchId: 1, IdempotencyKey: ""}); resp.GetCode() != commonv1.ErrCode_ERR_INVALID_ARG {
		t.Fatalf("empty id want ERR_INVALID_ARG, got %v", resp.GetCode())
	}
	if resp, _ := s.AckCommand(ctx, &gmv1.AckCommandRequest{MatchId: 1, IdempotencyKey: "abc", Ok: true}); resp.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("valid ack want OK, got %v", resp.GetCode())
	}
}

// fakeChecker 是 BattleLivenessChecker 的可控假体:found/err 由测试指定。
type fakeChecker struct {
	found bool
	err   error
}

func (f fakeChecker) GetBattle(context.Context, uint64) (*dsv1.BattleStorageRecord, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.found {
		return nil, false, nil
	}
	return &dsv1.BattleStorageRecord{MatchId: 1}, true, nil
}

// TestSendCommand_LivenessCheck 验证 battleChecker 三态:镜像缺失拒(NOT_FOUND)、
// 读失败 fail-open(仍入队 OK)、镜像存在放行(OK)。
func TestSendCommand_LivenessCheck(t *testing.T) {
	ctx := context.Background()

	// 缺失 → ERR_NOT_FOUND
	s1, c1 := newTestService(t)
	defer c1()
	s1.SetBattleChecker(fakeChecker{found: false})
	if resp, _ := s1.SendCommand(ctx, addItemReq(1, 1001, 10001, 1)); resp.GetCode() != commonv1.ErrCode_ERR_NOT_FOUND {
		t.Fatalf("missing match want ERR_NOT_FOUND, got %v", resp.GetCode())
	}

	// 读失败 → fail-open,仍 OK 入队
	s2, c2 := newTestService(t)
	defer c2()
	s2.SetBattleChecker(fakeChecker{err: context.DeadlineExceeded})
	if resp, _ := s2.SendCommand(ctx, addItemReq(1, 1001, 10001, 1)); resp.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("checker error want fail-open OK, got %v", resp.GetCode())
	}

	// 存在 → OK
	s3, c3 := newTestService(t)
	defer c3()
	s3.SetBattleChecker(fakeChecker{found: true})
	if resp, _ := s3.SendCommand(ctx, addItemReq(1, 1001, 10001, 1)); resp.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("active match want OK, got %v", resp.GetCode())
	}
}
