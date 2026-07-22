package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	teamv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/team/v1"
)

// TestSetReadyRejectsMatchingAndBattleStatesWithoutMutation 覆盖匹配中、战斗中继续
// 改 Ready/英雄的非法操作；拒绝后队伍快照与推送次数都必须保持不变。
func TestSetReadyRejectsMatchingAndBattleStatesWithoutMutation(t *testing.T) {
	for _, state := range []teamv1.TeamState{stateMatching, stateInBattle} {
		t.Run(state.String(), func(t *testing.T) {
			uc, pusher, cleanup := newTestUsecase(t)
			defer cleanup()
			ctx := context.Background()
			const teamID, playerID = uint64(8101), uint64(9101)

			if _, err := uc.CreateTeam(ctx, teamID, playerID); err != nil {
				t.Fatalf("CreateTeam: %v", err)
			}
			if err := uc.repo.UpdateWithLock(ctx, teamID, uc.cfg.OptimisticRetry,
				func(team *teamv1.TeamStorageRecord) error {
					team.State = state
					return nil
				}, uc.activeTTL()); err != nil {
				t.Fatalf("seed state %v: %v", state, err)
			}
			pushesBefore := len(pusher.calls)

			if _, err := uc.SetReady(ctx, teamID, playerID, true, 77); errcode.As(err) != errcode.ErrTeamWrongState {
				t.Fatalf("SetReady in state %v err=%v, want ErrTeamWrongState", state, err)
			}
			team, err := uc.GetTeam(ctx, teamID)
			if err != nil {
				t.Fatalf("GetTeam after rejection: %v", err)
			}
			if team.GetState() != state {
				t.Fatalf("state changed after rejection: got=%v want=%v", team.GetState(), state)
			}
			if len(team.GetMembers()) != 1 || team.GetMembers()[0].GetReady() || team.GetMembers()[0].GetHeroId() != 0 {
				t.Fatalf("member mutated after rejection: %+v", team.GetMembers())
			}
			if len(pusher.calls) != pushesBefore {
				t.Fatalf("rejected operation emitted push: before=%d after=%d", pushesBefore, len(pusher.calls))
			}
		})
	}
}
