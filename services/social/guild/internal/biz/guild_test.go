// guild_test.go — GuildUsecase 业务逻辑单测(2026-06-27)。
//
// 用内存版 fakeGuildRepo 复刻 MySQL 语义(单归属 + 职位 + 申请),无需真 DB。
// 覆盖:建会 / 申请 / 审批 / 退会(会长禁退)/ 踢人权限(leader / officer / member)/
// 解散 / 转让 / 任命官员 / 查询 / 推送 fan-out。
package biz

import (
	"context"
	"sort"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	guildv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/guild/v1"
	"github.com/luyuancpp/pandora/services/social/guild/internal/conf"
	"github.com/luyuancpp/pandora/services/social/guild/internal/data"
)

// ── 内存 fakeGuildRepo ──────────────────────────────────────────────────────────

type fakeGuildRepo struct {
	guilds   map[uint64]*data.GuildRow
	members  map[uint64]*data.GuildMemberRow // key = player_id(单归属)
	requests map[uint64]*data.GuildJoinRequestRow
	names    map[string]struct{}
}

func newFakeGuildRepo() *fakeGuildRepo {
	return &fakeGuildRepo{
		guilds:   map[uint64]*data.GuildRow{},
		members:  map[uint64]*data.GuildMemberRow{},
		requests: map[uint64]*data.GuildJoinRequestRow{},
		names:    map[string]struct{}{},
	}
}

func (f *fakeGuildRepo) CreateGuild(_ context.Context, newGuildID, leaderID uint64, name string, _ int) error {
	if _, ok := f.members[leaderID]; ok {
		return errcode.New(errcode.ErrGuildAlreadyInGuild, "already in guild")
	}
	if _, dup := f.names[name]; dup {
		return errcode.New(errcode.ErrGuildNameTaken, "name taken")
	}
	f.guilds[newGuildID] = &data.GuildRow{GuildID: newGuildID, Name: name, LeaderID: leaderID, MemberCount: 1, MaxMembers: 100}
	f.members[leaderID] = &data.GuildMemberRow{PlayerID: leaderID, GuildID: newGuildID, Role: data.GuildRoleLeader}
	f.names[name] = struct{}{}
	return nil
}

func (f *fakeGuildRepo) GetGuild(_ context.Context, guildID uint64) (*data.GuildRow, bool, error) {
	g, ok := f.guilds[guildID]
	return g, ok, nil
}

func (f *fakeGuildRepo) GetMyGuild(_ context.Context, playerID uint64) (*data.GuildRow, bool, error) {
	m, ok := f.members[playerID]
	if !ok {
		return nil, false, nil
	}
	return f.guilds[m.GuildID], true, nil
}

func (f *fakeGuildRepo) GetMember(_ context.Context, playerID uint64) (*data.GuildMemberRow, bool, error) {
	m, ok := f.members[playerID]
	return m, ok, nil
}

func (f *fakeGuildRepo) ListMembers(_ context.Context, guildID, cursor uint64, limit int) ([]data.GuildMemberRow, error) {
	var out []data.GuildMemberRow
	for _, m := range f.members {
		if m.GuildID == guildID && (cursor == 0 || m.PlayerID > cursor) {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlayerID < out[j].PlayerID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeGuildRepo) CreateJoinRequest(_ context.Context, newRequestID, guildID, playerID uint64, maxPending int) (uint64, bool, error) {
	pending := 0
	for _, rq := range f.requests {
		if rq.GuildID == guildID && rq.PlayerID == playerID && rq.Status == 1 {
			return rq.RequestID, true, nil
		}
		if rq.GuildID == guildID && rq.Status == 1 {
			pending++
		}
	}
	if maxPending > 0 && pending >= maxPending {
		return 0, false, errcode.New(errcode.ErrGuildRequestLimit, "pending limit")
	}
	f.requests[newRequestID] = &data.GuildJoinRequestRow{RequestID: newRequestID, GuildID: guildID, PlayerID: playerID, Status: 1}
	return newRequestID, false, nil
}

func (f *fakeGuildRepo) GetRequest(_ context.Context, requestID uint64) (*data.GuildJoinRequestRow, bool, error) {
	rq, ok := f.requests[requestID]
	return rq, ok, nil
}

func (f *fakeGuildRepo) ApproveJoin(_ context.Context, requestID, approverID uint64, maxMembers int) (bool, error) {
	rq, ok := f.requests[requestID]
	if !ok || rq.Status != 1 {
		return false, nil
	}
	ap, ok := f.members[approverID]
	if !ok || ap.GuildID != rq.GuildID || (ap.Role != data.GuildRoleLeader && ap.Role != data.GuildRoleOfficer) {
		return false, errcode.New(errcode.ErrGuildNoPermission, "no perm")
	}
	if _, in := f.members[rq.PlayerID]; in {
		return false, errcode.New(errcode.ErrGuildAlreadyInGuild, "applicant already in guild")
	}
	g := f.guilds[rq.GuildID]
	if int(g.MemberCount) >= maxMembers {
		return false, errcode.New(errcode.ErrGuildFull, "full")
	}
	f.members[rq.PlayerID] = &data.GuildMemberRow{PlayerID: rq.PlayerID, GuildID: rq.GuildID, Role: data.GuildRoleMember}
	g.MemberCount++
	rq.Status = 2
	return true, nil
}

func (f *fakeGuildRepo) RejectJoin(_ context.Context, requestID, approverID uint64) (bool, error) {
	rq, ok := f.requests[requestID]
	if !ok || rq.Status != 1 {
		return false, nil
	}
	ap, ok := f.members[approverID]
	if !ok || ap.GuildID != rq.GuildID || (ap.Role != data.GuildRoleLeader && ap.Role != data.GuildRoleOfficer) {
		return false, errcode.New(errcode.ErrGuildNoPermission, "no perm")
	}
	rq.Status = 3
	return true, nil
}

func (f *fakeGuildRepo) RemoveMember(_ context.Context, guildID, playerID uint64) error {
	if m, ok := f.members[playerID]; ok && m.GuildID == guildID {
		delete(f.members, playerID)
		if g := f.guilds[guildID]; g != nil {
			g.MemberCount--
		}
	}
	return nil
}

func (f *fakeGuildRepo) DisbandGuild(_ context.Context, guildID uint64) error {
	for pid, m := range f.members {
		if m.GuildID == guildID {
			delete(f.members, pid)
		}
	}
	for rid, rq := range f.requests {
		if rq.GuildID == guildID {
			delete(f.requests, rid)
		}
	}
	if g := f.guilds[guildID]; g != nil {
		delete(f.names, g.Name)
	}
	delete(f.guilds, guildID)
	return nil
}

func (f *fakeGuildRepo) SetRole(_ context.Context, guildID, playerID uint64, role int32) error {
	if m, ok := f.members[playerID]; ok && m.GuildID == guildID {
		m.Role = role
	}
	return nil
}

func (f *fakeGuildRepo) TransferLeader(_ context.Context, guildID, oldLeaderID, newLeaderID uint64) error {
	if m, ok := f.members[oldLeaderID]; ok {
		m.Role = data.GuildRoleMember
	}
	if m, ok := f.members[newLeaderID]; ok {
		m.Role = data.GuildRoleLeader
	}
	if g := f.guilds[guildID]; g != nil {
		g.LeaderID = newLeaderID
	}
	return nil
}

func (f *fakeGuildRepo) ListPendingRequests(_ context.Context, guildID, cursor uint64, limit int) ([]data.GuildJoinRequestRow, error) {
	var out []data.GuildJoinRequestRow
	for _, rq := range f.requests {
		if rq.GuildID == guildID && rq.Status == 1 && (cursor == 0 || rq.RequestID > cursor) {
			out = append(out, *rq)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ── fakeGuildPusher ─────────────────────────────────────────────────────────────

type guildPushRecord struct {
	to  uint64
	evt *guildv1.GuildEvent
}

type fakeGuildPusher struct {
	pushes []guildPushRecord
}

func (f *fakeGuildPusher) PushGuildEvent(_ context.Context, toPlayerID uint64, evt *guildv1.GuildEvent) error {
	f.pushes = append(f.pushes, guildPushRecord{to: toPlayerID, evt: evt})
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newGuildUC(repo data.GuildRepo, pusher GuildEventPusher) *GuildUsecase {
	return NewGuildUsecase(repo, pusher, conf.GuildConf{MaxGuildMembers: 100, MaxGroupMembers: 50, MaxNameLen: 24})
}

func wantGuildCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if errcode.As(err) != code {
		t.Fatalf("want code %d, got err=%v (code=%d)", code, err, errcode.As(err))
	}
}

// ── 测试 ──────────────────────────────────────────────────────────────────────

func TestCreateGuild_OK(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	id, err := uc.CreateGuild(context.Background(), 1, "Knights", 1001)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != 1001 {
		t.Fatalf("want 1001, got %d", id)
	}
	if m, _, _ := repo.GetMember(context.Background(), 1); m == nil || m.Role != data.GuildRoleLeader {
		t.Fatalf("creator must be leader")
	}
}

func TestCreateGuild_EmptyName(t *testing.T) {
	uc := newGuildUC(newFakeGuildRepo(), nil)
	_, err := uc.CreateGuild(context.Background(), 1, "   ", 1001)
	wantGuildCode(t, err, errcode.ErrInvalidArg)
}

func TestCreateGuild_AlreadyInGuild(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "A", 1001)
	_, err := uc.CreateGuild(context.Background(), 1, "B", 1002)
	wantGuildCode(t, err, errcode.ErrGuildAlreadyInGuild)
}

func TestCreateGuild_NameTaken(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "Dup", 1001)
	_, err := uc.CreateGuild(context.Background(), 2, "Dup", 1002)
	wantGuildCode(t, err, errcode.ErrGuildNameTaken)
}

func TestApplyAndApprove_OK(t *testing.T) {
	repo := newFakeGuildRepo()
	pusher := &fakeGuildPusher{}
	uc := newGuildUC(repo, pusher)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)

	rid, err := uc.ApplyJoin(context.Background(), 2, 1001, 2001)
	if err != nil {
		t.Fatalf("apply err: %v", err)
	}
	// 申请通知发给会长 1(原则 2:不发申请人)。
	if len(pusher.pushes) != 1 || pusher.pushes[0].to != 1 {
		t.Fatalf("want 1 push to leader, got %+v", pusher.pushes)
	}

	if err := uc.ApproveJoin(context.Background(), 1, rid); err != nil {
		t.Fatalf("approve err: %v", err)
	}
	if m, _, _ := repo.GetMember(context.Background(), 2); m == nil || m.GuildID != 1001 {
		t.Fatalf("applicant should be member now")
	}
}

func TestApplyJoin_AlreadyInGuild(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	_, err := uc.ApplyJoin(context.Background(), 1, 1001, 2001)
	wantGuildCode(t, err, errcode.ErrGuildAlreadyInGuild)
}

func TestApplyJoin_GuildNotFound(t *testing.T) {
	uc := newGuildUC(newFakeGuildRepo(), nil)
	_, err := uc.ApplyJoin(context.Background(), 2, 9999, 2001)
	wantGuildCode(t, err, errcode.ErrGuildNotFound)
}

func TestLeaveGuild_LeaderForbidden(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	err := uc.LeaveGuild(context.Background(), 1)
	wantGuildCode(t, err, errcode.ErrGuildNotLeader)
}

func TestLeaveGuild_MemberOK(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	rid, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2001)
	_ = uc.ApproveJoin(context.Background(), 1, rid)
	if err := uc.LeaveGuild(context.Background(), 2); err != nil {
		t.Fatalf("member leave err: %v", err)
	}
	if m, _, _ := repo.GetMember(context.Background(), 2); m != nil {
		t.Fatalf("member should be gone")
	}
}

func TestKickMember_OfficerCannotKickOfficer(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001) // 1 = leader
	// 2 / 3 入会
	r2, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2002)
	_ = uc.ApproveJoin(context.Background(), 1, r2)
	r3, _ := uc.ApplyJoin(context.Background(), 3, 1001, 2003)
	_ = uc.ApproveJoin(context.Background(), 1, r3)
	// 2 / 3 都升 officer
	_ = uc.SetOfficer(context.Background(), 1, 2, true)
	_ = uc.SetOfficer(context.Background(), 1, 3, true)
	// officer 2 踢 officer 3 → 无权
	err := uc.KickMember(context.Background(), 2, 3)
	wantGuildCode(t, err, errcode.ErrGuildNoPermission)
}

func TestKickMember_LeaderKicksOfficer(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	r2, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2002)
	_ = uc.ApproveJoin(context.Background(), 1, r2)
	_ = uc.SetOfficer(context.Background(), 1, 2, true)
	if err := uc.KickMember(context.Background(), 1, 2); err != nil {
		t.Fatalf("leader kick officer err: %v", err)
	}
	if m, _, _ := repo.GetMember(context.Background(), 2); m != nil {
		t.Fatalf("officer should be kicked")
	}
}

func TestKickMember_CannotKickLeader(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	r2, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2002)
	_ = uc.ApproveJoin(context.Background(), 1, r2)
	_ = uc.SetOfficer(context.Background(), 1, 2, true)
	err := uc.KickMember(context.Background(), 2, 1) // officer 踢 leader
	wantGuildCode(t, err, errcode.ErrGuildNoPermission)
}

func TestDisbandGuild_NotifiesAll(t *testing.T) {
	repo := newFakeGuildRepo()
	pusher := &fakeGuildPusher{}
	uc := newGuildUC(repo, pusher)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	r2, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2002)
	_ = uc.ApproveJoin(context.Background(), 1, r2)
	pusher.pushes = nil // 清掉申请通知

	if err := uc.DisbandGuild(context.Background(), 1); err != nil {
		t.Fatalf("disband err: %v", err)
	}
	if len(pusher.pushes) != 2 {
		t.Fatalf("want 2 disband notifies (all members), got %d", len(pusher.pushes))
	}
	if _, ok := repo.guilds[1001]; ok {
		t.Fatalf("guild should be deleted")
	}
}

func TestDisbandGuild_NotLeader(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	r2, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2002)
	_ = uc.ApproveJoin(context.Background(), 1, r2)
	err := uc.DisbandGuild(context.Background(), 2)
	wantGuildCode(t, err, errcode.ErrGuildNotLeader)
}

func TestTransferLeader_OK(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	r2, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2002)
	_ = uc.ApproveJoin(context.Background(), 1, r2)
	if err := uc.TransferLeader(context.Background(), 1, 2); err != nil {
		t.Fatalf("transfer err: %v", err)
	}
	if m, _, _ := repo.GetMember(context.Background(), 2); m == nil || m.Role != data.GuildRoleLeader {
		t.Fatalf("2 should be leader")
	}
	if m, _, _ := repo.GetMember(context.Background(), 1); m == nil || m.Role != data.GuildRoleMember {
		t.Fatalf("1 should be demoted to member")
	}
}

func TestListJoinRequests_MemberNoPerm(t *testing.T) {
	repo := newFakeGuildRepo()
	uc := newGuildUC(repo, nil)
	_, _ = uc.CreateGuild(context.Background(), 1, "G", 1001)
	r2, _ := uc.ApplyJoin(context.Background(), 2, 1001, 2002)
	_ = uc.ApproveJoin(context.Background(), 1, r2)
	_, _, err := uc.ListJoinRequests(context.Background(), 2, 0, 0) // 普通成员
	wantGuildCode(t, err, errcode.ErrGuildNoPermission)
}

func TestGetMyGuild_NotInGuild(t *testing.T) {
	uc := newGuildUC(newFakeGuildRepo(), nil)
	g, err := uc.GetMyGuild(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if g != nil {
		t.Fatalf("want nil guild for non-member")
	}
}
