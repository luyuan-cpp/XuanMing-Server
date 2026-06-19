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

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/data"
)

// stubRepo 内存版 LocationRepo,只供单测用。
type stubRepo struct {
	store map[uint64]data.LocationRecord
}

func newStubRepo() *stubRepo {
	return &stubRepo{store: map[uint64]data.LocationRecord{}}
}

func (s *stubRepo) SetGuarded(_ context.Context, playerID uint64, rec data.LocationRecord, _ time.Duration, _ int, guard func(cur data.LocationRecord, found bool) error) error {
	cur, found := s.store[playerID]
	if guard != nil {
		if err := guard(cur, found); err != nil {
			return err
		}
	}
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

func (s *stubRepo) BatchGet(_ context.Context, playerIDs []uint64) (map[uint64]data.LocationRecord, error) {
	out := make(map[uint64]data.LocationRecord, len(playerIDs))
	for _, pid := range playerIDs {
		if pid == 0 {
			continue
		}
		if rec, ok := s.store[pid]; ok {
			out[pid] = rec
		}
	}
	return out, nil
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

func TestBatchGetLocation(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	// 42 在 HUB,7 在 BATTLE,999 不存在(应缺席)。
	if err := uc.SetLocation(ctx, LocationInput{PlayerID: 42, State: LocationStateHub, HubPod: "hub-1", ShardID: 2}); err != nil {
		t.Fatalf("SetLocation(42): %v", err)
	}
	if err := uc.SetLocation(ctx, LocationInput{PlayerID: 7, State: LocationStateBattle, MatchID: 1001, BattlePod: "bp-9"}); err != nil {
		t.Fatalf("SetLocation(7): %v", err)
	}

	// 含 0 与重复 id,验证被跳过 / 去重不报错。
	got, err := uc.BatchGetLocation(ctx, []uint64{42, 7, 999, 0, 42})
	if err != nil {
		t.Fatalf("BatchGetLocation: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries (42,7), got %d: %+v", len(got), got)
	}
	if _, ok := got[999]; ok {
		t.Errorf("missing player 999 should be absent (offline), but present")
	}
	if _, ok := got[0]; ok {
		t.Errorf("player_id 0 should be skipped, but present")
	}
	if got[42].State != LocationStateHub || got[42].HubPod != "hub-1" || got[42].ShardID != 2 {
		t.Errorf("player 42 mismatch: %+v", got[42])
	}
	if got[7].State != LocationStateBattle || got[7].MatchID != 1001 || got[7].BattlePod != "bp-9" {
		t.Errorf("player 7 mismatch: %+v", got[7])
	}
}

func TestBatchGetLocation_Empty(t *testing.T) {
	uc := NewLocatorUsecase(newStubRepo(), 30*time.Second)
	got, err := uc.BatchGetLocation(context.Background(), nil)
	if err != nil {
		t.Fatalf("BatchGetLocation(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty input should yield empty map, got %d", len(got))
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

// --- W4 ⑩ 状态机守卫(不变量 §1) ---

// TestGuard_HubRejectedDuringMatching:玩家在 MATCHING 时,hub DS 的 HUB 上报被拒,
// 且 MATCHING 状态不被顶掉。
func TestGuard_HubRejectedDuringMatching(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	// matchmaker 写 MATCHING(控制面)
	if err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 100, State: LocationStateMatching, MatchID: 8888,
	}); err != nil {
		t.Fatalf("set MATCHING failed: %v", err)
	}

	// hub DS 上报 HUB(stale)→ 应被拒
	err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 100, State: LocationStateHub, HubPod: "hub-pod-2", ShardID: 1,
	})
	if err == nil {
		t.Fatal("expected ErrLocatorConflict for HUB report during MATCHING, got nil")
	}
	if got := errcode.As(err); got != errcode.ErrLocatorConflict {
		t.Errorf("expected ErrLocatorConflict(9202), got %d", got)
	}

	// MATCHING 不被顶掉
	out, _ := uc.GetLocation(ctx, 100)
	if out.State != LocationStateMatching {
		t.Errorf("MATCHING should survive stale HUB report, got state=%d", out.State)
	}
	if out.MatchID != 8888 {
		t.Errorf("match_id should remain 8888, got %d", out.MatchID)
	}
}

// TestGuard_ControlPlaneAlwaysWins:控制面写(MATCHING→BATTLE、LOGIN_PENDING 顶号)不受守卫拦截。
func TestGuard_ControlPlaneAlwaysWins(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	// MATCHING → BATTLE(matchmaker 全员确认)
	if err := uc.SetLocation(ctx, LocationInput{PlayerID: 1, State: LocationStateMatching, MatchID: 7}); err != nil {
		t.Fatalf("set MATCHING failed: %v", err)
	}
	if err := uc.SetLocation(ctx, LocationInput{PlayerID: 1, State: LocationStateBattle, MatchID: 7, BattlePod: "bp-1"}); err != nil {
		t.Fatalf("MATCHING→BATTLE should pass, got %v", err)
	}
	if out, _ := uc.GetLocation(ctx, 1); out.State != LocationStateBattle {
		t.Errorf("state should be BATTLE, got %d", out.State)
	}

	// 新登录 LOGIN_PENDING 顶号(覆盖 BATTLE)
	if err := uc.SetLocation(ctx, LocationInput{PlayerID: 1, State: LocationStateLoginPending}); err != nil {
		t.Fatalf("LOGIN_PENDING 顶号 should pass, got %v", err)
	}
	if out, _ := uc.GetLocation(ctx, 1); out.State != LocationStateLoginPending {
		t.Errorf("state should be LOGIN_PENDING, got %d", out.State)
	}
}

// TestGuard_HubAllowedFromNonMatching:HUB 上报在 OFFLINE / LOGIN_PENDING / HUB 时放行。
// （BATTLE 回流受 W4 ⑪ fence 约束，另见 TestFence_* 用例。）
func TestGuard_HubAllowedFromNonMatching(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		seed *LocationInput // nil = 不预置（OFFLINE）
	}{
		{"from offline", nil},
		{"from login_pending", &LocationInput{PlayerID: 1, State: LocationStateLoginPending}},
		{"from hub", &LocationInput{PlayerID: 1, State: LocationStateHub, HubPod: "hub-a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uc := NewLocatorUsecase(newStubRepo(), 30*time.Second)
			if c.seed != nil {
				if err := uc.SetLocation(ctx, *c.seed); err != nil {
					t.Fatalf("seed failed: %v", err)
				}
			}
			if err := uc.SetLocation(ctx, LocationInput{PlayerID: 1, State: LocationStateHub, HubPod: "hub-b", ShardID: 2}); err != nil {
				t.Fatalf("HUB report should pass from %s, got %v", c.name, err)
			}
			if out, _ := uc.GetLocation(ctx, 1); out.State != LocationStateHub || out.HubPod != "hub-b" {
				t.Errorf("state should be HUB(hub-b), got state=%d pod=%s", out.State, out.HubPod)
			}
		})
	}
}

// --- W4 ⑪ BATTLE fence（不变量 §1） ---

// TestFence_HubReturnFromBattleWithToken:玩家在 BATTLE（match_id=5），hub DS 携带正确
// match_id=5 的 HUB 回流上报 → 放行，切到 HUB，且不持久化 match_id/battle_pod。
func TestFence_HubReturnFromBattleWithToken(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	if err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 200, State: LocationStateBattle, MatchID: 5, BattlePod: "bp-5",
	}); err != nil {
		t.Fatalf("set BATTLE failed: %v", err)
	}

	// hub DS 回流，携带刚结束那场战斗的 fence 令牌 match_id=5
	if err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 200, State: LocationStateHub, HubPod: "hub-7", ShardID: 2, MatchID: 5,
	}); err != nil {
		t.Fatalf("HUB return with matching fence token should pass, got %v", err)
	}

	out, _ := uc.GetLocation(ctx, 200)
	if out.State != LocationStateHub || out.HubPod != "hub-7" {
		t.Errorf("state should be HUB(hub-7), got state=%d pod=%s", out.State, out.HubPod)
	}
	// fence 令牌不持久化：HUB 记录里 match_id/battle_pod 应被清零
	if out.MatchID != 0 || out.BattlePod != "" {
		t.Errorf("HUB record must not persist fence match_id/battle_pod, got match_id=%d battle_pod=%q", out.MatchID, out.BattlePod)
	}
}

// TestFence_StaleHubRejectedDuringBattle:玩家在 BATTLE（match_id=5），stale hub DS 不知道
// 该局，上报 HUB 携带 match_id=0 → 被拒，BATTLE 不被顶掉。
func TestFence_StaleHubRejectedDuringBattle(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	if err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 201, State: LocationStateBattle, MatchID: 5, BattlePod: "bp-5",
	}); err != nil {
		t.Fatalf("set BATTLE failed: %v", err)
	}

	err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 201, State: LocationStateHub, HubPod: "hub-stale", ShardID: 1,
	})
	if err == nil {
		t.Fatal("expected ErrLocatorConflict for stale HUB(match_id=0) during BATTLE, got nil")
	}
	if got := errcode.As(err); got != errcode.ErrLocatorConflict {
		t.Errorf("expected ErrLocatorConflict(9202), got %d", got)
	}

	out, _ := uc.GetLocation(ctx, 201)
	if out.State != LocationStateBattle || out.MatchID != 5 {
		t.Errorf("BATTLE(match_id=5) should survive stale HUB, got state=%d match_id=%d", out.State, out.MatchID)
	}
}

// TestFence_WrongMatchHubRejectedDuringBattle:玩家在 BATTLE（match_id=5），hub DS 上报
// HUB 携带错误 / 陈旧的 match_id=9 → 被拒，BATTLE 不被顶掉。
func TestFence_WrongMatchHubRejectedDuringBattle(t *testing.T) {
	repo := newStubRepo()
	uc := NewLocatorUsecase(repo, 30*time.Second)
	ctx := context.Background()

	if err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 202, State: LocationStateBattle, MatchID: 5, BattlePod: "bp-5",
	}); err != nil {
		t.Fatalf("set BATTLE failed: %v", err)
	}

	err := uc.SetLocation(ctx, LocationInput{
		PlayerID: 202, State: LocationStateHub, HubPod: "hub-old", ShardID: 1, MatchID: 9,
	})
	if err == nil {
		t.Fatal("expected ErrLocatorConflict for HUB with wrong fence match_id during BATTLE, got nil")
	}
	if got := errcode.As(err); got != errcode.ErrLocatorConflict {
		t.Errorf("expected ErrLocatorConflict(9202), got %d", got)
	}

	out, _ := uc.GetLocation(ctx, 202)
	if out.State != LocationStateBattle || out.MatchID != 5 {
		t.Errorf("BATTLE(match_id=5) should survive wrong-token HUB, got state=%d match_id=%d", out.State, out.MatchID)
	}
}
