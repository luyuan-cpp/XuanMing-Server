// Package service 是 player 服务的 gRPC service 层(W4 ④,2026-06-06)。
//
// 职责:
//   - 实现 playerv1.PlayerServiceServer 接口
//   - proto Request/Response ↔ biz 入参/出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 说明:调用方为后端内部(battle_result GetMMR)/ 经 Envoy 的客户端(GetProfile 等),
// player_id 由 proto 字段显式传入(不从 ctx 取),鉴权由 Envoy jwt_authn 完成。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"

	"github.com/luyuancpp/pandora/services/account/player/internal/biz"
	"github.com/luyuancpp/pandora/services/account/player/internal/data"
)

// PlayerService 实现 playerv1.PlayerServiceServer。
type PlayerService struct {
	playerv1.UnimplementedPlayerServiceServer
	uc *biz.PlayerUsecase
}

// NewPlayerService 构造。
func NewPlayerService(uc *biz.PlayerUsecase) *PlayerService {
	return &PlayerService{uc: uc}
}

// GetProfile 读玩家档案(懒创建)。
func (s *PlayerService) GetProfile(ctx context.Context, req *playerv1.GetProfileRequest) (*playerv1.GetProfileResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GetProfileResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	profile, err := s.uc.GetProfile(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.GetProfileResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.GetProfileResponse{Code: commonv1.ErrCode_OK, Profile: profile}, nil
}

// UpdateNickname 改昵称。
func (s *PlayerService) UpdateNickname(ctx context.Context, req *playerv1.UpdateNicknameRequest) (*playerv1.UpdateNicknameResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.UpdateNicknameResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.UpdateNickname(ctx, req.GetPlayerId(), req.GetNickname()); err != nil {
		return &playerv1.UpdateNicknameResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.UpdateNicknameResponse{Code: commonv1.ErrCode_OK}, nil
}

// ListHeroes 列出玩家已解锁英雄。
func (s *PlayerService) ListHeroes(ctx context.Context, req *playerv1.ListHeroesRequest) (*playerv1.ListHeroesResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.ListHeroesResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	heroes, err := s.uc.ListHeroes(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.ListHeroesResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.ListHeroesResponse{Code: commonv1.ErrCode_OK, HeroIds: heroes}, nil
}

// UnlockHero 解锁英雄。
func (s *PlayerService) UnlockHero(ctx context.Context, req *playerv1.UnlockHeroRequest) (*playerv1.UnlockHeroResponse, error) {
	if req.GetPlayerId() == 0 || req.GetHeroId() == 0 {
		return &playerv1.UnlockHeroResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.UnlockHero(ctx, req.GetPlayerId(), req.GetHeroId(), req.GetSource()); err != nil {
		return &playerv1.UnlockHeroResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.UnlockHeroResponse{Code: commonv1.ErrCode_OK}, nil
}

// GetMMR 读玩家当前 MMR(供 battle_result 当 reader)。
func (s *PlayerService) GetMMR(ctx context.Context, req *playerv1.GetMMRRequest) (*playerv1.GetMMRResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GetMMRResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	mmr, err := s.uc.GetMMR(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.GetMMRResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.GetMMRResponse{Code: commonv1.ErrCode_OK, Mmr: int32(mmr)}, nil
}

// UpdateMMR 幂等改 MMR(同步兜底;正常链路走 kafka 消费 player.update)。
func (s *PlayerService) UpdateMMR(ctx context.Context, req *playerv1.UpdateMMRRequest) (*playerv1.UpdateMMRResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.UpdateMMRResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if req.GetIdempotencyKey() == "" {
		return &playerv1.UpdateMMRResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	newMMR, _, err := s.uc.UpdateMMR(ctx, req.GetPlayerId(), req.GetDelta(), req.GetReason(), req.GetIdempotencyKey())
	if err != nil {
		return &playerv1.UpdateMMRResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.UpdateMMRResponse{Code: commonv1.ErrCode_OK, NewMmr: int32(newMMR)}, nil
}

// ── 出战养成 ──────────────────────────────────────────────────────────────────

// SelectHero 设定出战英雄。
func (s *PlayerService) SelectHero(ctx context.Context, req *playerv1.SelectHeroRequest) (*playerv1.SelectHeroResponse, error) {
	if req.GetPlayerId() == 0 || req.GetHeroId() == 0 {
		return &playerv1.SelectHeroResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	if err := s.uc.SelectHero(ctx, req.GetPlayerId(), req.GetHeroId()); err != nil {
		return &playerv1.SelectHeroResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.SelectHeroResponse{Code: commonv1.ErrCode_OK}, nil
}

// GetActiveHero 读出战英雄(未选定返回 hero_id=0)。
func (s *PlayerService) GetActiveHero(ctx context.Context, req *playerv1.GetActiveHeroRequest) (*playerv1.GetActiveHeroResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GetActiveHeroResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	heroID, err := s.uc.GetActiveHero(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.GetActiveHeroResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.GetActiveHeroResponse{Code: commonv1.ErrCode_OK, HeroId: heroID}, nil
}

// GrantAttributePoints 幂等授予可分配点。
func (s *PlayerService) GrantAttributePoints(ctx context.Context, req *playerv1.GrantAttributePointsRequest) (*playerv1.GrantAttributePointsResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GrantAttributePointsResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	unspent, err := s.uc.GrantAttributePoints(ctx, req.GetPlayerId(), req.GetPoints(), req.GetIdempotencyKey())
	if err != nil {
		return &playerv1.GrantAttributePointsResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.GrantAttributePointsResponse{Code: commonv1.ErrCode_OK, UnspentPoints: int32(unspent)}, nil
}

// AllocateAttributePoints 分配属性点。
func (s *PlayerService) AllocateAttributePoints(ctx context.Context, req *playerv1.AllocateAttributePointsRequest) (*playerv1.AllocateAttributePointsResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.AllocateAttributePointsResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	allocs := make([]data.AttrAllocation, 0, len(req.GetAllocations()))
	for _, a := range req.GetAllocations() {
		allocs = append(allocs, data.AttrAllocation{Key: a.GetAttrKey(), Points: a.GetPoints()})
	}
	unspent, err := s.uc.AllocateAttributePoints(ctx, req.GetPlayerId(), allocs)
	if err != nil {
		return &playerv1.AllocateAttributePointsResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.AllocateAttributePointsResponse{Code: commonv1.ErrCode_OK, UnspentPoints: int32(unspent)}, nil
}

// ResetAttributes 洗点。
func (s *PlayerService) ResetAttributes(ctx context.Context, req *playerv1.ResetAttributesRequest) (*playerv1.ResetAttributesResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.ResetAttributesResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	unspent, err := s.uc.ResetAttributes(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.ResetAttributesResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.ResetAttributesResponse{Code: commonv1.ErrCode_OK, UnspentPoints: int32(unspent)}, nil
}

// GetAttributes 读已分配属性点 + 未分配点。
func (s *PlayerService) GetAttributes(ctx context.Context, req *playerv1.GetAttributesRequest) (*playerv1.GetAttributesResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GetAttributesResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	attrs, unspent, err := s.uc.GetAttributes(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.GetAttributesResponse{Code: toProtoCode(err)}, nil
	}
	out := make([]*playerv1.AttributeAllocation, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, &playerv1.AttributeAllocation{AttrKey: a.Key, Points: a.Points})
	}
	return &playerv1.GetAttributesResponse{Code: commonv1.ErrCode_OK, Attributes: out, UnspentPoints: int32(unspent)}, nil
}

// SetEquipment 全量替换出战装备预设。
func (s *PlayerService) SetEquipment(ctx context.Context, req *playerv1.SetEquipmentRequest) (*playerv1.SetEquipmentResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.SetEquipmentResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	slots := make([]data.EquipmentSlot, 0, len(req.GetEquipment()))
	for _, e := range req.GetEquipment() {
		slots = append(slots, data.EquipmentSlot{Slot: e.GetSlot(), ItemConfigID: e.GetItemConfigId()})
	}
	if err := s.uc.SetEquipment(ctx, req.GetPlayerId(), slots); err != nil {
		return &playerv1.SetEquipmentResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.SetEquipmentResponse{Code: commonv1.ErrCode_OK}, nil
}

// GetEquipment 读出战装备预设。
func (s *PlayerService) GetEquipment(ctx context.Context, req *playerv1.GetEquipmentRequest) (*playerv1.GetEquipmentResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GetEquipmentResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	slots, err := s.uc.GetEquipment(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.GetEquipmentResponse{Code: toProtoCode(err)}, nil
	}
	out := make([]*playerv1.LoadoutEquipment, 0, len(slots))
	for _, sl := range slots {
		out = append(out, &playerv1.LoadoutEquipment{Slot: sl.Slot, ItemConfigId: sl.ItemConfigID})
	}
	return &playerv1.GetEquipmentResponse{Code: commonv1.ErrCode_OK, Equipment: out}, nil
}

// GrantTalentPoints 幂等授予天赋点。
func (s *PlayerService) GrantTalentPoints(ctx context.Context, req *playerv1.GrantTalentPointsRequest) (*playerv1.GrantTalentPointsResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GrantTalentPointsResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	unspent, err := s.uc.GrantTalentPoints(ctx, req.GetPlayerId(), req.GetPoints(), req.GetIdempotencyKey())
	if err != nil {
		return &playerv1.GrantTalentPointsResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.GrantTalentPointsResponse{Code: commonv1.ErrCode_OK, UnspentPoints: int32(unspent)}, nil
}

// SetTalents 全量重置天赋分配。
func (s *PlayerService) SetTalents(ctx context.Context, req *playerv1.SetTalentsRequest) (*playerv1.SetTalentsResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.SetTalentsResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	talents := make([]data.TalentLevel, 0, len(req.GetTalents()))
	for _, t := range req.GetTalents() {
		talents = append(talents, data.TalentLevel{TalentID: t.GetTalentId(), Level: t.GetLevel()})
	}
	unspent, err := s.uc.SetTalents(ctx, req.GetPlayerId(), talents)
	if err != nil {
		return &playerv1.SetTalentsResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.SetTalentsResponse{Code: commonv1.ErrCode_OK, UnspentPoints: int32(unspent)}, nil
}

// ResetTalents 清空天赋分配。
func (s *PlayerService) ResetTalents(ctx context.Context, req *playerv1.ResetTalentsRequest) (*playerv1.ResetTalentsResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.ResetTalentsResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	unspent, err := s.uc.ResetTalents(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.ResetTalentsResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.ResetTalentsResponse{Code: commonv1.ErrCode_OK, UnspentPoints: int32(unspent)}, nil
}

// GetTalents 读已点天赋 + 可点天赋点。
func (s *PlayerService) GetTalents(ctx context.Context, req *playerv1.GetTalentsRequest) (*playerv1.GetTalentsResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GetTalentsResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	talents, unspent, err := s.uc.GetTalents(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.GetTalentsResponse{Code: toProtoCode(err)}, nil
	}
	out := make([]*playerv1.TalentNode, 0, len(talents))
	for _, t := range talents {
		out = append(out, &playerv1.TalentNode{TalentId: t.TalentID, Level: t.Level})
	}
	return &playerv1.GetTalentsResponse{Code: commonv1.ErrCode_OK, Talents: out, UnspentPoints: int32(unspent)}, nil
}

// GetLoadout 组装开战前快照(出战英雄 + 属性点 + 装备预设 + 天赋)。
func (s *PlayerService) GetLoadout(ctx context.Context, req *playerv1.GetLoadoutRequest) (*playerv1.GetLoadoutResponse, error) {
	if req.GetPlayerId() == 0 {
		return &playerv1.GetLoadoutResponse{Code: commonv1.ErrCode_ERR_INVALID_ARG}, nil
	}
	loadout, err := s.uc.GetLoadout(ctx, req.GetPlayerId())
	if err != nil {
		return &playerv1.GetLoadoutResponse{Code: toProtoCode(err)}, nil
	}
	return &playerv1.GetLoadoutResponse{Code: commonv1.ErrCode_OK, Loadout: loadout}, nil
}

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
