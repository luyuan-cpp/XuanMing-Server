package data

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/battleabort"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// BattleAllocationLifecycleRepo is deliberately separate from BattleRepo.
// Only the Model-B lifecycle recovery path needs authority to attest a Kafka
// ACK for one exact, already-torn-down allocation.
type BattleAllocationLifecycleRepo interface {
	RecordAllocationLifecyclePublished(context.Context, uint64, placement.Target) error
}

func battleAllocationLifecyclePublishedKey(matchID uint64) string {
	return fmt.Sprintf("pandora:ds:allocation-lifecycle-published:{%d}", matchID)
}

func lifecyclePublishedTarget(record *dsv1.BattleAllocationLifecyclePublishedStorageRecord) placement.Target {
	if record == nil {
		return placement.Target{}
	}
	return placement.Target{
		PodName: record.GetDsPodName(), InstanceUID: record.GetGameserverUid(),
		InstanceEpoch: record.GetInstanceEpoch(), AllocationID: record.GetAllocationId(),
		ReleaseTrack: record.GetReleaseTrack(),
	}
}

func lifecyclePublishedRecordMatches(
	record *dsv1.BattleAllocationLifecyclePublishedStorageRecord,
	matchID uint64,
	target placement.Target,
) bool {
	return record != nil && record.GetMatchId() == matchID &&
		record.GetPhase() == dsv1.DSLifecyclePhase_DS_LIFECYCLE_PHASE_ABANDONED &&
		record.GetPublishedAtMs() > 0 && lifecyclePublishedTarget(record).Equal(target)
}

func readAllocationLifecyclePublished(
	ctx context.Context,
	command redis.Cmdable,
	matchID uint64,
	key string,
) (*dsv1.BattleAllocationLifecyclePublishedStorageRecord, bool, error) {
	payload, err := command.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	record := &dsv1.BattleAllocationLifecyclePublishedStorageRecord{}
	if err := proto.Unmarshal(payload, record); err != nil {
		return nil, false, errcode.NewCause(errcode.ErrInternal, err,
			"decode battle %d allocation lifecycle marker", matchID)
	}
	if record.GetMatchId() != matchID || !battleabort.ValidTarget(lifecyclePublishedTarget(record)) ||
		record.GetPhase() != dsv1.DSLifecyclePhase_DS_LIFECYCLE_PHASE_ABANDONED ||
		record.GetPublishedAtMs() <= 0 {
		return nil, false, errcode.New(errcode.ErrInvalidState,
			"battle %d allocation lifecycle marker is malformed", matchID)
	}
	return record, true, nil
}

func marshalAllocationLifecyclePublished(record *dsv1.BattleAllocationLifecyclePublishedStorageRecord) ([]byte, error) {
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
	if err != nil {
		return nil, errcode.NewCause(errcode.ErrInternal, err,
			"marshal battle allocation lifecycle marker")
	}
	return payload, nil
}

// RecordAllocationLifecyclePublished persists the Kafka ACK only after the
// exact GameServer+Pod teardown proof and canonical battle target are both
// present. The marker never expires. If its Redis response is lost, replaying
// the same lifecycle event and this method is idempotent; a different target
// can never inherit the old ACK.
func (r *RedisBattleRepo) RecordAllocationLifecyclePublished(
	ctx context.Context,
	matchID uint64,
	target placement.Target,
) error {
	return recordAllocationLifecyclePublished(ctx, r.rdb, r.StrictModelBWritesEnabled(), matchID, target)
}

// RedisBattleAuthRepo exposes the same narrowly-scoped capability to the
// Model-B usecase. Both repository views share the Redis authority; no caller
// receives a broader BattleRepo mutation surface merely to write this proof.
func (r *RedisBattleAuthRepo) RecordAllocationLifecyclePublished(
	ctx context.Context,
	matchID uint64,
	target placement.Target,
) error {
	return recordAllocationLifecyclePublished(ctx, r.rdb, r.StrictModelBWritesEnabled(), matchID, target)
}

func recordAllocationLifecyclePublished(
	ctx context.Context,
	rdb redis.UniversalClient,
	strictModelBWrites bool,
	matchID uint64,
	target placement.Target,
) error {
	if matchID == 0 || !battleabort.ValidTarget(target) {
		return errcode.New(errcode.ErrInvalidArg,
			"complete battle allocation lifecycle target required")
	}
	source := BattleDepartureSource{
		DSPodName: target.PodName, GameServerUID: target.InstanceUID,
		InstanceEpoch: target.InstanceEpoch, AllocationID: target.AllocationID,
	}
	tKey := battleInstanceTeardownKey(matchID, source)
	mKey := battleAllocationLifecyclePublishedKey(matchID)
	bKey := battleKey(matchID)
	aKey := battleAuthKey(matchID)
	jKey := battleAllocationAbortKey(matchID)
	for attempt := 0; attempt < battleAuthCASRetries; attempt++ {
		err := rdb.Watch(ctx, func(tx *redis.Tx) error {
			marker, found, err := readAllocationLifecyclePublished(ctx, tx, matchID, mKey)
			if err != nil {
				return err
			}
			if found {
				if !lifecyclePublishedRecordMatches(marker, matchID, target) {
					return errcode.New(errcode.ErrInvalidState,
						"battle %d allocation lifecycle marker tuple conflict", matchID)
				}
				return nil
			}
			teardown, teardownFound, err := readTeardownProof(ctx, tx, matchID, source)
			if err != nil {
				return err
			}
			if !teardownFound || !departureSourceEqualsTeardown(source, teardown) {
				return errcode.New(errcode.ErrInvalidState,
					"battle %d lifecycle ACK lacks exact teardown proof", matchID)
			}
			authRecord, battle, err := readBoundAuthority(tx, ctx, matchID, aKey, bKey)
			if err != nil {
				return err
			}
			battleBefore := proto.Clone(battle).(*dsv1.BattleStorageRecord)
			expected := BattleExpectedInstance{
				AllocationID: target.AllocationID, InstanceUID: target.InstanceUID,
				InstanceEpoch: target.InstanceEpoch,
			}
			abortTransition := false
			if battle.GetState() == BattleStateAllocationAbortPending {
				abortRecord, abortFound, abortErr := readAllocationAbortRecord(ctx, tx, matchID, jKey)
				if abortErr != nil {
					return abortErr
				}
				abortTransition = abortFound && abortRecord.GetReleasedAtMs() == 0 &&
					abortRequestFromRecord(abortRecord).Target.Equal(target)
			}
			if !battleAuthRecordV2Exact(authRecord) ||
				authRecord.GetPhase() != dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING ||
				!expectedBattleInstanceMatches(authRecord, battle, expected) ||
				(battle.GetState() != "abandoned" && !abortTransition) ||
				battle.GetDsPodName() != target.PodName || battle.GetGameserverUid() != target.InstanceUID ||
				battle.GetInstanceEpoch() != target.InstanceEpoch || battle.GetAllocationId() != target.AllocationID ||
				battle.GetReleaseTrack() != target.ReleaseTrack {
				return errcode.New(errcode.ErrInvalidState,
					"battle %d lifecycle ACK lacks exact abandoned/terminating authority", matchID)
			}
			marker = &dsv1.BattleAllocationLifecyclePublishedStorageRecord{
				MatchId: matchID, DsPodName: target.PodName, GameserverUid: target.InstanceUID,
				InstanceEpoch: target.InstanceEpoch, AllocationId: target.AllocationID,
				ReleaseTrack:  target.ReleaseTrack,
				Phase:         dsv1.DSLifecyclePhase_DS_LIFECYCLE_PHASE_ABANDONED,
				PublishedAtMs: time.Now().UnixMilli(),
			}
			payload, err := marshalAllocationLifecyclePublished(marker)
			if err != nil {
				return err
			}
			var battlePayload []byte
			if abortTransition {
				battle.State = "abandoned"
				if strictModelBWrites {
					battlePayload, err = marshalBattleTransition(battleBefore, battle)
				} else {
					battlePayload, err = proto.Marshal(battle)
				}
				if err != nil {
					return err
				}
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, mKey, payload, 0)
				if abortTransition {
					// The lifecycle ACK is the terminal transition for an allocation
					// abort. Keep both authority keys permanent until the abort journal
					// itself is atomically marked RELEASED.
					pipe.Set(ctx, bKey, battlePayload, 0)
				}
				return nil
			})
			return err
		}, tKey, mKey, bKey, aKey, jKey)
		if err == redis.TxFailedErr {
			continue
		}
		return err
	}
	return errcode.New(errcode.ErrInternal,
		"battle %d allocation lifecycle marker CAS retry exhausted", matchID)
}
