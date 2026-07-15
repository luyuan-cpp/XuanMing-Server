package data

// Battle→Hub 物理离场日志。
//
// 这里刻意不使用 TTL 与 presence 推断：
//   - pending order 只能被 credential-bound Battle DS 的完整 active-player
//     snapshot 提交；
//   - 或者外部 GameServer UID 条件回收明确成功后的 teardown proof 提交。
//
// journal / teardown / battle / auth 都使用 {match_id} hashtag，因此 Redis Cluster
// 下的 WATCH/MULTI 仍是同 slot 事务。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

const (
	battleDepartureCASRetries = 64
	maxBattleDepartureOrders  = 1024
)

func battleDepartureJournalKey(matchID uint64) string {
	return fmt.Sprintf("pandora:ds:departures:{%d}", matchID)
}

func battleInstanceTeardownKey(matchID uint64) string {
	return fmt.Sprintf("pandora:ds:teardown:{%d}", matchID)
}

// BattleDepartureSource 是 placement/source snapshot 与 Battle Redis authority 都必须精确
// 匹配的物理实例栅栏。
type BattleDepartureSource struct {
	DSPodName     string
	GameServerUID string
	InstanceEpoch uint32
	AllocationID  string
}

func (s BattleDepartureSource) valid() bool {
	return s.DSPodName != "" && s.GameServerUID != "" && s.InstanceEpoch > 0 && s.AllocationID != ""
}

// BattlePlayerDepartureExpected 是 EnsurePlayerDeparture 的完整幂等输入。
type BattlePlayerDepartureExpected struct {
	MatchID          uint64
	PlayerID         uint64
	PlacementVersion uint64
	OperationID      string
	Source           BattleDepartureSource
}

// BattlePlayerDepartureResult 区分心跳离场与整个 UID teardown，便于观测但
// 两者都是 Hub ticket/admission 可接受的物理证明。
type BattlePlayerDepartureResult struct {
	Departed bool
	Status   dsv1.BattlePlayerDepartureStatus
}

func stableDepartureID(in BattlePlayerDepartureExpected) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%d\x00%s",
		in.MatchID, in.PlayerID, in.PlacementVersion, in.OperationID)))
	return hex.EncodeToString(sum[:16])
}

func departureSourceEqualsRecord(source BattleDepartureSource, record *dsv1.BattlePlayerDepartureStorageRecord) bool {
	return record != nil && record.GetDsPodName() == source.DSPodName &&
		record.GetGameserverUid() == source.GameServerUID &&
		record.GetInstanceEpoch() == source.InstanceEpoch &&
		record.GetAllocationId() == source.AllocationID
}

func departureSourceEqualsBattle(source BattleDepartureSource, battle *dsv1.BattleStorageRecord) bool {
	return battle != nil && battle.GetDsPodName() == source.DSPodName &&
		battle.GetGameserverUid() == source.GameServerUID &&
		battle.GetInstanceEpoch() == source.InstanceEpoch &&
		battle.GetAllocationId() == source.AllocationID
}

func departureSourceEqualsTeardown(source BattleDepartureSource, proof *dsv1.BattleInstanceTeardownStorageRecord) bool {
	return proof != nil && proof.GetDsPodName() == source.DSPodName &&
		proof.GetGameserverUid() == source.GameServerUID &&
		proof.GetInstanceEpoch() == source.InstanceEpoch &&
		proof.GetAllocationId() == source.AllocationID
}

func departureSourceEqualsSource(a, b BattleDepartureSource) bool {
	return a.DSPodName == b.DSPodName && a.GameServerUID == b.GameServerUID &&
		a.InstanceEpoch == b.InstanceEpoch && a.AllocationID == b.AllocationID
}

func battleContainsPlayer(battle *dsv1.BattleStorageRecord, playerID uint64) bool {
	for _, candidate := range battle.GetPlayerIds() {
		if candidate == playerID {
			return true
		}
	}
	return false
}

func readDepartureJournal(ctx context.Context, tx redis.Cmdable, matchID uint64) (*dsv1.BattlePlayerDepartureJournalStorageRecord, error) {
	payload, err := tx.Get(ctx, battleDepartureJournalKey(matchID)).Bytes()
	if err == redis.Nil {
		return &dsv1.BattlePlayerDepartureJournalStorageRecord{MatchId: matchID}, nil
	}
	if err != nil {
		return nil, err
	}
	journal := &dsv1.BattlePlayerDepartureJournalStorageRecord{}
	if err := proto.Unmarshal(payload, journal); err != nil {
		return nil, errcode.NewCause(errcode.ErrInternal, err,
			"decode battle departure journal %d", matchID)
	}
	if journal.GetMatchId() != matchID {
		return nil, errcode.New(errcode.ErrInvalidState,
			"battle departure journal match mismatch: got %d want %d", journal.GetMatchId(), matchID)
	}
	return journal, nil
}

func readTeardownProof(ctx context.Context, tx redis.Cmdable, matchID uint64) (*dsv1.BattleInstanceTeardownStorageRecord, bool, error) {
	payload, err := tx.Get(ctx, battleInstanceTeardownKey(matchID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	proof := &dsv1.BattleInstanceTeardownStorageRecord{}
	if err := proto.Unmarshal(payload, proof); err != nil {
		return nil, false, errcode.NewCause(errcode.ErrInternal, err,
			"decode battle teardown proof %d", matchID)
	}
	if proof.GetMatchId() != matchID {
		return nil, false, errcode.New(errcode.ErrInvalidState,
			"battle teardown proof match mismatch: got %d want %d", proof.GetMatchId(), matchID)
	}
	return proof, true, nil
}

func marshalDepartureMessage(message proto.Message) ([]byte, error) {
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return nil, errcode.NewCause(errcode.ErrInternal, err, "marshal battle departure record")
	}
	return payload, nil
}

// EnsurePlayerDeparture 只创建/查询 exact pending order。battle key 缺失并不是
// departure proof；没有 durable teardown 时必须返回可重试 unavailable。
func (r *RedisBattleRepo) EnsurePlayerDeparture(
	ctx context.Context,
	expected BattlePlayerDepartureExpected,
) (BattlePlayerDepartureResult, error) {
	if expected.MatchID == 0 || expected.PlayerID == 0 || expected.PlacementVersion == 0 ||
		expected.OperationID == "" || !expected.Source.valid() {
		return BattlePlayerDepartureResult{}, errcode.New(errcode.ErrInvalidArg,
			"complete battle departure operation and source tuple required")
	}
	departureID := stableDepartureID(expected)
	jKey := battleDepartureJournalKey(expected.MatchID)
	tKey := battleInstanceTeardownKey(expected.MatchID)
	bKey := battleKey(expected.MatchID)

	for attempt := 0; attempt < battleDepartureCASRetries; attempt++ {
		var result BattlePlayerDepartureResult
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			journal, err := readDepartureJournal(ctx, tx, expected.MatchID)
			if err != nil {
				return err
			}
			proof, proofFound, err := readTeardownProof(ctx, tx, expected.MatchID)
			if err != nil {
				return err
			}

			var existing *dsv1.BattlePlayerDepartureStorageRecord
			for _, order := range journal.GetDepartures() {
				if order.GetDepartureId() == departureID {
					existing = order
					break
				}
			}
			if existing != nil {
				if existing.GetMatchId() != expected.MatchID || existing.GetPlayerId() != expected.PlayerID ||
					existing.GetOperationId() != expected.OperationID ||
					existing.GetPlacementVersion() != expected.PlacementVersion ||
					!departureSourceEqualsRecord(expected.Source, existing) {
					return errcode.New(errcode.ErrInvalidState,
						"battle departure idempotency tuple conflict")
				}
				if existing.GetStatus() == dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_DEPARTED ||
					existing.GetStatus() == dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_SOURCE_TORN_DOWN {
					result = BattlePlayerDepartureResult{Departed: true, Status: existing.GetStatus()}
					return nil
				}
				if proofFound && departureSourceEqualsTeardown(expected.Source, proof) {
					existing.Status = dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_SOURCE_TORN_DOWN
					existing.DepartedAtMs = proof.GetTornDownAtMs()
					payload, err := marshalDepartureMessage(journal)
					if err != nil {
						return err
					}
					_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
						pipe.Set(ctx, jKey, payload, 0)
						return nil
					})
					if err == nil {
						result = BattlePlayerDepartureResult{Departed: true, Status: existing.GetStatus()}
					}
					return err
				}
				result = BattlePlayerDepartureResult{Status: dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_PENDING}
				return nil
			}

			if len(journal.GetDepartures()) >= maxBattleDepartureOrders {
				return errcode.New(errcode.ErrRateLimited,
					"battle %d departure journal full", expected.MatchID)
			}
			nowMs := time.Now().UnixMilli()
			order := &dsv1.BattlePlayerDepartureStorageRecord{
				DepartureId: departureID, MatchId: expected.MatchID, PlayerId: expected.PlayerID,
				PlacementVersion: expected.PlacementVersion,
				OperationId:      expected.OperationID, DsPodName: expected.Source.DSPodName,
				GameserverUid: expected.Source.GameServerUID, InstanceEpoch: expected.Source.InstanceEpoch,
				AllocationId: expected.Source.AllocationID, RequestedAtMs: nowMs,
				Status: dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_PENDING,
			}
			if proofFound && departureSourceEqualsTeardown(expected.Source, proof) {
				order.Status = dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_SOURCE_TORN_DOWN
				order.DepartedAtMs = proof.GetTornDownAtMs()
				result = BattlePlayerDepartureResult{Departed: true, Status: order.GetStatus()}
			} else {
				battlePayload, getErr := tx.Get(ctx, bKey).Bytes()
				if getErr == redis.Nil {
					return errcode.New(errcode.ErrUnavailable,
						"battle %d source missing without exact teardown proof", expected.MatchID)
				}
				if getErr != nil {
					return getErr
				}
				battle, decodeErr := unmarshalBattle(expected.MatchID, battlePayload)
				if decodeErr != nil {
					return decodeErr
				}
				if !departureSourceEqualsBattle(expected.Source, battle) {
					return errcode.New(errcode.ErrInvalidState,
						"battle %d source tuple mismatch", expected.MatchID)
				}
				if !battleContainsPlayer(battle, expected.PlayerID) {
					return errcode.New(errcode.ErrInvalidState,
						"player %d not in battle %d authoritative roster", expected.PlayerID, expected.MatchID)
				}
				result = BattlePlayerDepartureResult{Status: order.GetStatus()}
			}
			journal.Departures = append(journal.Departures, order)
			payload, err := marshalDepartureMessage(journal)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, jKey, payload, 0)
				return nil
			})
			return err
		}, jKey, tKey, bKey)
		if err == redis.TxFailedErr {
			continue
		}
		return result, err
	}
	return BattlePlayerDepartureResult{}, errcode.New(errcode.ErrDSAllocationFailed,
		"battle %d departure CAS retry exhausted", expected.MatchID)
}

// ReconcilePlayerDepartures 只在 Heartbeat 已通过 active/pending credential 验证后调用。
func (r *RedisBattleRepo) ReconcilePlayerDepartures(
	ctx context.Context,
	matchID uint64,
	source BattleDepartureSource,
	snapshotPresent bool,
	activePlayerIDs []uint64,
	acknowledgedDepartureIDs []string,
) ([]*dsv1.BattleEvictionOrder, error) {
	if matchID == 0 || !source.valid() {
		return nil, errcode.New(errcode.ErrInvalidArg, "complete battle heartbeat source tuple required")
	}
	active := make(map[uint64]struct{}, len(activePlayerIDs))
	for _, playerID := range activePlayerIDs {
		if playerID == 0 {
			return nil, errcode.New(errcode.ErrInvalidArg, "active player snapshot contains zero player")
		}
		if _, duplicate := active[playerID]; duplicate {
			return nil, errcode.New(errcode.ErrInvalidArg, "active player snapshot contains duplicate player %d", playerID)
		}
		active[playerID] = struct{}{}
	}
	acked := make(map[string]struct{}, len(acknowledgedDepartureIDs))
	for _, departureID := range acknowledgedDepartureIDs {
		if departureID == "" {
			return nil, errcode.New(errcode.ErrInvalidArg, "empty acknowledged departure id")
		}
		if _, duplicate := acked[departureID]; duplicate {
			return nil, errcode.New(errcode.ErrInvalidArg, "duplicate acknowledged departure id")
		}
		acked[departureID] = struct{}{}
	}
	if !snapshotPresent && (len(active) != 0 || len(acked) != 0) {
		return nil, errcode.New(errcode.ErrInvalidArg,
			"battle active player snapshot payload requires present=true")
	}

	jKey := battleDepartureJournalKey(matchID)
	for attempt := 0; attempt < battleDepartureCASRetries; attempt++ {
		var orders []*dsv1.BattleEvictionOrder
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			orders = nil
			journal, err := readDepartureJournal(ctx, tx, matchID)
			if err != nil {
				return err
			}
			changed := false
			knownAcks := make(map[string]struct{}, len(acked))
			nowMs := time.Now().UnixMilli()
			for _, order := range journal.GetDepartures() {
				if order.GetMatchId() != matchID || !departureSourceEqualsRecord(source, order) {
					continue
				}
				if _, present := acked[order.GetDepartureId()]; present {
					knownAcks[order.GetDepartureId()] = struct{}{}
					if _, stillActive := active[order.GetPlayerId()]; stillActive {
						return errcode.New(errcode.ErrInvalidState,
							"departure %s acknowledged while player %d remains active",
							order.GetDepartureId(), order.GetPlayerId())
					}
				}
				if order.GetStatus() != dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_PENDING {
					continue
				}
				if snapshotPresent {
					if _, stillActive := active[order.GetPlayerId()]; !stillActive {
						order.Status = dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_DEPARTED
						order.DepartedAtMs = nowMs
						changed = true
						continue
					}
				}
				if order.GetIssuedAtMs() == 0 {
					order.IssuedAtMs = nowMs
					changed = true
				}
				orders = append(orders, &dsv1.BattleEvictionOrder{
					DepartureId: order.GetDepartureId(), MatchId: order.GetMatchId(),
					PlayerId: order.GetPlayerId(), DsPodName: order.GetDsPodName(),
					GameserverUid: order.GetGameserverUid(), InstanceEpoch: order.GetInstanceEpoch(),
					AllocationId: order.GetAllocationId(), PlacementVersion: order.GetPlacementVersion(),
				})
			}
			if snapshotPresent && len(knownAcks) != len(acked) {
				return errcode.New(errcode.ErrInvalidArg, "heartbeat acknowledged unknown departure id")
			}
			if !changed {
				return nil
			}
			payload, err := marshalDepartureMessage(journal)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, jKey, payload, 0)
				return nil
			})
			return err
		}, jKey)
		if err == redis.TxFailedErr {
			continue
		}
		return orders, err
	}
	return nil, errcode.New(errcode.ErrDSAllocationFailed,
		"battle %d departure reconcile CAS retry exhausted", matchID)
}

// RecordInstanceTeardown 必须在 ReleaseExpected 已明确成功之后调用。它先写
// durable proof 再允许上层 purge battle/auth；若 Redis 回包不确定，上层保留
// release fence 并重试 UID 条件回收，404 幂等成功后可重建证明。
func (r *RedisBattleRepo) RecordInstanceTeardown(
	ctx context.Context,
	matchID uint64,
	source BattleDepartureSource,
) error {
	if matchID == 0 || !source.valid() {
		return errcode.New(errcode.ErrInvalidArg, "complete battle teardown source tuple required")
	}
	jKey := battleDepartureJournalKey(matchID)
	tKey := battleInstanceTeardownKey(matchID)
	for attempt := 0; attempt < battleDepartureCASRetries; attempt++ {
		err := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			journal, err := readDepartureJournal(ctx, tx, matchID)
			if err != nil {
				return err
			}
			existing, found, err := readTeardownProof(ctx, tx, matchID)
			if err != nil {
				return err
			}
			if found && !departureSourceEqualsTeardown(source, existing) {
				return errcode.New(errcode.ErrInvalidState,
					"battle %d teardown proof tuple conflict", matchID)
			}
			nowMs := time.Now().UnixMilli()
			proof := existing
			if proof == nil {
				proof = &dsv1.BattleInstanceTeardownStorageRecord{
					MatchId: matchID, DsPodName: source.DSPodName, GameserverUid: source.GameServerUID,
					InstanceEpoch: source.InstanceEpoch, AllocationId: source.AllocationID,
					TornDownAtMs: nowMs,
				}
			}
			journalChanged := false
			for _, order := range journal.GetDepartures() {
				if order.GetStatus() == dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_PENDING &&
					departureSourceEqualsRecord(source, order) {
					order.Status = dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_SOURCE_TORN_DOWN
					order.DepartedAtMs = proof.GetTornDownAtMs()
					journalChanged = true
				}
			}
			if found && !journalChanged {
				return nil
			}
			proofPayload, err := marshalDepartureMessage(proof)
			if err != nil {
				return err
			}
			var journalPayload []byte
			if journalChanged {
				journalPayload, err = marshalDepartureMessage(journal)
				if err != nil {
					return err
				}
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, tKey, proofPayload, 0)
				if journalChanged {
					pipe.Set(ctx, jKey, journalPayload, 0)
				}
				return nil
			})
			return err
		}, jKey, tKey)
		if err == redis.TxFailedErr {
			continue
		}
		return err
	}
	return errcode.New(errcode.ErrDSAllocationFailed,
		"battle %d teardown proof CAS retry exhausted", matchID)
}
