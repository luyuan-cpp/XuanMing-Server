// Package service 是 leaderboard 服务的 gRPC service 层(2026-06-27)。
//
// 职责:
//   - 实现 leaderboardv1.LeaderboardServiceServer
//   - proto Request/Response ↔ biz / data 入参出参互转
//   - errcode.Code → commonv1.ErrCode 1:1 映射
//
// 鉴权原则:
//   - 写入 / 系统 RPC(SubmitScore / SettleBoard / RemoveEntry / DeleteBoard):只允许后端内部直连
//     (无 JWT,callerID==0);带玩家 JWT 的客户端调用一律拒绝(同 inventory.GrantItems),
//     且不在 Envoy 暴露这些路由。
//   - 读 RPC(GetRank / GetRange / GetAround):允许经 Envoy 的客户端调用,只回客户端可见结构。
package service

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	leaderboardv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/leaderboard/v1"

	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/biz"
	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/data"
)

// LeaderboardService 实现 leaderboardv1.LeaderboardServiceServer。
type LeaderboardService struct {
	leaderboardv1.UnimplementedLeaderboardServiceServer
	uc *biz.LeaderboardUsecase
}

// NewLeaderboardService 构造。
func NewLeaderboardService(uc *biz.LeaderboardUsecase) *LeaderboardService {
	return &LeaderboardService{uc: uc}
}

// SubmitScore 上报分数(系统接口,拒绝玩家 JWT)。
func (s *LeaderboardService) SubmitScore(ctx context.Context, req *leaderboardv1.SubmitScoreRequest) (*leaderboardv1.SubmitScoreResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &leaderboardv1.SubmitScoreResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	b, ok := toBoardKey(req.GetBoard())
	if !ok {
		return &leaderboardv1.SubmitScoreResponse{Code: commonv1.ErrCode_ERR_LEADERBOARD_INVALID_BOARD}, nil
	}
	opt := toOptions(req.GetOptions())
	newScore, rank, err := s.uc.SubmitScore(ctx, b, req.GetEntityId(), req.GetScore(), int32(req.GetMode()), opt)
	if err != nil {
		return &leaderboardv1.SubmitScoreResponse{Code: toProtoCode(err)}, nil
	}
	return &leaderboardv1.SubmitScoreResponse{Code: commonv1.ErrCode_OK, NewScore: newScore, Rank: rank}, nil
}

// GetRank 查名次(读接口,允许客户端)。精确榜命中回精确名次;被截断玩家回区间估算(estimated=true)。
func (s *LeaderboardService) GetRank(ctx context.Context, req *leaderboardv1.GetRankRequest) (*leaderboardv1.GetRankResponse, error) {
	b, ok := toBoardKey(req.GetBoard())
	if !ok {
		return &leaderboardv1.GetRankResponse{Code: commonv1.ErrCode_ERR_LEADERBOARD_INVALID_BOARD}, nil
	}
	view, err := s.uc.GetRank(ctx, b, req.GetEntityId())
	if err != nil {
		return &leaderboardv1.GetRankResponse{Code: toProtoCode(err)}, nil
	}
	resp := &leaderboardv1.GetRankResponse{Code: commonv1.ErrCode_OK, Found: view.Found}
	if view.Found {
		resp.Entry = toEntry(view.Entry)
		resp.Estimated = view.Estimated
		resp.TotalSubmitters = view.TotalSubmitters
	}
	return resp, nil
}

// GetRange 取榜区间(读接口)。
func (s *LeaderboardService) GetRange(ctx context.Context, req *leaderboardv1.GetRangeRequest) (*leaderboardv1.GetRangeResponse, error) {
	b, ok := toBoardKey(req.GetBoard())
	if !ok {
		return &leaderboardv1.GetRangeResponse{Code: commonv1.ErrCode_ERR_LEADERBOARD_INVALID_BOARD}, nil
	}
	entries, total, err := s.uc.GetRange(ctx, b, req.GetOffset(), int(req.GetLimit()))
	if err != nil {
		return &leaderboardv1.GetRangeResponse{Code: toProtoCode(err)}, nil
	}
	return &leaderboardv1.GetRangeResponse{Code: commonv1.ErrCode_OK, Entries: toEntries(entries), Total: total}, nil
}

// GetAround 取上下 N 名(读接口)。
func (s *LeaderboardService) GetAround(ctx context.Context, req *leaderboardv1.GetAroundRequest) (*leaderboardv1.GetAroundResponse, error) {
	b, ok := toBoardKey(req.GetBoard())
	if !ok {
		return &leaderboardv1.GetAroundResponse{Code: commonv1.ErrCode_ERR_LEADERBOARD_INVALID_BOARD}, nil
	}
	entries, found, err := s.uc.GetAround(ctx, b, req.GetEntityId(), int(req.GetRadius()))
	if err != nil {
		return &leaderboardv1.GetAroundResponse{Code: toProtoCode(err)}, nil
	}
	return &leaderboardv1.GetAroundResponse{Code: commonv1.ErrCode_OK, Entries: toEntries(entries), Found: found}, nil
}

// RemoveEntry 移除某 entity(系统接口)。
func (s *LeaderboardService) RemoveEntry(ctx context.Context, req *leaderboardv1.RemoveEntryRequest) (*leaderboardv1.RemoveEntryResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &leaderboardv1.RemoveEntryResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	b, ok := toBoardKey(req.GetBoard())
	if !ok {
		return &leaderboardv1.RemoveEntryResponse{Code: commonv1.ErrCode_ERR_LEADERBOARD_INVALID_BOARD}, nil
	}
	if err := s.uc.RemoveEntry(ctx, b, req.GetEntityId()); err != nil {
		return &leaderboardv1.RemoveEntryResponse{Code: toProtoCode(err)}, nil
	}
	return &leaderboardv1.RemoveEntryResponse{Code: commonv1.ErrCode_OK}, nil
}

// SettleBoard 结算 + 发奖(系统接口)。
func (s *LeaderboardService) SettleBoard(ctx context.Context, req *leaderboardv1.SettleBoardRequest) (*leaderboardv1.SettleBoardResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &leaderboardv1.SettleBoardResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	b, ok := toBoardKey(req.GetBoard())
	if !ok {
		return &leaderboardv1.SettleBoardResponse{Code: commonv1.ErrCode_ERR_LEADERBOARD_INVALID_BOARD}, nil
	}
	res, err := s.uc.SettleBoard(ctx, b, int(req.GetTopN()), req.GetRewardTable(), req.GetResetAfter(), req.GetSettleIdempotencyKey())
	if err != nil {
		return &leaderboardv1.SettleBoardResponse{Code: toProtoCode(err)}, nil
	}
	return &leaderboardv1.SettleBoardResponse{
		Code:           commonv1.ErrCode_OK,
		SettlementId:   res.SettlementID,
		SettledCount:   res.SettledCount,
		AlreadySettled: res.AlreadySettled,
		Winners:        toEntries(res.Winners),
	}, nil
}

// DeleteBoard 删整个榜(系统接口)。
func (s *LeaderboardService) DeleteBoard(ctx context.Context, req *leaderboardv1.DeleteBoardRequest) (*leaderboardv1.DeleteBoardResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &leaderboardv1.DeleteBoardResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	b, ok := toBoardKey(req.GetBoard())
	if !ok {
		return &leaderboardv1.DeleteBoardResponse{Code: commonv1.ErrCode_ERR_LEADERBOARD_INVALID_BOARD}, nil
	}
	if err := s.uc.DeleteBoard(ctx, b); err != nil {
		return &leaderboardv1.DeleteBoardResponse{Code: toProtoCode(err)}, nil
	}
	return &leaderboardv1.DeleteBoardResponse{Code: commonv1.ErrCode_OK}, nil
}

// ── 转换辅助 ──────────────────────────────────────────────────────────────────

// toBoardKey 把 proto BoardKey 转 data.BoardKey;board_type==0 / scope 非法 → ok=false。
func toBoardKey(pb *leaderboardv1.BoardKey) (data.BoardKey, bool) {
	if pb == nil || pb.GetBoardType() == 0 {
		return data.BoardKey{}, false
	}
	scope := data.Scope(pb.GetScope())
	if scope < data.ScopeGlobal || scope > data.ScopeCustom {
		return data.BoardKey{}, false
	}
	return data.BoardKey{
		BoardType: pb.GetBoardType(),
		Scope:     scope,
		ScopeID:   pb.GetScopeId(),
		Period:    pb.GetPeriod(),
	}, true
}

// toOptions 把 proto BoardOptions 转 data.Options。
func toOptions(pb *leaderboardv1.BoardOptions) data.Options {
	if pb == nil {
		return data.Options{}
	}
	return data.Options{
		TTLSeconds:          pb.GetTtlSeconds(),
		MaxSize:             pb.GetMaxSize(),
		TieBreakByTime:      pb.GetTieBreakByTime(),
		Ascending:           pb.GetAscending(),
		EstimateBucketWidth: pb.GetEstimateBucketWidth(),
	}
}

// toEntry 把 data.Entry 转 proto LeaderboardEntry。
func toEntry(e data.Entry) *leaderboardv1.LeaderboardEntry {
	return &leaderboardv1.LeaderboardEntry{
		EntityId:    e.EntityID,
		Score:       e.Score,
		Rank:        e.Rank,
		UpdatedAtMs: e.UpdatedAtMs,
	}
}

// toEntries 批量转换。
func toEntries(es []data.Entry) []*leaderboardv1.LeaderboardEntry {
	out := make([]*leaderboardv1.LeaderboardEntry, 0, len(es))
	for _, e := range es {
		out = append(out, toEntry(e))
	}
	return out
}

// toProtoCode 把 pkg/errcode 1:1 映射成 proto enum(数值相同)。
func toProtoCode(err error) commonv1.ErrCode {
	return commonv1.ErrCode(errcode.As(err))
}
