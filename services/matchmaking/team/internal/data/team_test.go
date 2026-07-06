package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
	teamv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/team/v1"
)

// newTestRepo 启动 miniredis 并返回 RedisTeamRepo + cleanup。
func newTestRepo(t *testing.T) (*RedisTeamRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = rdb.Close()
		mr.Close()
	})
	return NewRedisTeamRepo(rdb), mr
}

// sampleTeam 构造一个测试用 teamv1.TeamStorageRecord。
func sampleTeam(teamID, captainID uint64) *teamv1.TeamStorageRecord {
	return &teamv1.TeamStorageRecord{
		TeamId:      teamID,
		CaptainId:   captainID,
		State:       teamv1.TeamState_TEAM_STATE_FORMING,
		Members:     []*teamv1.TeamMemberStorageRecord{{PlayerId: captainID, Nickname: "alice", Mmr: 1000, Ready: false}},
		CreatedAtMs: 1_780_000_000_000,
		UpdatedAtMs: 1_780_000_000_000,
		MaxSize:     5,
	}
}

// TestCreate 验证创建队伍后可正常读回。
func TestCreate(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	team := sampleTeam(1001, 2001)

	if err := repo.Create(ctx, team, 30*time.Second); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, found, err := repo.Get(ctx, 1001)
	if err != nil || !found {
		t.Fatalf("Get after Create: found=%v err=%v", found, err)
	}
	if got.CaptainId != 2001 || got.State != teamv1.TeamState_TEAM_STATE_FORMING || len(got.Members) != 1 {
		t.Errorf("unexpected team: %+v", got)
	}
}

// TestGetNotFound 验证不存在的 key 返回 (nil, false, nil)。
func TestGetNotFound(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	_, found, err := repo.Get(ctx, 9999)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if found {
		t.Error("expected not found")
	}
}

// TestUpdateWithLock 验证 UpdateWithLock 正常更新。
func TestUpdateWithLock(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	team := sampleTeam(2001, 3001)

	if err := repo.Create(ctx, team, 30*time.Second); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 加一个成员并改状态
	err := repo.UpdateWithLock(ctx, 2001, 3, func(rec *teamv1.TeamStorageRecord) error {
		rec.Members = append(rec.Members, &teamv1.TeamMemberStorageRecord{PlayerId: 3002, Nickname: "bob", Mmr: 900})
		rec.UpdatedAtMs = 1_780_000_001_000
		return nil
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("UpdateWithLock: %v", err)
	}

	got, _, _ := repo.Get(ctx, 2001)
	if len(got.Members) != 2 {
		t.Errorf("expected 2 members, got %d", len(got.Members))
	}
	if got.UpdatedAtMs != 1_780_000_001_000 {
		t.Errorf("unexpected updated_at_ms: %d", got.UpdatedAtMs)
	}
}

// TestUpdateWithLockFnError 验证 fn 返错时不写 Redis 且透传错误。
func TestUpdateWithLockFnError(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()
	team := sampleTeam(3001, 4001)

	if err := repo.Create(ctx, team, 30*time.Second); err != nil {
		t.Fatalf("Create: %v", err)
	}

	bizErr := errcode.New(errcode.ErrTeamWrongState, "cannot leave in MATCHING")
	err := repo.UpdateWithLock(ctx, 3001, 3, func(rec *teamv1.TeamStorageRecord) error {
		return bizErr
	}, 30*time.Second)
	if errcode.As(err) != errcode.ErrTeamWrongState {
		t.Errorf("expected ErrTeamWrongState, got: %v", err)
	}
}

// TestUpdateWithLockNotFound 验证 key 不存在时返回 ErrTeamNotFound。
func TestUpdateWithLockNotFound(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	err := repo.UpdateWithLock(ctx, 9999, 3, func(rec *teamv1.TeamStorageRecord) error {
		return nil
	}, 30*time.Second)
	if errcode.As(err) != errcode.ErrTeamNotFound {
		t.Errorf("expected ErrTeamNotFound, got: %v", err)
	}
}

// TestPlayerIndex 验证 player index 的 set/get/delete。
func TestPlayerIndex(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	if err := repo.SetPlayerIndex(ctx, 5001, 6001, 30*time.Second); err != nil {
		t.Fatalf("SetPlayerIndex: %v", err)
	}

	teamID, found, err := repo.GetPlayerTeamID(ctx, 5001)
	if err != nil || !found || teamID != 6001 {
		t.Errorf("GetPlayerTeamID: found=%v teamID=%d err=%v", found, teamID, err)
	}

	if err := repo.DeletePlayerIndex(ctx, 5001); err != nil {
		t.Fatalf("DeletePlayerIndex: %v", err)
	}

	_, found, _ = repo.GetPlayerTeamID(ctx, 5001)
	if found {
		t.Error("expected not found after delete")
	}
}

// TestInvite 验证 SetInvite / GetInvite / DeleteInvite。
func TestInvite(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	if err := repo.SetInvite(ctx, 7001, 8001, 9001, 60*time.Second); err != nil {
		t.Fatalf("SetInvite: %v", err)
	}

	inv, found, err := repo.GetInvite(ctx, 7001)
	if err != nil || !found {
		t.Fatalf("GetInvite: found=%v err=%v", found, err)
	}
	if inv.TeamID != 8001 || inv.TargetPlayerID != 9001 {
		t.Errorf("unexpected invite: %+v", inv)
	}

	if err := repo.DeleteInvite(ctx, 7001); err != nil {
		t.Fatalf("DeleteInvite: %v", err)
	}
	_, found, _ = repo.GetInvite(ctx, 7001)
	if found {
		t.Error("expected not found after delete")
	}
}

// TestInviteExpiry 验证邀请 TTL 过期后 GetInvite 返回 not found。
func TestInviteExpiry(t *testing.T) {
	repo, mr := newTestRepo(t)
	ctx := context.Background()

	if err := repo.SetInvite(ctx, 7002, 8002, 9002, 1*time.Second); err != nil {
		t.Fatalf("SetInvite: %v", err)
	}

	// 快进 miniredis 时钟 2s
	mr.FastForward(2 * time.Second)

	_, found, err := repo.GetInvite(ctx, 7002)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if found {
		t.Error("expected invite expired")
	}
}

// TestClaimPlayer 验证 SETNX 语义:首次声明成功,重复声明返回现有归属且失败。
func TestClaimPlayer(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	// 首次声明 player 5001 → team 6001 成功
	gotTeam, claimed, err := repo.ClaimPlayer(ctx, 5001, 6001, 30*time.Second)
	if err != nil || !claimed || gotTeam != 6001 {
		t.Fatalf("first claim: team=%d claimed=%v err=%v", gotTeam, claimed, err)
	}

	// 同一 player 再声明到 team 6002 → 失败,返回现有归属 6001
	gotTeam, claimed, err = repo.ClaimPlayer(ctx, 5001, 6002, 30*time.Second)
	if err != nil {
		t.Fatalf("second claim err: %v", err)
	}
	if claimed {
		t.Error("expected second claim to fail (player already claimed)")
	}
	if gotTeam != 6001 {
		t.Errorf("expected existing team 6001, got %d", gotTeam)
	}

	// 释放后可再次声明
	if err := repo.DeletePlayerIndex(ctx, 5001); err != nil {
		t.Fatalf("DeletePlayerIndex: %v", err)
	}
	gotTeam, claimed, err = repo.ClaimPlayer(ctx, 5001, 6002, 30*time.Second)
	if err != nil || !claimed || gotTeam != 6002 {
		t.Fatalf("reclaim after release: team=%d claimed=%v err=%v", gotTeam, claimed, err)
	}
}

// TestExpireTeam 验证 ExpireTeam 只改 TTL 不动 value,且过期后 key 消失。
func TestExpireTeam(t *testing.T) {
	repo, mr := newTestRepo(t)
	ctx := context.Background()
	team := sampleTeam(1001, 2001)

	if err := repo.Create(ctx, team, 30*time.Second); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 改短 TTL 为 1s
	if err := repo.ExpireTeam(ctx, 1001, 1*time.Second); err != nil {
		t.Fatalf("ExpireTeam: %v", err)
	}

	// 改 TTL 不动数据,立即仍可读
	if _, found, _ := repo.Get(ctx, 1001); !found {
		t.Fatal("team should still exist right after ExpireTeam")
	}

	// 快进 2s,key 过期消失
	mr.FastForward(2 * time.Second)
	if _, found, _ := repo.Get(ctx, 1001); found {
		t.Error("team should be gone after TTL expired")
	}
}

// TestTouchTeam 验证 TouchTeam 同时把队伍 key 与玩家索引 key 的 TTL 拉回 ttl(在线保活)。
func TestTouchTeam(t *testing.T) {
	repo, mr := newTestRepo(t)
	ctx := context.Background()
	team := sampleTeam(1001, 2001)

	if err := repo.Create(ctx, team, 5*time.Second); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.SetPlayerIndex(ctx, 2001, 1001, 5*time.Second); err != nil {
		t.Fatalf("SetPlayerIndex: %v", err)
	}

	// 快进 4s:两 key 仅剩 1s 寿命
	mr.FastForward(4 * time.Second)

	if err := repo.TouchTeam(ctx, 1001, 2001, 60*time.Second); err != nil {
		t.Fatalf("TouchTeam: %v", err)
	}

	// 再快进 30s:若未续期早已过期;续期后应仍存活
	mr.FastForward(30 * time.Second)
	if _, found, _ := repo.Get(ctx, 1001); !found {
		t.Error("team key should survive after TouchTeam renewal")
	}
	if _, found, _ := repo.GetPlayerTeamID(ctx, 2001); !found {
		t.Error("player index should survive after TouchTeam renewal")
	}

	// 续期不动 value
	got, _, err := repo.Get(ctx, 1001)
	if err != nil || got.CaptainId != 2001 {
		t.Errorf("TouchTeam must not modify value: %+v err=%v", got, err)
	}
}
