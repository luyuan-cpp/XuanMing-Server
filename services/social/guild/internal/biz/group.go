// group.go 是 guild 服务里临时群(GroupService)的业务逻辑层(2026-06-27)。
//
// 职责(docs/design/decision-revisit-chat-group.md):
//   - 临时群成员管理:建群 / 拉人 / 退群 / 踢人 / 解散 / 转让群主 / 查询
//   - 权限:OWNER 拉人 / 踢人 / 解散 / 转让;MEMBER 聊天 / 退群 / 拉人(允许成员拉人)
//   - 多归属:玩家可同时在多个群
//   - 群聊即时频道,不落聊天历史;成员变更 MVP 不单独推送(客户端拉 ListMyGroups 兜底)
//
// 关键规则:
//   - OWNER 不能直接退群 / 被本人解散外退出:须先 TransferOwner 或 DisbandGroup
//   - 拉人幂等:已在群 → 返回成功(不重复加)
//   - nickname 留空,客户端按 player_id 解析(§5.8);客户端只拿可见结构(§14)
package biz

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/luyuancpp/pandora/pkg/errcode"
	groupv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/group/v1"

	"github.com/luyuancpp/pandora/services/social/guild/internal/conf"
	"github.com/luyuancpp/pandora/services/social/guild/internal/data"
)

// GroupUsecase 是 guild 服务临时群业务逻辑核心。
type GroupUsecase struct {
	repo data.GroupRepo
	cfg  conf.GuildConf
}

// NewGroupUsecase 构造。
func NewGroupUsecase(repo data.GroupRepo, cfg conf.GuildConf) *GroupUsecase {
	return &GroupUsecase{repo: repo, cfg: cfg}
}

// CreateGroup 建群,建群者成为群主。newGroupID 由 service 预生成。
// memberIDs 是初始成员(可空);服务端去重并排除建群者,总数 ≤ 上限。
func (u *GroupUsecase) CreateGroup(ctx context.Context, ownerID uint64, name string, memberIDs []uint64, newGroupID uint64) (uint64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errcode.New(errcode.ErrInvalidArg, "group name required")
	}
	if utf8.RuneCountInString(name) > u.cfg.MaxNameLen {
		return 0, errcode.New(errcode.ErrInvalidArg, "group name too long")
	}
	members := dedupExclude(memberIDs, ownerID)
	if err := u.repo.CreateGroup(ctx, newGroupID, ownerID, name, members, u.cfg.MaxGroupMembers, u.cfg.MaxGroupsPerPlayer); err != nil {
		return 0, err
	}
	return newGroupID, nil
}

// InviteToGroup 拉人入群。操作者须为群成员;目标已在群则幂等成功。
func (u *GroupUsecase) InviteToGroup(ctx context.Context, operatorID, groupID, targetID uint64) error {
	if groupID == 0 || targetID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "group_id and target_id required")
	}
	// 操作者须在群内。
	if _, ok, err := u.repo.GetGroupMember(ctx, groupID, operatorID); err != nil {
		return err
	} else if !ok {
		return errcode.New(errcode.ErrGroupNotMember, "operator %d not in group %d", operatorID, groupID)
	}
	_, err := u.repo.AddMember(ctx, groupID, operatorID, targetID, u.cfg.MaxGroupMembers, u.cfg.MaxGroupsPerPlayer)
	return err
}

// LeaveGroup 退群。OWNER 不能直接退群(须先转让或解散)。
func (u *GroupUsecase) LeaveGroup(ctx context.Context, playerID, groupID uint64) error {
	m, ok, err := u.repo.GetGroupMember(ctx, groupID, playerID)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.ErrGroupNotMember, "player %d not in group %d", playerID, groupID)
	}
	if m.Role == data.GroupRoleOwner {
		return errcode.New(errcode.ErrGroupNotOwner, "owner must transfer or disband before leaving")
	}
	return u.repo.RemoveMember(ctx, groupID, playerID)
}

// KickFromGroup 踢人。仅 OWNER;不能踢自己。
func (u *GroupUsecase) KickFromGroup(ctx context.Context, ownerID, groupID, targetID uint64) error {
	if ownerID == targetID {
		return errcode.New(errcode.ErrInvalidArg, "cannot kick self")
	}
	owner, ok, err := u.repo.GetGroupMember(ctx, groupID, ownerID)
	if err != nil {
		return err
	}
	if !ok || owner.Role != data.GroupRoleOwner {
		return errcode.New(errcode.ErrGroupNotOwner, "only owner can kick")
	}
	if _, ok, err := u.repo.GetGroupMember(ctx, groupID, targetID); err != nil {
		return err
	} else if !ok {
		return errcode.New(errcode.ErrGroupNotMember, "target %d not in group %d", targetID, groupID)
	}
	return u.repo.KickMember(ctx, groupID, ownerID, targetID)
}

// DisbandGroup 解散群。仅 OWNER。
func (u *GroupUsecase) DisbandGroup(ctx context.Context, ownerID, groupID uint64) error {
	owner, ok, err := u.repo.GetGroupMember(ctx, groupID, ownerID)
	if err != nil {
		return err
	}
	if !ok || owner.Role != data.GroupRoleOwner {
		return errcode.New(errcode.ErrGroupNotOwner, "only owner can disband")
	}
	return u.repo.DisbandGroup(ctx, groupID, ownerID)
}

// TransferOwner 转让群主。仅现任 OWNER;目标须为本群成员且非自己。
func (u *GroupUsecase) TransferOwner(ctx context.Context, ownerID, groupID, targetID uint64) error {
	if ownerID == targetID {
		return errcode.New(errcode.ErrInvalidArg, "cannot transfer to self")
	}
	owner, ok, err := u.repo.GetGroupMember(ctx, groupID, ownerID)
	if err != nil {
		return err
	}
	if !ok || owner.Role != data.GroupRoleOwner {
		return errcode.New(errcode.ErrGroupNotOwner, "only owner can transfer")
	}
	return u.repo.TransferOwner(ctx, groupID, ownerID, targetID)
}

// GetGroup 查群。不存在 → ErrGroupNotFound。
func (u *GroupUsecase) GetGroup(ctx context.Context, groupID uint64) (*groupv1.Group, error) {
	g, ok, err := u.repo.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errcode.New(errcode.ErrGroupNotFound, "group %d not found", groupID)
	}
	return toGroupView(g), nil
}

// ListGroupMembers 列群成员(客户端可见结构)。
func (u *GroupUsecase) ListGroupMembers(ctx context.Context, groupID uint64) ([]*groupv1.GroupMember, error) {
	rows, err := u.repo.ListGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]*groupv1.GroupMember, 0, len(rows))
	for _, m := range rows {
		out = append(out, &groupv1.GroupMember{
			PlayerId: m.PlayerID,
			Role:     groupv1.GroupRole(m.Role),
			JoinedMs: m.JoinedMs,
		})
	}
	return out, nil
}

// ListMyGroups 列玩家所在的群。
func (u *GroupUsecase) ListMyGroups(ctx context.Context, playerID uint64) ([]*groupv1.Group, error) {
	rows, err := u.repo.ListMyGroups(ctx, playerID)
	if err != nil {
		return nil, err
	}
	out := make([]*groupv1.Group, 0, len(rows))
	for i := range rows {
		out = append(out, toGroupView(&rows[i]))
	}
	return out, nil
}

// dedupExclude 去重并排除 exclude(建群初始成员清洗)。
func dedupExclude(ids []uint64, exclude uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 || id == exclude {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// toGroupView 把存储行映射成客户端可见 Group(CLAUDE.md §14)。
func toGroupView(g *data.GroupRow) *groupv1.Group {
	return &groupv1.Group{
		GroupId:     g.GroupID,
		Name:        g.Name,
		OwnerId:     g.OwnerID,
		MemberCount: g.MemberCount,
		MaxMembers:  g.MaxMembers,
		CreatedMs:   g.CreatedMs,
	}
}
