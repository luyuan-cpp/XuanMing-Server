// group_test.go — GroupUsecase 业务逻辑单测(2026-06-27)。
//
// 用内存版 fakeGroupRepo 复刻 MySQL 语义(多归属 + owner/member 两级),无需真 DB。
// 覆盖:建群 / 拉人幂等 / 退群(群主禁退)/ 踢人(仅群主)/ 解散 / 转让群主 / 查询。
package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/social/guild/internal/conf"
	"github.com/luyuancpp/pandora/services/social/guild/internal/data"
)

// ── 内存 fakeGroupRepo ──────────────────────────────────────────────────────────

type memberKey struct {
	group  uint64
	player uint64
}

type fakeGroupRepo struct {
	groups  map[uint64]*data.GroupRow
	members map[memberKey]*data.GroupMemberRow
}

func newFakeGroupRepo() *fakeGroupRepo {
	return &fakeGroupRepo{
		groups:  map[uint64]*data.GroupRow{},
		members: map[memberKey]*data.GroupMemberRow{},
	}
}

func (f *fakeGroupRepo) CreateGroup(_ context.Context, newGroupID, ownerID uint64, name string, memberIDs []uint64, maxMembers, maxGroups int) error {
	count := 1 + len(memberIDs)
	if count > maxMembers {
		return errcode.New(errcode.ErrGroupFull, "full")
	}
	if maxGroups > 0 {
		for _, p := range append([]uint64{ownerID}, memberIDs...) {
			if f.countPlayerGroups(p) >= maxGroups {
				return errcode.New(errcode.ErrGroupJoinLimit, "group join limit")
			}
		}
	}
	f.groups[newGroupID] = &data.GroupRow{GroupID: newGroupID, Name: name, OwnerID: ownerID, MemberCount: int32(count), MaxMembers: int32(maxMembers)}
	f.members[memberKey{newGroupID, ownerID}] = &data.GroupMemberRow{GroupID: newGroupID, PlayerID: ownerID, Role: data.GroupRoleOwner}
	for _, m := range memberIDs {
		f.members[memberKey{newGroupID, m}] = &data.GroupMemberRow{GroupID: newGroupID, PlayerID: m, Role: data.GroupRoleMember}
	}
	return nil
}

func (f *fakeGroupRepo) countPlayerGroups(playerID uint64) int {
	n := 0
	for k := range f.members {
		if k.player == playerID {
			n++
		}
	}
	return n
}

func (f *fakeGroupRepo) GetGroup(_ context.Context, groupID uint64) (*data.GroupRow, bool, error) {
	g, ok := f.groups[groupID]
	return g, ok, nil
}

func (f *fakeGroupRepo) GetGroupMember(_ context.Context, groupID, playerID uint64) (*data.GroupMemberRow, bool, error) {
	m, ok := f.members[memberKey{groupID, playerID}]
	return m, ok, nil
}

func (f *fakeGroupRepo) ListGroupMembers(_ context.Context, groupID uint64) ([]data.GroupMemberRow, error) {
	var out []data.GroupMemberRow
	for k, m := range f.members {
		if k.group == groupID {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (f *fakeGroupRepo) ListMyGroups(_ context.Context, playerID uint64) ([]data.GroupRow, error) {
	var out []data.GroupRow
	for k := range f.members {
		if k.player == playerID {
			if g := f.groups[k.group]; g != nil {
				out = append(out, *g)
			}
		}
	}
	return out, nil
}

func (f *fakeGroupRepo) AddMember(_ context.Context, groupID, playerID uint64, maxMembers, maxGroups int) (bool, error) {
	if _, ok := f.members[memberKey{groupID, playerID}]; ok {
		return true, nil
	}
	g, ok := f.groups[groupID]
	if !ok {
		return false, errcode.New(errcode.ErrGroupNotFound, "not found")
	}
	if int(g.MemberCount) >= maxMembers {
		return false, errcode.New(errcode.ErrGroupFull, "full")
	}
	if maxGroups > 0 && f.countPlayerGroups(playerID) >= maxGroups {
		return false, errcode.New(errcode.ErrGroupJoinLimit, "group join limit")
	}
	f.members[memberKey{groupID, playerID}] = &data.GroupMemberRow{GroupID: groupID, PlayerID: playerID, Role: data.GroupRoleMember}
	g.MemberCount++
	return false, nil
}

func (f *fakeGroupRepo) RemoveMember(_ context.Context, groupID, playerID uint64) error {
	if _, ok := f.members[memberKey{groupID, playerID}]; ok {
		delete(f.members, memberKey{groupID, playerID})
		if g := f.groups[groupID]; g != nil {
			g.MemberCount--
		}
	}
	return nil
}

func (f *fakeGroupRepo) DisbandGroup(_ context.Context, groupID uint64) error {
	for k := range f.members {
		if k.group == groupID {
			delete(f.members, k)
		}
	}
	delete(f.groups, groupID)
	return nil
}

func (f *fakeGroupRepo) TransferOwner(_ context.Context, groupID, oldOwnerID, newOwnerID uint64) error {
	if m, ok := f.members[memberKey{groupID, oldOwnerID}]; ok {
		m.Role = data.GroupRoleMember
	}
	if m, ok := f.members[memberKey{groupID, newOwnerID}]; ok {
		m.Role = data.GroupRoleOwner
	}
	if g := f.groups[groupID]; g != nil {
		g.OwnerID = newOwnerID
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newGroupUC(repo data.GroupRepo) *GroupUsecase {
	return NewGroupUsecase(repo, conf.GuildConf{MaxGuildMembers: 100, MaxGroupMembers: 50, MaxNameLen: 24})
}

// ── 测试 ──────────────────────────────────────────────────────────────────────

func TestCreateGroup_OK(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	id, err := uc.CreateGroup(context.Background(), 1, "g", []uint64{2, 3, 1, 2}, 9001) // 含重复 + owner
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != 9001 {
		t.Fatalf("want 9001, got %d", id)
	}
	// owner + 2 + 3 = 3 人(去重排除 owner / 重复)。
	if g := repo.groups[9001]; g == nil || g.MemberCount != 3 {
		t.Fatalf("want 3 members, got %+v", g)
	}
}

func TestCreateGroup_EmptyName(t *testing.T) {
	uc := newGroupUC(newFakeGroupRepo())
	_, err := uc.CreateGroup(context.Background(), 1, "  ", nil, 9001)
	wantGuildCode(t, err, errcode.ErrInvalidArg)
}

func TestInviteToGroup_Idempotent(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", nil, 9001)
	// 1 拉 2 入群
	if err := uc.InviteToGroup(context.Background(), 1, 9001, 2); err != nil {
		t.Fatalf("invite err: %v", err)
	}
	// 重复拉 2 → 幂等成功
	if err := uc.InviteToGroup(context.Background(), 1, 9001, 2); err != nil {
		t.Fatalf("idempotent invite err: %v", err)
	}
	if g := repo.groups[9001]; g.MemberCount != 2 {
		t.Fatalf("want 2 members after idempotent invite, got %d", g.MemberCount)
	}
}

func TestInviteToGroup_OperatorNotMember(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", nil, 9001)
	err := uc.InviteToGroup(context.Background(), 99, 9001, 2) // 99 不在群
	wantGuildCode(t, err, errcode.ErrGroupNotMember)
}

func TestLeaveGroup_OwnerForbidden(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", nil, 9001)
	err := uc.LeaveGroup(context.Background(), 1, 9001)
	wantGuildCode(t, err, errcode.ErrGroupNotOwner)
}

func TestLeaveGroup_MemberOK(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", []uint64{2}, 9001)
	if err := uc.LeaveGroup(context.Background(), 2, 9001); err != nil {
		t.Fatalf("member leave err: %v", err)
	}
	if _, ok := repo.members[memberKey{9001, 2}]; ok {
		t.Fatalf("member 2 should be gone")
	}
}

func TestKickFromGroup_OnlyOwner(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", []uint64{2, 3}, 9001)
	// 普通成员 2 踢 3 → 无权
	err := uc.KickFromGroup(context.Background(), 2, 9001, 3)
	wantGuildCode(t, err, errcode.ErrGroupNotOwner)
	// 群主 1 踢 3 → OK
	if err := uc.KickFromGroup(context.Background(), 1, 9001, 3); err != nil {
		t.Fatalf("owner kick err: %v", err)
	}
}

func TestKickFromGroup_CannotKickSelf(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", nil, 9001)
	err := uc.KickFromGroup(context.Background(), 1, 9001, 1)
	wantGuildCode(t, err, errcode.ErrInvalidArg)
}

func TestDisbandGroup_OnlyOwner(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", []uint64{2}, 9001)
	err := uc.DisbandGroup(context.Background(), 2, 9001) // 成员无权
	wantGuildCode(t, err, errcode.ErrGroupNotOwner)
	if err := uc.DisbandGroup(context.Background(), 1, 9001); err != nil {
		t.Fatalf("owner disband err: %v", err)
	}
	if _, ok := repo.groups[9001]; ok {
		t.Fatalf("group should be deleted")
	}
}

func TestTransferOwner_OK(t *testing.T) {
	repo := newFakeGroupRepo()
	uc := newGroupUC(repo)
	_, _ = uc.CreateGroup(context.Background(), 1, "g", []uint64{2}, 9001)
	if err := uc.TransferOwner(context.Background(), 1, 9001, 2); err != nil {
		t.Fatalf("transfer err: %v", err)
	}
	if m := repo.members[memberKey{9001, 2}]; m == nil || m.Role != data.GroupRoleOwner {
		t.Fatalf("2 should be owner")
	}
	if m := repo.members[memberKey{9001, 1}]; m == nil || m.Role != data.GroupRoleMember {
		t.Fatalf("1 should be demoted")
	}
}

func TestGetGroup_NotFound(t *testing.T) {
	uc := newGroupUC(newFakeGroupRepo())
	_, err := uc.GetGroup(context.Background(), 9999)
	wantGuildCode(t, err, errcode.ErrGroupNotFound)
}
