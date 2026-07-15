package data

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

func TestBattleTerminalFenceSharesPlayerSlotAndBlocksPlacementCAS(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := NewRedisPlacementRepo(rdb)
	ctx := context.Background()
	rec := &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 42, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         3,
	}
	payload, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, PlacementKey(42), payload, 0).Err(); err != nil {
		t.Fatal(err)
	}
	fenceKey := placement.BattleTerminalFenceKey(42, 700)
	// Deliberately malformed tombstones still fail closed. The trusted relay will
	// reject rather than overwrite this shape, but an old match must never be
	// allowed to exploit corrupted terminal authority.
	if err := rdb.HSet(ctx, fenceKey, placement.BattleTerminalFenceFieldProofID, "result:700:match:700").Err(); err != nil {
		t.Fatal(err)
	}
	proofKey := placement.BattleExitProofKey(42, 700)
	if redisSlot(PlacementKey(42)) != redisSlot(fenceKey) || redisSlot(fenceKey) != redisSlot(proofKey) {
		t.Fatalf("placement/fence/proof keys are not co-slotted: %q %q %q", PlacementKey(42), fenceKey, proofKey)
	}
	_, err = repo.UpdatePlacementWithBattleTerminalFence(ctx, 42, 700, 1,
		func(current *locatorv1.PlayerPlacementStorageRecord, found bool) (*locatorv1.PlayerPlacementStorageRecord, error) {
			if !found {
				t.Fatal("placement missing")
			}
			next := proto.Clone(current).(*locatorv1.PlayerPlacementStorageRecord)
			next.Version++
			return next, nil
		})
	if errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("terminal fence code=%v err=%v", errcode.As(err), err)
	}
	got, found, err := repo.GetPlacement(ctx, 42)
	if err != nil || !found || got.GetVersion() != 3 {
		t.Fatalf("fenced CAS mutated placement: found=%v got=%+v err=%v", found, got, err)
	}
}

func TestBattleTerminalFenceWinsRaceWithPlacementExec(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := NewRedisPlacementRepo(rdb)
	ctx := context.Background()
	rec := &locatorv1.PlayerPlacementStorageRecord{PlayerId: 42,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         3}
	payload, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, PlacementKey(42), payload, 0).Err(); err != nil {
		t.Fatal(err)
	}

	mutating := make(chan struct{})
	continueMutation := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, updateErr := repo.UpdatePlacementWithBattleTerminalFence(ctx, 42, 700, 2,
			func(current *locatorv1.PlayerPlacementStorageRecord, found bool) (*locatorv1.PlayerPlacementStorageRecord, error) {
				if !found {
					return nil, errcode.New(errcode.ErrLocatorNotFound, "placement missing")
				}
				close(mutating)
				<-continueMutation
				next := proto.Clone(current).(*locatorv1.PlayerPlacementStorageRecord)
				next.Version++
				return next, nil
			})
		done <- updateErr
	}()
	<-mutating
	if err := rdb.HSet(ctx, placement.BattleTerminalFenceKey(42, 700),
		placement.BattleTerminalFenceFieldProofID, "result:700:match:700").Err(); err != nil {
		t.Fatal(err)
	}
	close(continueMutation)
	if err := <-done; errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("terminal tombstone lost WATCH race: code=%v err=%v", errcode.As(err), err)
	}
	got, found, err := repo.GetPlacement(ctx, 42)
	if err != nil || !found || got.GetVersion() != 3 {
		t.Fatalf("racing fenced CAS committed: found=%v placement=%+v err=%v", found, got, err)
	}
}

// redisSlot is a compact CRC16 implementation used only to lock the key-tag
// contract. Redis Cluster hashes the substring inside the first non-empty {}.
func redisSlot(key string) uint16 {
	start, end := -1, -1
	for i, c := range []byte(key) {
		if c == '{' && start < 0 {
			start = i + 1
		} else if c == '}' && start >= 0 {
			end = i
			break
		}
	}
	if start >= 0 && end > start {
		key = key[start:end]
	}
	var crc uint16
	for _, b := range []byte(key) {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc % 16384
}
