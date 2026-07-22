package service

import (
	"context"
	"testing"
	"time"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	dialoguev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/dialogue/v1"
	"github.com/luyuancpp/pandora/services/social/dialogue/internal/biz"
	"github.com/luyuancpp/pandora/services/social/dialogue/internal/data"
)

type dialogueIDGenProbe struct {
	next  uint64
	calls int
}

func (g *dialogueIDGenProbe) Generate() uint64 {
	g.calls++
	g.next++
	return g.next
}

func newDialogueServiceForIdentityTest() (*DialogueService, *dialogueIDGenProbe) {
	trees := map[uint32]*data.DialogueTree{
		1: {
			NpcID:     1,
			Speaker:   "NPC",
			StartNode: "start",
			Nodes: map[string]*data.DialogueNode{
				"start": {
					NodeID: "start",
					Options: []data.DialogueOption{
						{OptionID: "continue", Visible: true, NextNode: "done"},
					},
				},
				"done": {NodeID: "done", Text: "finished"},
			},
		},
	}
	uc := biz.NewDialogueUsecase(
		data.NewConfigTreeProvider(trees),
		data.NewMemorySessionStore(),
		time.Minute,
	)
	gen := &dialogueIDGenProbe{next: 1000}
	return NewDialogueService(uc, gen), gen
}

// TestDialogueServiceRequiresPlayerIdentityBeforeBusiness 固化所有玩家 RPC 的入口身份门：
// 无 JWT player_id 时必须在生成 dialogue_id 或触达会话存储之前拒绝。
func TestDialogueServiceRequiresPlayerIdentityBeforeBusiness(t *testing.T) {
	svc, gen := newDialogueServiceForIdentityTest()
	ctx := context.Background()

	start, err := svc.StartDialogue(ctx, &dialoguev1.StartDialogueRequest{NpcId: 1})
	if err != nil || start.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED {
		t.Fatalf("StartDialogue code=%s err=%v", start.GetCode(), err)
	}
	choose, err := svc.ChooseOption(ctx, &dialoguev1.ChooseOptionRequest{DialogueId: 1001, OptionId: "continue"})
	if err != nil || choose.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED {
		t.Fatalf("ChooseOption code=%s err=%v", choose.GetCode(), err)
	}
	end, err := svc.EndDialogue(ctx, &dialoguev1.EndDialogueRequest{DialogueId: 1001})
	if err != nil || end.GetCode() != commonv1.ErrCode_ERR_UNAUTHORIZED {
		t.Fatalf("EndDialogue code=%s err=%v", end.GetCode(), err)
	}
	if gen.calls != 0 {
		t.Fatalf("unauthorized start generated %d dialogue ids", gen.calls)
	}
}

// TestDialogueServiceContextIdentityPreventsCrossPlayerMutation 证明会话归属只取可信 ctx：
// 其他玩家的选择按不存在处理，幂等 End 也不得删除原玩家会话。
func TestDialogueServiceContextIdentityPreventsCrossPlayerMutation(t *testing.T) {
	svc, _ := newDialogueServiceForIdentityTest()
	ownerCtx := context.WithValue(context.Background(), plog.CtxKeyPlayerID, uint64(7))
	otherCtx := context.WithValue(context.Background(), plog.CtxKeyPlayerID, uint64(8))

	start, err := svc.StartDialogue(ownerCtx, &dialoguev1.StartDialogueRequest{NpcId: 1})
	if err != nil || start.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("owner start code=%s err=%v", start.GetCode(), err)
	}
	dialogueID := start.GetState().GetDialogueId()

	wrongChoice, err := svc.ChooseOption(otherCtx, &dialoguev1.ChooseOptionRequest{
		DialogueId: dialogueID,
		OptionId:   "continue",
	})
	if err != nil || wrongChoice.GetCode() != commonv1.ErrCode_ERR_DIALOGUE_NOT_FOUND {
		t.Fatalf("cross-player choose code=%s err=%v", wrongChoice.GetCode(), err)
	}
	wrongEnd, err := svc.EndDialogue(otherCtx, &dialoguev1.EndDialogueRequest{DialogueId: dialogueID})
	if err != nil || wrongEnd.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("cross-player idempotent end code=%s err=%v", wrongEnd.GetCode(), err)
	}

	ownerChoice, err := svc.ChooseOption(ownerCtx, &dialoguev1.ChooseOptionRequest{
		DialogueId: dialogueID,
		OptionId:   "continue",
	})
	if err != nil || ownerChoice.GetCode() != commonv1.ErrCode_OK {
		t.Fatalf("owner choose after cross-player calls code=%s err=%v", ownerChoice.GetCode(), err)
	}
	if ownerChoice.GetState().GetNodeId() != "done" || !ownerChoice.GetState().GetEnded() {
		t.Fatalf("owner session was changed by another player: %+v", ownerChoice.GetState())
	}
}
