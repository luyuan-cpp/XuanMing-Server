package biz

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/social/dialogue/internal/data"
)

// TestChooseOption_RejectedChoiceLeavesSessionAtCurrentNode 固化服务端权威选择边界：
// 客户端提交不存在或不可见的 option_id 时，不能推进、删除或毒化当前会话。
func TestChooseOption_RejectedChoiceLeavesSessionAtCurrentNode(t *testing.T) {
	for _, optionID := range []string{"no-such-option", "secret"} {
		t.Run(optionID, func(t *testing.T) {
			u := newUsecase()
			ctx := context.Background()
			if _, err := u.StartDialogue(ctx, 7, testNpcID, 1000); err != nil {
				t.Fatalf("StartDialogue: %v", err)
			}

			if _, err := u.ChooseOption(ctx, 7, 1000, optionID); errcode.As(err) != errcode.ErrDialogueOptionInvalid {
				t.Fatalf("rejected option code=%d want=%d err=%v",
					errcode.As(err), errcode.ErrDialogueOptionInvalid, err)
			}

			// 若拒绝路径错误推进或删除了会话，从 greet 选择 menu 将不能再成功。
			state, err := u.ChooseOption(ctx, 7, 1000, "menu")
			if err != nil {
				t.Fatalf("valid choice after rejection: %v", err)
			}
			if state.GetNodeId() != "menu" || state.GetEnded() {
				t.Fatalf("session changed by rejected option: %+v", state)
			}
		})
	}
}

type expiredSessionProbe struct {
	session     data.Session
	updateCalls int
	deleteCalls int
}

func (*expiredSessionProbe) Create(context.Context, *data.Session) (bool, error) {
	return false, nil
}

func (s *expiredSessionProbe) Get(_ context.Context, dialogueID uint64, now int64) (*data.Session, bool, error) {
	if dialogueID != s.session.DialogueID || now >= s.session.ExpiresMs {
		return nil, false, nil
	}
	cp := s.session
	return &cp, true, nil
}

func (s *expiredSessionProbe) Update(_ context.Context, session *data.Session) error {
	s.updateCalls++
	s.session = *session
	return nil
}

func (s *expiredSessionProbe) Delete(context.Context, uint64) error {
	s.deleteCalls++
	return nil
}

// TestChooseOption_ExpiredSessionNeverReachesMutation 固化过期边界：存储已把会话判为
// 不存在后，usecase 只能返回 NOT_FOUND，不能再调用 Update/Delete 推进或结束旧会话。
func TestChooseOption_ExpiredSessionNeverReachesMutation(t *testing.T) {
	store := &expiredSessionProbe{session: data.Session{
		DialogueID: 1000,
		PlayerID:   7,
		NpcID:      testNpcID,
		NodeID:     "greet",
		ExpiresMs:  time.Now().Add(-time.Second).UnixMilli(),
	}}
	u := NewDialogueUsecase(data.NewConfigTreeProvider(newTestTree()), store, time.Minute)

	_, err := u.ChooseOption(context.Background(), 7, 1000, "menu")
	if errcode.As(err) != errcode.ErrDialogueNotFound {
		t.Fatalf("expired session code=%d want=%d err=%v",
			errcode.As(err), errcode.ErrDialogueNotFound, err)
	}
	if store.updateCalls != 0 || store.deleteCalls != 0 || store.session.NodeID != "greet" {
		t.Fatalf("expired session mutated: update=%d delete=%d node=%q",
			store.updateCalls, store.deleteCalls, store.session.NodeID)
	}
}
