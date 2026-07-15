package biz

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

// allocationFromStoredTarget reconstructs the exact allocator result checkpointed
// on the canonical match.  BattleTarget is written before the first placement
// Begin, so a process restart never has to guess which DS identity partially
// prepared players were bound to.
func allocationFromStoredTarget(target *matchv1.MatchBattleTargetStorageRecord) (*model.BattleAllocation, bool) {
	if target == nil {
		return nil, false
	}
	allocation := &model.BattleAllocation{
		Address: target.GetDsAddr(),
		Target: placement.Target{
			PodName:       target.GetDsPodName(),
			InstanceUID:   target.GetDsInstanceUid(),
			InstanceEpoch: target.GetDsInstanceEpoch(),
			AllocationID:  target.GetAllocationId(),
			ReleaseTrack:  target.GetReleaseTrack(),
		},
	}
	return allocation, allocation.Address != "" && allocation.Target.CompleteBattle()
}

func sameBattleAllocation(left, right *model.BattleAllocation) bool {
	return left != nil && right != nil && left.Address == right.Address &&
		left.Target.PodName == right.Target.PodName &&
		left.Target.InstanceUID == right.Target.InstanceUID &&
		left.Target.InstanceEpoch == right.Target.InstanceEpoch &&
		left.Target.AllocationID == right.Target.AllocationID &&
		left.Target.ReleaseTrack == right.Target.ReleaseTrack
}

// checkpointBattleAllocation is the allocation saga's durable handoff:
//
//	allocator READY -> CAS exact target on MatchStorageRecord -> placement Begin/Bind
//
// A retry always reuses this checkpoint.  A different target for the same
// operation is UNKNOWN/conflict and must never be used to overwrite already
// prepared player placements.
func (u *MatchUsecase) checkpointBattleAllocation(
	ctx context.Context,
	job *matchv1.MatchStorageRecord,
	allocation *model.BattleAllocation,
) (*model.BattleAllocation, error) {
	if job == nil || job.GetMatchId() == 0 || job.GetAllocationOperationId() == "" ||
		allocation == nil || allocation.Address == "" || !allocation.Target.CompleteBattle() {
		return nil, errcode.New(errcode.ErrInvalidArg, "complete allocation checkpoint required")
	}

	var checkpoint *model.BattleAllocation
	err := u.repo.UpdateMatchWithLock(ctx, job.GetMatchId(), u.cfg.OptimisticRetry, func(rec *matchv1.MatchStorageRecord) error {
		if rec.GetStage() != stageAllocating || rec.GetAllocationOperationId() != job.GetAllocationOperationId() {
			return errcode.New(errcode.ErrInvalidState,
				"match %d allocation operation no longer active", job.GetMatchId())
		}
		if rec.GetBattleTarget() != nil {
			existing, complete := allocationFromStoredTarget(rec.GetBattleTarget())
			if !complete {
				return errcode.New(errcode.ErrInvalidState,
					"match %d has incomplete durable battle target", job.GetMatchId())
			}
			if !sameBattleAllocation(existing, allocation) {
				return errcode.New(errcode.ErrMatchConcurrent,
					"match %d allocation target changed for operation %s",
					job.GetMatchId(), job.GetAllocationOperationId())
			}
			checkpoint = existing
			return nil
		}

		// Player bindings deliberately remain empty at this checkpoint.  They are
		// filled only by the READY CAS after every player returned the exact same
		// operation binding and every ticket was signed.
		rec.BattleTarget = battleTargetStorage(allocation, nil)
		checkpoint = &model.BattleAllocation{Address: allocation.Address, Target: allocation.Target}
		return nil
	}, u.matchTTL())
	if err != nil {
		return nil, err
	}
	if checkpoint == nil {
		return nil, errcode.New(errcode.ErrUnavailable,
			"match %d allocation checkpoint response is unknown", job.GetMatchId())
	}
	return checkpoint, nil
}

func validatePreparedBindings(operationID string, playerIDs []uint64, bindings map[uint64]placement.Binding) error {
	if !placement.ValidOperationID(operationID) || len(playerIDs) == 0 || len(bindings) != len(playerIDs) {
		return errcode.New(errcode.ErrUnavailable, "placement returned an incomplete player binding set")
	}
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, playerID := range playerIDs {
		if playerID == 0 {
			return errcode.New(errcode.ErrInvalidArg, "allocation roster contains zero player_id")
		}
		if _, duplicate := seen[playerID]; duplicate {
			return errcode.New(errcode.ErrInvalidArg, "allocation roster contains duplicate player %d", playerID)
		}
		seen[playerID] = struct{}{}
		binding, ok := bindings[playerID]
		if !ok || !binding.Complete() || binding.OperationID != operationID {
			return errcode.New(errcode.ErrUnavailable,
				"placement binding missing or drifted for player %d", playerID)
		}
	}
	for playerID := range bindings {
		if _, ok := seen[playerID]; !ok {
			return errcode.New(errcode.ErrUnavailable,
				"placement returned outsider binding for player %d", playerID)
		}
	}
	return nil
}

func validateSignedBattleTickets(playerIDs []uint64, tickets map[uint64]string) error {
	if len(tickets) != len(playerIDs) {
		return errcode.New(errcode.ErrUnavailable, "ticket signer returned an incomplete player ticket set")
	}
	for _, playerID := range playerIDs {
		if tickets[playerID] == "" {
			return errcode.New(errcode.ErrUnavailable, "ticket signer omitted player %d", playerID)
		}
	}
	return nil
}
