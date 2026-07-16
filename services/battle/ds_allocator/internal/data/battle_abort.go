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

// BattleAllocationAbortFenceResult is the durable snapshot returned by the
// pre-admission abort linearization point. Released may be true even after the
// bounded auth/battle audit records have expired because the exact ACK journal
// is intentionally permanent.
type BattleAllocationAbortFenceResult struct {
	Battle   *dsv1.BattleStorageRecord
	Released bool
}

// BattleAllocationAbortRepo owns the exact, same-slot abort journal.  It is a
// separate capability from BattleAuthRepo so code that can rotate DS callback
// credentials does not automatically gain destructive allocation authority.
type BattleAllocationAbortRepo interface {
	FenceAllocationAbortExpected(context.Context, battleabort.Request) (BattleAllocationAbortFenceResult, error)
	ReadAllocationAbort(context.Context, uint64) (battleabort.Request, bool, bool, error)
	CompleteAllocationAbortExpected(context.Context, battleabort.Request, time.Duration, time.Duration) (bool, error)
}

func battleAllocationAbortKey(matchID uint64) string {
	return fmt.Sprintf("pandora:ds:allocation-abort:{%d}", matchID)
}

func abortRecordMatchesRequest(record *dsv1.BattleAllocationAbortStorageRecord, request battleabort.Request) bool {
	return record != nil && record.GetMatchId() == request.MatchID &&
		record.GetAllocationOperationId() == request.OperationID &&
		record.GetDsPodName() == request.Target.PodName &&
		record.GetGameserverUid() == request.Target.InstanceUID &&
		record.GetInstanceEpoch() == request.Target.InstanceEpoch &&
		record.GetAllocationId() == request.Target.AllocationID &&
		record.GetReleaseTrack() == request.Target.ReleaseTrack
}

func abortRequestFromRecord(record *dsv1.BattleAllocationAbortStorageRecord) battleabort.Request {
	if record == nil {
		return battleabort.Request{}
	}
	return battleabort.Request{
		MatchID: record.GetMatchId(), OperationID: record.GetAllocationOperationId(),
		Target: placement.Target{
			PodName: record.GetDsPodName(), InstanceUID: record.GetGameserverUid(),
			InstanceEpoch: record.GetInstanceEpoch(), AllocationID: record.GetAllocationId(),
			ReleaseTrack: record.GetReleaseTrack(),
		},
	}
}

func readAllocationAbortRecord(ctx context.Context, command redis.Cmdable, matchID uint64, key string) (*dsv1.BattleAllocationAbortStorageRecord, bool, error) {
	payload, err := command.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	record := &dsv1.BattleAllocationAbortStorageRecord{}
	if err := proto.Unmarshal(payload, record); err != nil {
		return nil, false, errcode.NewCause(errcode.ErrInternal, err,
			"decode battle %d allocation abort journal", matchID)
	}
	if record.GetMatchId() != matchID || !abortRequestFromRecord(record).Complete() || record.GetRequestedAtMs() <= 0 {
		return nil, false, errcode.New(errcode.ErrInvalidState,
			"battle %d allocation abort journal is malformed", matchID)
	}
	return record, true, nil
}

func marshalAllocationAbort(record *dsv1.BattleAllocationAbortStorageRecord) ([]byte, error) {
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
	if err != nil {
		return nil, errcode.NewCause(errcode.ErrInternal, err, "marshal battle allocation abort journal")
	}
	return payload, nil
}

func allocationAbortTerminalProofsMatch(
	ctx context.Context,
	tx redis.Cmdable,
	request battleabort.Request,
	teardownKey, lifecycleKey string,
) (bool, error) {
	source := BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
	}
	teardown, teardownFound, err := readTeardownProof(ctx, tx, request.MatchID, source)
	if err != nil {
		return false, err
	}
	marker, markerFound, err := readAllocationLifecyclePublished(ctx, tx, request.MatchID, lifecycleKey)
	if err != nil {
		return false, err
	}
	return teardownFound && departureSourceEqualsTeardown(source, teardown) && markerFound &&
		lifecyclePublishedRecordMatches(marker, request.MatchID, request.Target), nil
}

func newAllocationAbortRecord(request battleabort.Request, requestedAtMs, releasedAtMs int64) *dsv1.BattleAllocationAbortStorageRecord {
	return &dsv1.BattleAllocationAbortStorageRecord{
		MatchId: request.MatchID, AllocationOperationId: request.OperationID,
		DsPodName: request.Target.PodName, GameserverUid: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationId: request.Target.AllocationID,
		ReleaseTrack: request.Target.ReleaseTrack, RequestedAtMs: requestedAtMs,
		ReleasedAtMs: releasedAtMs,
	}
}

// FenceAllocationAbortExpected is the only linearization point before the
// external UID-conditioned delete.  It atomically locks auth+battle into a
// permanent non-routable state and creates an immutable operation+instance
// journal. A missing record, a terminal state, any tuple drift, or an already
// admitted player all fail closed.
func (r *RedisBattleAuthRepo) FenceAllocationAbortExpected(
	ctx context.Context,
	request battleabort.Request,
) (BattleAllocationAbortFenceResult, error) {
	if !request.Complete() {
		return BattleAllocationAbortFenceResult{}, errcode.New(errcode.ErrInvalidArg,
			"complete battle allocation abort request required")
	}
	aKey, bKey, jKey := battleAuthKey(request.MatchID), battleKey(request.MatchID), battleAllocationAbortKey(request.MatchID)
	source := BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
	}
	tKey := battleInstanceTeardownKey(request.MatchID, source)
	lKey := battleAllocationLifecyclePublishedKey(request.MatchID)
	for attempt := 0; attempt < battleAuthCASRetries; attempt++ {
		result := BattleAllocationAbortFenceResult{}
		var bizErr error
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			record, found, err := readAllocationAbortRecord(ctx, tx, request.MatchID, jKey)
			if err != nil {
				return err
			}
			if found {
				if !abortRecordMatchesRequest(record, request) {
					bizErr = errcode.New(errcode.ErrInvalidState,
						"battle %d allocation abort idempotency tuple conflict", request.MatchID)
					return bizErr
				}
				if record.GetReleasedAtMs() > 0 {
					result.Released = true
					return nil
				}
				terminal, terminalErr := allocationAbortTerminalProofsMatch(
					ctx, tx, request, tKey, lKey)
				if terminalErr != nil {
					return terminalErr
				}
				if terminal {
					record.ReleasedAtMs = r.now().UnixMilli()
					payload, marshalErr := marshalAllocationAbort(record)
					if marshalErr != nil {
						return marshalErr
					}
					_, writeErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
						pipe.Set(ctx, jKey, payload, 0)
						return nil
					})
					if writeErr == nil {
						result.Released = true
					}
					return writeErr
				}
				authRecord, battle, readErr := readBoundAuthority(tx, ctx, request.MatchID, aKey, bKey)
				if readErr != nil || !allocationAbortFenceMatches(authRecord, battle, request) {
					if readErr != nil {
						return readErr
					}
					bizErr = errcode.New(errcode.ErrInvalidState,
						"battle %d allocation abort fence changed", request.MatchID)
					return bizErr
				}
				result.Battle = proto.Clone(battle).(*dsv1.BattleStorageRecord)
				return nil
			}

			// A stale/empty-match terminal worker may have completed before the
			// Matchmaker observes its placement conflict.  Only the conjunction of
			// the exact permanent UID+Pod teardown proof and exact full-target Kafka
			// ACK marker can close that race. Neither a terminal state, missing TTL
			// record, nor either proof by itself is enough.
			terminal, terminalErr := allocationAbortTerminalProofsMatch(
				ctx, tx, request, tKey, lKey)
			if terminalErr != nil {
				return terminalErr
			}
			if terminal {
				nowMs := r.now().UnixMilli()
				record = newAllocationAbortRecord(request, nowMs, nowMs)
				payload, marshalErr := marshalAllocationAbort(record)
				if marshalErr != nil {
					return marshalErr
				}
				_, writeErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.Set(ctx, jKey, payload, 0)
					return nil
				})
				if writeErr == nil {
					result.Released = true
				}
				return writeErr
			}

			authRecord, battle, err := readBoundAuthority(tx, ctx, request.MatchID, aKey, bKey)
			if err != nil {
				return err
			}
			battleBefore := proto.Clone(battle).(*dsv1.BattleStorageRecord)
			expected := BattleExpectedInstance{
				AllocationID: request.Target.AllocationID, InstanceUID: request.Target.InstanceUID,
				InstanceEpoch: request.Target.InstanceEpoch,
			}
			if !battleResultStableAuthorityMatches(authRecord, battle, expected) ||
				(battle.GetState() != "ready" && battle.GetState() != "running") ||
				battle.GetDsPodName() != request.Target.PodName ||
				battle.GetReleaseTrack() != request.Target.ReleaseTrack || battle.GetPlayerCount() != 0 {
				bizErr = errcode.New(errcode.ErrInvalidState,
					"battle %d is not an exact zero-admission abort target", request.MatchID)
				return bizErr
			}

			nowMs := r.now().UnixMilli()
			record = newAllocationAbortRecord(request, nowMs, 0)
			journalPayload, err := marshalAllocationAbort(record)
			if err != nil {
				return err
			}
			authRecord.Phase = dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING
			authRecord.Pending = nil
			authRecord.PendingStartedMs = 0
			authRecord.DeliveredRv = ""
			authRecord.UpdatedAtMs = nowMs
			authPayload, err := proto.MarshalOptions{Deterministic: true}.Marshal(authRecord)
			if err != nil {
				return err
			}
			battle.State = BattleStateAllocationAbortPending
			// Make the derived active index immediately eligible for recovery. The
			// canonical reconciler will repair a lost cross-slot ZADD from this value.
			battle.LastHeartbeatMs = 0
			battlePayload, err := r.marshalBattleTransition(battleBefore, battle)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, aKey, authPayload, 0)
				pipe.Set(ctx, bKey, battlePayload, 0)
				pipe.Set(ctx, jKey, journalPayload, 0)
				return nil
			})
			if err == nil {
				result.Battle = proto.Clone(battle).(*dsv1.BattleStorageRecord)
			}
			return err
		}, aKey, bKey, jKey, tKey, lKey)
		if txErr == redis.TxFailedErr {
			continue
		}
		if txErr != nil {
			if bizErr != nil {
				return BattleAllocationAbortFenceResult{}, bizErr
			}
			return BattleAllocationAbortFenceResult{}, txErr
		}
		if result.Released {
			return result, nil
		}
		if result.Battle == nil {
			return BattleAllocationAbortFenceResult{}, errcode.New(errcode.ErrUnavailable,
				"battle %d allocation abort fence result unavailable", request.MatchID)
		}
		if err := r.rdb.ZAdd(ctx, activeKey, redis.Z{Score: 0, Member: request.MatchID}).Err(); err != nil {
			return result, err
		}
		return result, nil
	}
	return BattleAllocationAbortFenceResult{}, errcode.New(errcode.ErrInternal,
		"battle %d allocation abort fence cas retry exhausted", request.MatchID)
}

func allocationAbortFenceMatches(authRecord *dsv1.BattleDSAuthStorageRecord, battle *dsv1.BattleStorageRecord, request battleabort.Request) bool {
	expected := BattleExpectedInstance{
		AllocationID: request.Target.AllocationID, InstanceUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch,
	}
	return request.Complete() && battleAuthRecordV2Exact(authRecord) &&
		expectedBattleInstanceMatches(authRecord, battle, expected) &&
		authRecord.GetPhase() == dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING &&
		battle.GetState() == BattleStateAllocationAbortPending &&
		battle.GetDsPodName() == request.Target.PodName &&
		battle.GetReleaseTrack() == request.Target.ReleaseTrack
}

func allocationAbortCompletionAuthorityMatches(
	authRecord *dsv1.BattleDSAuthStorageRecord,
	battle *dsv1.BattleStorageRecord,
	request battleabort.Request,
) bool {
	if allocationAbortFenceMatches(authRecord, battle, request) {
		return true
	}
	expected := BattleExpectedInstance{
		AllocationID: request.Target.AllocationID, InstanceUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch,
	}
	return request.Complete() && battleAuthRecordV2Exact(authRecord) &&
		expectedBattleInstanceMatches(authRecord, battle, expected) &&
		authRecord.GetPhase() == dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING &&
		battle.GetState() == "abandoned" && battle.GetDsPodName() == request.Target.PodName &&
		battle.GetReleaseTrack() == request.Target.ReleaseTrack
}

func allocationAbortReleasedAuthMatches(
	authRecord *dsv1.BattleDSAuthStorageRecord,
	request battleabort.Request,
) bool {
	return request.Complete() && battleAuthRecordV2Exact(authRecord) &&
		authRecord.GetMatchId() == request.MatchID &&
		authRecord.GetAllocationId() == request.Target.AllocationID &&
		authRecord.GetDsPodName() == request.Target.PodName &&
		authRecord.GetInstanceUid() == request.Target.InstanceUID &&
		authRecord.GetInstanceEpoch() == request.Target.InstanceEpoch &&
		authRecord.GetPhase() == dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING
}

func allocationAbortReleasedBattleMatches(
	battle *dsv1.BattleStorageRecord,
	request battleabort.Request,
) bool {
	return request.Complete() && battle != nil && battle.GetMatchId() == request.MatchID &&
		battle.GetState() == "abandoned" && battle.GetDsPodName() == request.Target.PodName &&
		battle.GetGameserverUid() == request.Target.InstanceUID &&
		battle.GetInstanceEpoch() == request.Target.InstanceEpoch &&
		battle.GetAllocationId() == request.Target.AllocationID &&
		battle.GetReleaseTrack() == request.Target.ReleaseTrack
}

// ReadAllocationAbort returns the permanent operation journal used by the
// service-lifecycle reconciler after a process crash.
func (r *RedisBattleAuthRepo) ReadAllocationAbort(
	ctx context.Context,
	matchID uint64,
) (battleabort.Request, bool, bool, error) {
	if matchID == 0 {
		return battleabort.Request{}, false, false, errcode.New(errcode.ErrInvalidArg, "match_id required")
	}
	record, found, err := readAllocationAbortRecord(ctx, r.rdb, matchID, battleAllocationAbortKey(matchID))
	if err != nil || !found {
		return battleabort.Request{}, false, found, err
	}
	return abortRequestFromRecord(record), record.GetReleasedAtMs() > 0, true, nil
}

// CompleteAllocationAbortExpected is called only after UID deletion and the
// lifecycle publish have both returned a definite ACK. It records RELEASED in
// the permanent journal and only then gives auth/battle bounded audit TTLs.
func (r *RedisBattleAuthRepo) CompleteAllocationAbortExpected(
	ctx context.Context,
	request battleabort.Request,
	authTTL, battleTTL time.Duration,
) (bool, error) {
	if !request.Complete() || authTTL <= 0 || battleTTL <= 0 {
		return false, errcode.New(errcode.ErrInvalidArg,
			"complete battle allocation abort and positive retention required")
	}
	aKey, bKey, jKey := battleAuthKey(request.MatchID), battleKey(request.MatchID), battleAllocationAbortKey(request.MatchID)
	source := BattleDepartureSource{
		DSPodName: request.Target.PodName, GameServerUID: request.Target.InstanceUID,
		InstanceEpoch: request.Target.InstanceEpoch, AllocationID: request.Target.AllocationID,
	}
	tKey := battleInstanceTeardownKey(request.MatchID, source)
	lKey := battleAllocationLifecyclePublishedKey(request.MatchID)
	for attempt := 0; attempt < battleAuthCASRetries; attempt++ {
		completed := false
		var bizErr error
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			record, found, err := readAllocationAbortRecord(ctx, tx, request.MatchID, jKey)
			if err != nil {
				return err
			}
			if !found || !abortRecordMatchesRequest(record, request) {
				bizErr = errcode.New(errcode.ErrInvalidState,
					"battle %d allocation abort journal missing or changed", request.MatchID)
				return bizErr
			}
			if record.GetReleasedAtMs() > 0 {
				// RELEASED is the durable external-effect ACK, not proof that the
				// local retention cleanup also committed. A crash can happen after a
				// retry recognizes the permanent teardown+lifecycle proofs but before
				// auth/battle receive bounded TTLs. Reconcile each remaining exact key
				// independently; missing keys are already clean, while a mismatched key
				// is never touched (it may belong to a later authority incarnation).
				authRaw, authErr := tx.Get(ctx, aKey).Bytes()
				if authErr != nil && authErr != redis.Nil {
					return authErr
				}
				battleRaw, battleErr := tx.Get(ctx, bKey).Bytes()
				if battleErr != nil && battleErr != redis.Nil {
					return battleErr
				}
				var authRecord *dsv1.BattleDSAuthStorageRecord
				if authErr == nil {
					authRecord = &dsv1.BattleDSAuthStorageRecord{}
					if decodeErr := unmarshalBattleAuth(request.MatchID, authRaw, authRecord); decodeErr != nil {
						return decodeErr
					}
				}
				var battle *dsv1.BattleStorageRecord
				if battleErr == nil {
					var decodeErr error
					battle, decodeErr = unmarshalBattle(request.MatchID, battleRaw)
					if decodeErr != nil {
						return decodeErr
					}
				}
				authExact := authRecord != nil && allocationAbortReleasedAuthMatches(authRecord, request)
				battleExact := battle != nil && allocationAbortReleasedBattleMatches(battle, request)
				if (authRecord != nil && !authExact) || (battle != nil && !battleExact) {
					bizErr = errcode.New(errcode.ErrInvalidState,
						"battle %d released abort found a different remaining authority", request.MatchID)
					return bizErr
				}
				if battleExact && r.StrictModelBWritesEnabled() {
					if validateErr := validateBattleStorageTransition(battle, battle); validateErr != nil {
						bizErr = errcode.NewCause(errcode.ErrInvalidState, validateErr,
							"battle %d released abort storage invariant failed", request.MatchID)
						return bizErr
					}
				}
				if authExact || battleExact {
					_, cleanupErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
						if authExact {
							pipe.Expire(ctx, aKey, authTTL)
						}
						if battleExact {
							pipe.Set(ctx, bKey, battleRaw, battleTTL)
						}
						return nil
					})
					if cleanupErr != nil {
						return cleanupErr
					}
				}
				completed = true
				return nil
			}
			terminal, terminalErr := allocationAbortTerminalProofsMatch(
				ctx, tx, request, tKey, lKey)
			if terminalErr != nil {
				return terminalErr
			}
			if !terminal {
				bizErr = errcode.New(errcode.ErrInvalidState,
					"battle %d allocation abort completion lacks teardown/lifecycle proofs", request.MatchID)
				return bizErr
			}
			authRecord, battle, err := readBoundAuthority(tx, ctx, request.MatchID, aKey, bKey)
			if err != nil {
				return err
			}
			battleBefore := proto.Clone(battle).(*dsv1.BattleStorageRecord)
			if !allocationAbortCompletionAuthorityMatches(authRecord, battle, request) {
				bizErr = errcode.New(errcode.ErrInvalidState,
					"battle %d allocation abort fence changed before completion", request.MatchID)
				return bizErr
			}
			aTTL, err := tx.PTTL(ctx, aKey).Result()
			if err != nil || aTTL != -1 {
				if err == nil {
					bizErr = errcode.New(errcode.ErrInvalidState, "battle %d abort auth fence is not permanent", request.MatchID)
					return bizErr
				}
				return err
			}
			bTTL, err := tx.PTTL(ctx, bKey).Result()
			if err != nil || bTTL != -1 {
				if err == nil {
					bizErr = errcode.New(errcode.ErrInvalidState, "battle %d abort battle fence is not permanent", request.MatchID)
					return bizErr
				}
				return err
			}

			nowMs := r.now().UnixMilli()
			record.ReleasedAtMs = nowMs
			journalPayload, err := marshalAllocationAbort(record)
			if err != nil {
				return err
			}
			battle.State = "abandoned"
			battlePayload, err := r.marshalBattleTransition(battleBefore, battle)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, jKey, journalPayload, 0)
				pipe.Set(ctx, bKey, battlePayload, battleTTL)
				pipe.Expire(ctx, aKey, authTTL)
				return nil
			})
			completed = err == nil
			return err
		}, aKey, bKey, jKey, tKey, lKey)
		if txErr == redis.TxFailedErr {
			continue
		}
		if txErr != nil {
			if bizErr != nil {
				return false, bizErr
			}
			return false, txErr
		}
		if !completed {
			return false, nil
		}
		if err := r.rdb.ZRem(ctx, activeKey, request.MatchID).Err(); err != nil {
			return true, err
		}
		return true, nil
	}
	return false, errcode.New(errcode.ErrInternal,
		"battle %d allocation abort completion cas retry exhausted", request.MatchID)
}
