// locator_test.go — LocatorUsecase 单测(W3 ⑤,2026-06-05)。
//
// 覆盖:
//   - SetLocation 输入校验(player_id 0、state 越界、HUB 缺 hub_pod、MATCHING 缺 match_id、BATTLE 缺 battle_pod)
//   - SetLocation OK + 回放 GetLocation 读取
//   - GetLocation 不存在 → OFFLINE 占位
//   - ClearLocation OK + 再 Get → OFFLINE
//
// 不接真实 redis;用一个简易的内存 stub 替 data.LocationRepo,验 biz 逻辑闭环。
package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/data"
)

// stubRepo 内存版 LocationRepo,只供单测用。
type stubRepo struct {
	store map[uint64]data.LocationRecord
}

func newStubRepo() *stubRepo {
	return &stubRepo{store: map[uint64]data.LocationRecord{}}
}

func (s *stubRepo) Set(_ context.Context, playerID uint64, rec data.LocationRecord, _ time.Duration) error {
	s.store[playerID] = rec
	return nil
}

func (s *stubRepo) Get(_ context.Context, playerID uint64) (data.LocationRecord, bool, error) {
	rec, ok := s.store[playerID]
	if !ok {
		return data.LocationRecord{}, false, nil
	}
	return rec, true, nil
}

func (s *stubRepo) Delete(_ context.Context, playerID uint64) error {
	delete(s.store, playerID)
	return nil
}

func TestSetLocation_InvalidInput(t *testing.T) {
	uc := NewLocatorUsecase(newStubRepo(), 30*time.Second)

	cases := []struct {
		name string
		in   LocationInput
	}{
		{"zero player_id", LocationInput{PlayerID: 0, State: LocationStateHub, HubPod: "p1"}},
		{"state out of range", LocationInput{PlayerID: 1, State: 99}},
		{"hub without pod", LocationInput{PlayerID: 1, State: LocationStateHub}},
		{"matching without match_id", LocationInput{PlayerID: 1, State: LocationStateMatching}},
		{"battle missing match_id", LocationInput{PlayerID: 1, State: LocationStateBattle, BattlePod: "bp"}},
		{"battle missing battle_pod", LocationInput{PlayerID: 1, State: LocationStateBattle, MatchID: 1001}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := uc.SetLocation(context.Background(), c.in); err == nil {
				t.Fatalf("expected error for %+v, got nil", c.in)
			}
		})
	}
}

func TestSetLocation_AndGet(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	in := LocationInput{
		PlayerID: 42,
		State:    LocationStateHub,
		HubPod:   "hub-pod-7",
		ShardID:  3,
	}
	if err := uc.SetLocation(ctx, in); err != nil {
		t.Fatalf("SetLocation failed: %v", err)
	}

	out, err := uc.GetLocation(ctx, 42)
	if err != nil {
		t.Fatalf("GetLocation failed: %v", err)
	}
	if out.State != LocationStateHub {
		t.Errorf("state mismatch: got %d, want %d", out.State, LocationStateHub)
	}
	if out.HubPod != "hub-pod-7" {
		t.Errorf("hub_pod mismatch: got %q, want %q", out.HubPod, "hub-pod-7")
	}
	if out.ShardID != 3 {
		t.Errorf("shard_id mismatch: got %d, want 3", out.ShardID)
	}
	if out.UpdatedAtMs == 0 {
		t.Errorf("updated_at_ms not set")
	}
}

func TestGetLocation_OfflineWhenMissing(t *testing.T) {
	uc := NewLocatorUsecase(newStubRepo(), 30*time.Second)
	out, err := uc.GetLocation(context.Background(), 999)
	if err != nil {
		t.Fatalf("GetLocation should not error on miss: %v", err)
	}
	if out.State != LocationStateOffline {
		t.Errorf("miss should return OFFLINE(%d), got %d", LocationStateOffline, out.State)
	}
}

func TestClearLocation(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	if err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 7,
		State:    LocationStateMatching,
		MatchID:  1001,
	}); err != nil {
		t.Fatalf("SetLocation failed: %v", err)
	}
	if err := uc.ClearLocation(ctx, 7); err != nil {
		t.Fatalf("ClearLocation failed: %v", err)
	}

	out, err := uc.GetLocation(ctx, 7)
	if err != nil {
		t.Fatalf("GetLocation after clear: %v", err)
	}
	if out.State != LocationStateOffline {
		t.Errorf("after clear should be OFFLINE, got state=%d", out.State)
	}
}

func TestClearLocation_InvalidPlayerID(t *testing.T) {
	uc := NewLocatorUsecase(newStubRepo(), 30*time.Second)
	err := uc.ClearLocation(context.Background(), 0)
	if err == nil {
		t.Fatal("expected error for player_id=0, got nil")
	}
	// 确认有错误就行,具体 code 不在本测试范围
	if errors.Is(err, nil) {
		t.Fatal("err should not be nil")
	}
}

func TestNewLocatorUsecase_DefaultTTL(t *testing.T) {
	uc := NewLocatorUsecase(newStubRepo(), 0)
	if uc.ttl != 30*time.Second {
		t.Errorf("default ttl should be 30s, got %v", uc.ttl)
	}
	uc2 := NewLocatorUsecase(newStubRepo(), -1)
	if uc2.ttl != 30*time.Second {
		t.Errorf("negative ttl should fall to 30s, got %v", uc2.ttl)
	}
	uc3 := NewLocatorUsecase(newStubRepo(), 5*time.Second)
	if uc3.ttl != 5*time.Second {
		t.Errorf("explicit ttl=5s expected, got %v", uc3.ttl)
	}
}
