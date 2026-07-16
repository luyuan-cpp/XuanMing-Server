package biz

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

// allocationFromStoredTarget reconstructs the exact allocator result checkpointed
// on the canonical match.  BattleTarget is written before any ticket is signed,
// so a process restart never has to guess which DS identity was allocated.
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
//	allocator READY -> CAS exact target on MatchStorageRecord -> ticket signing
//
// A retry always reuses this checkpoint.  A different target for the same
// operation is UNKNOWN/conflict and must never overwrite the checkpointed DS.
func (u *MatchUsecase) checkpointBattleAllocation(
	ctx context.Context,
	job *matchv1.MatchStorageRecord,
	allocation *model.BattleAllocation,
) (*model.BattleAllocation, error) {
	if job == nil || job.GetMatchId() == 0 || !placement.ValidOperationID(job.GetAllocationOperationId()) ||
		job.GetStage() != stageAllocating ||
		job.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING ||
		job.GetBattleTarget() != nil ||
		allocation == nil || allocation.Address == "" || !allocation.Target.CompleteBattle() {
		return nil, errcode.New(errcode.ErrInvalidArg, "complete allocation checkpoint required")
	}

	expectedTarget := battleTargetStorage(allocation)
	var checkpoint *model.BattleAllocation
	err := u.repo.UpdateMatchWithLock(ctx, job.GetMatchId(), u.cfg.OptimisticRetry, func(rec *matchv1.MatchStorageRecord) error {
		if rec.GetBattleTarget() != nil {
			expected := cloneMatch(job)
			expected.BattleTarget = expectedTarget
			if !exactAllocationSnapshot(rec, expected,
				matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING) {
				return errcode.New(errcode.ErrMatchConcurrent,
					"match %d allocation checkpoint generation changed for operation %s",
					job.GetMatchId(), job.GetAllocationOperationId())
			}
			existing, complete := allocationFromStoredTarget(rec.GetBattleTarget())
			if !complete || !sameBattleAllocation(existing, allocation) {
				return errcode.New(errcode.ErrMatchConcurrent,
					"match %d allocation checkpoint is not the exact allocator result", job.GetMatchId())
			}
			checkpoint = existing
			return nil
		}
		if !exactUncheckpointedRequestingAllocation(rec, job) {
			return errcode.New(errcode.ErrMatchConcurrent,
				"match %d allocation operation no longer exact REQUESTING", job.GetMatchId())
		}

		rec.BattleTarget = expectedTarget
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

// fenceRequestingAllocationCheckpoint is the authorization linearization
// point immediately before each post-checkpoint external side effect.  The
// no-op WATCH/CAS is deliberate: either this REQUESTING generation wins before
// an abort fence, or ABORTING wins and the old worker cannot proceed to the
// next placement/departure/ticket operation.
func (u *MatchUsecase) fenceRequestingAllocationCheckpoint(
	ctx context.Context,
	matchID uint64,
	operationID string,
	allocation *model.BattleAllocation,
) (*matchv1.MatchStorageRecord, error) {
	if matchID == 0 || !placement.ValidOperationID(operationID) || allocation == nil ||
		allocation.Address == "" || !allocation.Target.CompleteBattle() {
		return nil, errcode.New(errcode.ErrInvalidArg, "complete requesting allocation fence required")
	}
	expected := &matchv1.MatchStorageRecord{
		MatchId: matchID, Stage: stageAllocating,
		AllocationPhase:       matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING,
		AllocationOperationId: operationID,
		BattleTarget:          battleTargetStorage(allocation),
	}
	var fenced *matchv1.MatchStorageRecord
	err := u.repo.UpdateMatchWithLock(ctx, matchID, u.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			if !exactAllocationSnapshot(rec, expected,
				matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING) {
				return errcode.New(errcode.ErrMatchConcurrent,
					"match %d requesting allocation checkpoint changed", matchID)
			}
			fenced = cloneMatch(rec)
			return nil
		}, u.matchTTL())
	if err != nil {
		return nil, err
	}
	if fenced == nil {
		return nil, errcode.New(errcode.ErrUnavailable,
			"match %d requesting allocation fence result unknown", matchID)
	}
	return fenced, nil
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
