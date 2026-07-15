package data

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

func departureFixture(t *testing.T, matchID uint64) (*RedisBattleRepo, BattleDepartureSource) {
	t.Helper()
	repo, _ := newRepo(t)
	battle := sampleBattle(matchID, time.Now().UnixMilli())
	battle.DsPodName = "battle-exact"
	battle.GameserverUid = "uid-exact"
	battle.InstanceEpoch = 7
	battle.AllocationId = "alloc-exact"
	if err := repo.CreateBattle(context.Background(), battle, testTTL); err != nil {
		t.Fatalf("seed battle: %v", err)
	}
	return repo, BattleDepartureSource{
		DSPodName: battle.DsPodName, GameServerUID: battle.GameserverUid,
		InstanceEpoch: battle.InstanceEpoch, AllocationID: battle.AllocationId,
	}
}

func expectedDeparture(matchID, playerID uint64, operationID string, source BattleDepartureSource) BattlePlayerDepartureExpected {
	return BattlePlayerDepartureExpected{
		MatchID: matchID, PlayerID: playerID, PlacementVersion: 17,
		OperationID: operationID, Source: source,
	}
}

func TestBattleDepartureRequiresDurableSourceProofWhenBattleMissing(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	source := BattleDepartureSource{
		DSPodName: "battle-gone", GameServerUID: "uid-gone", InstanceEpoch: 2, AllocationID: "alloc-gone",
	}
	_, err := repo.EnsurePlayerDeparture(ctx, expectedDeparture(901, 10, "op-901", source))
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("missing battle without teardown err=%v code=%v", err, errcode.As(err))
	}
	if mr.Exists(battleDepartureJournalKey(901)) {
		t.Fatal("missing source without teardown must have zero journal side effects")
	}
}

func TestBattleDepartureHeartbeatOrderThenExactAbsenceCommits(t *testing.T) {
	ctx := context.Background()
	const matchID uint64 = 902
	repo, source := departureFixture(t, matchID)
	expected := expectedDeparture(matchID, 10, "placement-op-902", source)

	first, err := repo.EnsurePlayerDeparture(ctx, expected)
	if err != nil || first.Departed || first.Status != dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_PENDING {
		t.Fatalf("ensure pending=%+v err=%v", first, err)
	}
	orders, err := repo.ReconcilePlayerDepartures(ctx, matchID, source, true, []uint64{10, 20, 30}, nil)
	if err != nil || len(orders) != 1 || orders[0].GetPlayerId() != 10 || orders[0].GetDepartureId() == "" {
		t.Fatalf("first heartbeat orders=%+v err=%v", orders, err)
	}

	// DS 踢人后的下一份完整快照不再含 player=10；ACK 与缺席双重一致。
	orders, err = repo.ReconcilePlayerDepartures(ctx, matchID, source, true,
		[]uint64{20, 30}, []string{orders[0].GetDepartureId()})
	if err != nil || len(orders) != 0 {
		t.Fatalf("departure commit orders=%+v err=%v", orders, err)
	}
	final, err := repo.EnsurePlayerDeparture(ctx, expected)
	if err != nil || !final.Departed || final.Status != dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_DEPARTED {
		t.Fatalf("final=%+v err=%v", final, err)
	}
}

func TestBattleDepartureOldHeartbeatCannotInterpretEmptyAsDeparture(t *testing.T) {
	ctx := context.Background()
	const matchID uint64 = 903
	repo, source := departureFixture(t, matchID)
	expected := expectedDeparture(matchID, 10, "placement-op-903", source)
	if _, err := repo.EnsurePlayerDeparture(ctx, expected); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// 旧 DS 没有 snapshot_present；repeated 零值不得被解释为空服。
	orders, err := repo.ReconcilePlayerDepartures(ctx, matchID, source, false, nil, nil)
	if err != nil || len(orders) != 1 {
		t.Fatalf("legacy heartbeat orders=%+v err=%v", orders, err)
	}
	stillPending, err := repo.EnsurePlayerDeparture(ctx, expected)
	if err != nil || stillPending.Departed {
		t.Fatalf("legacy empty snapshot falsely departed: %+v err=%v", stillPending, err)
	}
}

func TestBattleDeparturePlacementVersionFencesABA(t *testing.T) {
	ctx := context.Background()
	const matchID uint64 = 907
	repo, source := departureFixture(t, matchID)
	old := expectedDeparture(matchID, 10, "same-operation-name", source)
	if _, err := repo.EnsurePlayerDeparture(ctx, old); err != nil {
		t.Fatalf("ensure old: %v", err)
	}
	if _, err := repo.ReconcilePlayerDepartures(ctx, matchID, source, true, []uint64{20, 30}, nil); err != nil {
		t.Fatalf("depart old: %v", err)
	}
	oldResult, err := repo.EnsurePlayerDeparture(ctx, old)
	if err != nil || !oldResult.Departed {
		t.Fatalf("old result=%+v err=%v", oldResult, err)
	}

	// 同 match/player/op 名在新 placement version 下是全新 journal identity。玩家
	// 已重连回源 Battle 时，旧 version 的 departed 不得让新轮直接通过。
	newer := old
	newer.PlacementVersion++
	newResult, err := repo.EnsurePlayerDeparture(ctx, newer)
	if err != nil || newResult.Departed ||
		newResult.Status != dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_PENDING {
		t.Fatalf("new version inherited old departure: %+v err=%v", newResult, err)
	}
	orders, err := repo.ReconcilePlayerDepartures(ctx, matchID, source, true, []uint64{10, 20, 30}, nil)
	if err != nil || len(orders) != 1 || orders[0].GetPlacementVersion() != newer.PlacementVersion {
		t.Fatalf("new version order=%+v err=%v", orders, err)
	}
}

func TestBattleDepartureRejectsAckWhilePlayerStillActiveWithoutMutation(t *testing.T) {
	ctx := context.Background()
	const matchID uint64 = 904
	repo, source := departureFixture(t, matchID)
	expected := expectedDeparture(matchID, 10, "placement-op-904", source)
	if _, err := repo.EnsurePlayerDeparture(ctx, expected); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	orders, err := repo.ReconcilePlayerDepartures(ctx, matchID, source, true, []uint64{10, 20, 30}, nil)
	if err != nil || len(orders) != 1 {
		t.Fatalf("issue: orders=%+v err=%v", orders, err)
	}
	if _, err := repo.ReconcilePlayerDepartures(ctx, matchID, source, true,
		[]uint64{10, 20, 30}, []string{orders[0].GetDepartureId()}); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("ack while active err=%v code=%v", err, errcode.As(err))
	}
	stillPending, err := repo.EnsurePlayerDeparture(ctx, expected)
	if err != nil || stillPending.Departed {
		t.Fatalf("rejected ack mutated departure: %+v err=%v", stillPending, err)
	}
}

func TestBattleDepartureExactUIDTeardownIsDurableProof(t *testing.T) {
	ctx := context.Background()
	const matchID uint64 = 905
	repo, source := departureFixture(t, matchID)
	first := expectedDeparture(matchID, 10, "placement-op-905-a", source)
	if _, err := repo.EnsurePlayerDeparture(ctx, first); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := repo.RecordInstanceTeardown(ctx, matchID, source); err != nil {
		t.Fatalf("record teardown: %v", err)
	}
	if err := repo.RecordInstanceTeardown(ctx, matchID, source); err != nil {
		t.Fatalf("idempotent teardown: %v", err)
	}
	after, err := repo.EnsurePlayerDeparture(ctx, first)
	if err != nil || !after.Departed || after.Status != dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_SOURCE_TORN_DOWN {
		t.Fatalf("pending not closed by teardown: %+v err=%v", after, err)
	}

	// 即使 battle 投影后续被 purge，同 exact UID 的新 placement operation 仍可幂等获证。
	if err := repo.DeleteBattle(ctx, matchID); err != nil {
		t.Fatalf("delete battle: %v", err)
	}
	second, err := repo.EnsurePlayerDeparture(ctx,
		expectedDeparture(matchID, 20, "placement-op-905-b", source))
	if err != nil || !second.Departed || second.Status != dsv1.BattlePlayerDepartureStatus_BATTLE_PLAYER_DEPARTURE_STATUS_SOURCE_TORN_DOWN {
		t.Fatalf("teardown proof after purge=%+v err=%v", second, err)
	}

	wrong := source
	wrong.GameServerUID = "uid-rebuilt"
	if err := repo.RecordInstanceTeardown(ctx, matchID, wrong); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("wrong UID overwrote teardown: err=%v code=%v", err, errcode.As(err))
	}
}

func TestBattleDepartureJournalAndTeardownNeverDependOnTTL(t *testing.T) {
	ctx := context.Background()
	const matchID uint64 = 906
	repo, mr := newRepo(t)
	battle := sampleBattle(matchID, time.Now().UnixMilli())
	battle.DsPodName = "battle-exact"
	battle.GameserverUid = "uid-exact"
	battle.InstanceEpoch = 7
	battle.AllocationId = "alloc-exact"
	if err := repo.CreateBattle(ctx, battle, testTTL); err != nil {
		t.Fatalf("seed battle: %v", err)
	}
	source := BattleDepartureSource{
		DSPodName: "battle-exact", GameServerUID: "uid-exact", InstanceEpoch: 7, AllocationID: "alloc-exact",
	}
	if _, err := repo.EnsurePlayerDeparture(ctx,
		expectedDeparture(matchID, 10, "placement-op-906", source)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if ttl := mr.TTL(battleDepartureJournalKey(matchID)); ttl != 0 {
		t.Fatalf("pending journal TTL=%v want persistent", ttl)
	}
	if err := repo.RecordInstanceTeardown(ctx, matchID, source); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if ttl := mr.TTL(battleInstanceTeardownKey(matchID)); ttl != 0 {
		t.Fatalf("teardown proof TTL=%v want persistent", ttl)
	}
}
