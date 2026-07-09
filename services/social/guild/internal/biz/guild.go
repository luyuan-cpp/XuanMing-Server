// Package biz 是 guild 服务的业务逻辑层(2026-06-27)。
//
// 职责(docs/design/decision-revisit-chat-group.md):
//   - 公会成员管理:创建 / 申请 / 审批 / 退会 / 踢人 / 解散 / 转让会长 / 任命官员 / 查询
//   - 权限:LEADER 解散 / 转让 / 任命 / 审批 / 踢任意成员;OFFICER 审批 / 踢普通成员;MEMBER 仅聊天 / 退会
//   - 单归属:玩家只能属于一个公会(DB guild_members.player_id 唯一 + 事务校验)
//   - 成员变更经 kafka pandora.guild.event → push 推送给接收方(弱依赖,nil 静默跳过)
//
// 关键规则:
//   - LEADER 不能直接退会 / 被踢:必须先 TransferLeader 或 DisbandGuild(否则公会无主)
//   - 推送原则 2:通知不回发操作者本人(申请通知发给会长 / 官员;审批结果发给申请人)
//   - nickname 留空:由客户端按 player_id 解析展示名(CLAUDE.md §5.8 最小数据单位)
//   - 客户端只拿可见结构(CLAUDE.md §14):RPC 只回 Guild / GuildMember / GuildJoinRequest
package biz

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	guildv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/guild/v1"

	"github.com/luyuancpp/pandora/services/social/guild/internal/conf"
	"github.com/luyuancpp/pandora/services/social/guild/internal/data"
)

// GuildEventPusher 把公会成员变更事件发到 kafka(main 注入;弱依赖,nil 时静默跳过)。
// kafka key = to_player_id(同接收方有序;push 服务按 key 路由到该玩家 stream)。
type GuildEventPusher interface {
	PushGuildEvent(ctx context.Context, toPlayerID uint64, evt *guildv1.GuildEvent) error
}

// GuildUsecase 是 guild 服务公会业务逻辑核心。
type GuildUsecase struct {
	repo   data.GuildRepo
	pusher GuildEventPusher // 弱依赖,可为 nil
	cfg    conf.GuildConf
}

// NewGuildUsecase 构造。pusher 允许为 nil(弱依赖未配置时降级)。
func NewGuildUsecase(repo data.GuildRepo, pusher GuildEventPusher, cfg conf.GuildConf) *GuildUsecase {
	return &GuildUsecase{repo: repo, pusher: pusher, cfg: cfg}
}

// CreateGuild 创建公会,创建者成为会长。newGuildID 由 service 用 snowflake 预生成。
func (u *GuildUsecase) CreateGuild(ctx context.Context, playerID uint64, name string, newGuildID uint64) (uint64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errcode.New(errcode.ErrInvalidArg, "guild name required")
	}
	if utf8.RuneCountInString(name) > u.cfg.MaxNameLen {
		return 0, errcode.New(errcode.ErrInvalidArg, "guild name too long")
	}
	if err := u.repo.CreateGuild(ctx, newGuildID, playerID, name, u.cfg.MaxGuildMembers); err != nil {
		return 0, err
	}
	return newGuildID, nil
}

// ApplyJoin 申请加入公会。newRequestID 由 service 预生成。
func (u *GuildUsecase) ApplyJoin(ctx context.Context, playerID, guildID, newRequestID uint64) (uint64, error) {
	if guildID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "guild_id required")
	}
	// 申请人不能已在任何公会(单归属)。
	if m, ok, err := u.repo.GetMember(ctx, playerID); err != nil {
		return 0, err
	} else if ok {
		return 0, errcode.New(errcode.ErrGuildAlreadyInGuild, "player %d already in guild %d", playerID, m.GuildID)
	}
	// 目标公会须存在。
	g, ok, err := u.repo.GetGuild(ctx, guildID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errcode.New(errcode.ErrGuildNotFound, "guild %d not found", guildID)
	}

	requestID, _, err := u.repo.CreateJoinRequest(ctx, newRequestID, guildID, playerID, u.cfg.MaxPendingRequestsPerGuild)
	if err != nil {
		return 0, err
	}

	// 推送:通知会长 / 官员有人申请(原则 2:不发申请人本人)。
	u.fanoutToManagers(ctx, guildID, &guildv1.GuildEvent{
		Type:      guildv1.GuildEventType_GUILD_EVENT_TYPE_JOIN_APPLIED,
		GuildId:   guildID,
		ActorId:   playerID,
		GuildName: g.Name,
	})
	return requestID, nil
}

// ApproveJoin 审批通过加入申请。approverID 须为该公会 LEADER / OFFICER(权威校验在事务内)。
func (u *GuildUsecase) ApproveJoin(ctx context.Context, approverID, requestID uint64) error {
	if requestID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "request_id required")
	}
	rq, ok, err := u.repo.GetRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.ErrGuildRequestInvalid, "request %d not found", requestID)
	}
	approved, err := u.repo.ApproveJoin(ctx, requestID, approverID, u.cfg.MaxGuildMembers)
	if err != nil {
		return err
	}
	if !approved {
		return errcode.New(errcode.ErrGuildRequestInvalid, "request %d not pending", requestID)
	}
	g, _, _ := u.repo.GetGuild(ctx, rq.GuildID)
	guildName := ""
	if g != nil {
		guildName = g.Name
	}
	// 通知申请人:通过(发给申请人本人)。
	u.push(ctx, rq.PlayerID, &guildv1.GuildEvent{
		Type:      guildv1.GuildEventType_GUILD_EVENT_TYPE_JOIN_APPROVED,
		GuildId:   rq.GuildID,
		ActorId:   approverID,
		GuildName: guildName,
	})
	return nil
}

// RejectJoin 拒绝加入申请。approverID 须为该公会 LEADER / OFFICER。
func (u *GuildUsecase) RejectJoin(ctx context.Context, approverID, requestID uint64) error {
	if requestID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "request_id required")
	}
	rq, ok, err := u.repo.GetRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.ErrGuildRequestInvalid, "request %d not found", requestID)
	}
	rejected, err := u.repo.RejectJoin(ctx, requestID, approverID)
	if err != nil {
		return err
	}
	if !rejected {
		return errcode.New(errcode.ErrGuildRequestInvalid, "request %d not pending", requestID)
	}
	u.push(ctx, rq.PlayerID, &guildv1.GuildEvent{
		Type:    guildv1.GuildEventType_GUILD_EVENT_TYPE_JOIN_REJECTED,
		GuildId: rq.GuildID,
		ActorId: approverID,
	})
	return nil
}

// LeaveGuild 退会。LEADER 不能直接退会(须先转让或解散)。
func (u *GuildUsecase) LeaveGuild(ctx context.Context, playerID uint64) error {
	m, ok, err := u.repo.GetMember(ctx, playerID)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.ErrGuildNotMember, "player %d not in any guild", playerID)
	}
	if m.Role == data.GuildRoleLeader {
		return errcode.New(errcode.ErrGuildNotLeader, "leader must transfer or disband before leaving")
	}
	return u.repo.RemoveMember(ctx, m.GuildID, playerID)
}

// KickMember 踢出成员。LEADER 可踢任意非会长成员;OFFICER 只能踢普通成员。不能踢自己 / 踢会长。
func (u *GuildUsecase) KickMember(ctx context.Context, operatorID, targetID uint64) error {
	if operatorID == targetID {
		return errcode.New(errcode.ErrInvalidArg, "cannot kick self")
	}
	op, ok, err := u.repo.GetMember(ctx, operatorID)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.ErrGuildNotMember, "operator %d not in any guild", operatorID)
	}
	target, ok, err := u.repo.GetMember(ctx, targetID)
	if err != nil {
		return err
	}
	if !ok || target.GuildID != op.GuildID {
		return errcode.New(errcode.ErrGuildNotMember, "target %d not in operator's guild", targetID)
	}
	if target.Role == data.GuildRoleLeader {
		return errcode.New(errcode.ErrGuildNoPermission, "cannot kick the leader")
	}
	switch op.Role {
	case data.GuildRoleLeader:
		// 可踢 officer / member
	case data.GuildRoleOfficer:
		if target.Role != data.GuildRoleMember {
			return errcode.New(errcode.ErrGuildNoPermission, "officer can only kick members")
		}
	default:
		return errcode.New(errcode.ErrGuildNoPermission, "member cannot kick")
	}
	if err := u.repo.RemoveMember(ctx, op.GuildID, targetID); err != nil {
		return err
	}
	g, _, _ := u.repo.GetGuild(ctx, op.GuildID)
	guildName := ""
	if g != nil {
		guildName = g.Name
	}
	u.push(ctx, targetID, &guildv1.GuildEvent{
		Type:      guildv1.GuildEventType_GUILD_EVENT_TYPE_KICKED,
		GuildId:   op.GuildID,
		ActorId:   operatorID,
		GuildName: guildName,
	})
	return nil
}

// DisbandGuild 解散公会。仅 LEADER。解散前先抓全体成员用于推送通知。
func (u *GuildUsecase) DisbandGuild(ctx context.Context, leaderID uint64) error {
	m, ok, err := u.repo.GetMember(ctx, leaderID)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.ErrGuildNotMember, "player %d not in any guild", leaderID)
	}
	if m.Role != data.GuildRoleLeader {
		return errcode.New(errcode.ErrGuildNotLeader, "only leader can disband")
	}
	g, _, _ := u.repo.GetGuild(ctx, m.GuildID)
	guildName := ""
	if g != nil {
		guildName = g.Name
	}
	members, _ := u.repo.ListMembers(ctx, m.GuildID, 0, 0)

	if err := u.repo.DisbandGuild(ctx, m.GuildID); err != nil {
		return err
	}
	// 通知全体成员(含会长本人,解散是全员事件,例外于原则 2)。
	for _, mem := range members {
		u.push(ctx, mem.PlayerID, &guildv1.GuildEvent{
			Type:      guildv1.GuildEventType_GUILD_EVENT_TYPE_DISBANDED,
			GuildId:   m.GuildID,
			ActorId:   leaderID,
			GuildName: guildName,
		})
	}
	return nil
}

// TransferLeader 转让会长。仅现任 LEADER;目标须为本公会成员且非自己。
func (u *GuildUsecase) TransferLeader(ctx context.Context, leaderID, targetID uint64) error {
	if leaderID == targetID {
		return errcode.New(errcode.ErrInvalidArg, "cannot transfer to self")
	}
	m, ok, err := u.repo.GetMember(ctx, leaderID)
	if err != nil {
		return err
	}
	if !ok || m.Role != data.GuildRoleLeader {
		return errcode.New(errcode.ErrGuildNotLeader, "only leader can transfer")
	}
	if err := u.repo.TransferLeader(ctx, m.GuildID, leaderID, targetID); err != nil {
		return err
	}
	g, _, _ := u.repo.GetGuild(ctx, m.GuildID)
	guildName := ""
	if g != nil {
		guildName = g.Name
	}
	members, _ := u.repo.ListMembers(ctx, m.GuildID, 0, 0)
	for _, mem := range members {
		u.push(ctx, mem.PlayerID, &guildv1.GuildEvent{
			Type:      guildv1.GuildEventType_GUILD_EVENT_TYPE_LEADER_CHANGED,
			GuildId:   m.GuildID,
			ActorId:   targetID,
			GuildName: guildName,
		})
	}
	return nil
}

// SetOfficer 任命 / 撤销官员。仅 LEADER;目标须为本公会成员且非自己。
func (u *GuildUsecase) SetOfficer(ctx context.Context, leaderID, targetID uint64, isOfficer bool) error {
	if leaderID == targetID {
		return errcode.New(errcode.ErrInvalidArg, "cannot set officer on self")
	}
	m, ok, err := u.repo.GetMember(ctx, leaderID)
	if err != nil {
		return err
	}
	if !ok || m.Role != data.GuildRoleLeader {
		return errcode.New(errcode.ErrGuildNotLeader, "only leader can set officer")
	}
	target, ok, err := u.repo.GetMember(ctx, targetID)
	if err != nil {
		return err
	}
	if !ok || target.GuildID != m.GuildID {
		return errcode.New(errcode.ErrGuildNotMember, "target %d not in guild", targetID)
	}
	if target.Role == data.GuildRoleLeader {
		return errcode.New(errcode.ErrGuildNoPermission, "target is leader")
	}
	role := int32(data.GuildRoleMember)
	if isOfficer {
		role = data.GuildRoleOfficer
	}
	return u.repo.SetRole(ctx, m.GuildID, targetID, role)
}

// GetGuild 查公会。不存在 → ErrGuildNotFound。
func (u *GuildUsecase) GetGuild(ctx context.Context, guildID uint64) (*guildv1.Guild, error) {
	g, ok, err := u.repo.GetGuild(ctx, guildID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errcode.New(errcode.ErrGuildNotFound, "guild %d not found", guildID)
	}
	return toGuildView(g), nil
}

// GetMyGuild 查玩家当前公会;不在任何公会返回 (nil, nil)(service 回 OK + 空)。
func (u *GuildUsecase) GetMyGuild(ctx context.Context, playerID uint64) (*guildv1.Guild, error) {
	g, ok, err := u.repo.GetMyGuild(ctx, playerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return toGuildView(g), nil
}

// 分页上限(决策:docs/design/decision-revisit-list-pagination.md)。
const (
	defaultPageLimit = 50
	maxPageLimit     = 100
)

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// ListMembers 列公会成员(客户端可见结构),按 player_id 升序游标分页。
func (u *GuildUsecase) ListMembers(ctx context.Context, guildID, cursor uint64, limit int) ([]*guildv1.GuildMember, uint64, error) {
	limit = clampLimit(limit)
	rows, err := u.repo.ListMembers(ctx, guildID, cursor, limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*guildv1.GuildMember, 0, len(rows))
	for _, m := range rows {
		out = append(out, &guildv1.GuildMember{
			PlayerId: m.PlayerID,
			Role:     guildv1.GuildRole(m.Role),
			JoinedMs: m.JoinedMs,
		})
	}
	var next uint64
	if len(rows) == limit {
		next = rows[len(rows)-1].PlayerID
	}
	return out, next, nil
}

// ListJoinRequests 列公会挂起申请。requesterID 须为该公会 LEADER / OFFICER。按 request_id 升序游标分页。
func (u *GuildUsecase) ListJoinRequests(ctx context.Context, requesterID, cursor uint64, limit int) ([]*guildv1.GuildJoinRequest, uint64, error) {
	limit = clampLimit(limit)
	m, ok, err := u.repo.GetMember(ctx, requesterID)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, errcode.New(errcode.ErrGuildNotMember, "player %d not in any guild", requesterID)
	}
	if m.Role != data.GuildRoleLeader && m.Role != data.GuildRoleOfficer {
		return nil, 0, errcode.New(errcode.ErrGuildNoPermission, "only leader/officer can list requests")
	}
	rows, err := u.repo.ListPendingRequests(ctx, m.GuildID, cursor, limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*guildv1.GuildJoinRequest, 0, len(rows))
	for _, rq := range rows {
		out = append(out, &guildv1.GuildJoinRequest{
			RequestId:    rq.RequestID,
			GuildId:      rq.GuildID,
			FromPlayerId: rq.PlayerID,
			CreatedMs:    rq.CreatedMs,
		})
	}
	var next uint64
	if len(rows) == limit {
		next = rows[len(rows)-1].RequestID
	}
	return out, next, nil
}

// fanoutToManagers 把事件推给公会的会长 + 官员(申请通知用),排除 evt.ActorId 本人。
func (u *GuildUsecase) fanoutToManagers(ctx context.Context, guildID uint64, evt *guildv1.GuildEvent) {
	if u.pusher == nil {
		return
	}
	members, err := u.repo.ListMembers(ctx, guildID, 0, 0)
	if err != nil {
		plog.With(ctx).Warnw("msg", "guild_fanout_managers_failed", "guild_id", guildID, "err", err)
		return
	}
	for _, m := range members {
		if m.Role != data.GuildRoleLeader && m.Role != data.GuildRoleOfficer {
			continue
		}
		if m.PlayerID == evt.GetActorId() {
			continue
		}
		u.push(ctx, m.PlayerID, evt)
	}
}

// push 发一条公会事件给接收方(弱依赖,nil / 失败只 warn)。
func (u *GuildUsecase) push(ctx context.Context, toPlayerID uint64, evt *guildv1.GuildEvent) {
	if u.pusher == nil || toPlayerID == 0 {
		return
	}
	e := &guildv1.GuildEvent{
		Type:       evt.GetType(),
		GuildId:    evt.GetGuildId(),
		ToPlayerId: toPlayerID,
		ActorId:    evt.GetActorId(),
		GuildName:  evt.GetGuildName(),
	}
	if err := u.pusher.PushGuildEvent(ctx, toPlayerID, e); err != nil {
		plog.With(ctx).Warnw("msg", "guild_push_failed",
			"to_player_id", toPlayerID, "type", evt.GetType(), "err", err)
	}
}

// toGuildView 把存储行映射成客户端可见 Guild(CLAUDE.md §14)。
func toGuildView(g *data.GuildRow) *guildv1.Guild {
	return &guildv1.Guild{
		GuildId:     g.GuildID,
		Name:        g.Name,
		LeaderId:    g.LeaderID,
		MemberCount: g.MemberCount,
		MaxMembers:  g.MaxMembers,
		CreatedMs:   g.CreatedMs,
	}
}
