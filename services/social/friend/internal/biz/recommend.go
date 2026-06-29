// 推荐好友策略链(2026-06-29)。
//
// 推荐 = 一组「可插拔策略」按序召回,直到凑够 limit。每种策略独立产候选,
// 互不耦合;加新策略只加一个实现 + conf 名,不改 RPC / RecommendFriends 主流程。
//
// 现有策略:
//   - mutual 熟人:好友的好友,按共同好友数降序(RecommendByMutual)
//   - random 兜底:好友图随机锚点扫(RecommendRandom),mutual 恒 0
//
// 未来策略(待 player 服务加 BatchGetProfile + region 后接,各自实现 RecommendStrategy):
//   - similar_power 实力相当:|Δmmr| 升序
//   - same_region 同区域:region 相同优先
package biz

import (
	"context"

	"github.com/luyuancpp/pandora/services/social/friend/internal/data"
)

// RecommendStrategy 是一种推荐召回策略。各策略独立产候选,排除条件由 repo 查询保证。
type RecommendStrategy interface {
	// Name 策略名(对齐 conf recommend_strategies 配置)。
	Name() string
	// Candidates 召回至多 limit 个候选;exclude 含调用方累计排除的 id(自己 / 已选)。
	Candidates(ctx context.Context, playerID uint64, exclude []uint64, limit int) ([]data.RecommendRow, error)
}

// mutualFriendStrategy 熟人:好友的好友,按共同好友数降序。
type mutualFriendStrategy struct{ repo data.FriendRepo }

func (s mutualFriendStrategy) Name() string { return "mutual" }
func (s mutualFriendStrategy) Candidates(ctx context.Context, playerID uint64, exclude []uint64, limit int) ([]data.RecommendRow, error) {
	return s.repo.RecommendByMutual(ctx, playerID, exclude, limit)
}

// randomStrategy 兜底:好友图随机锚点扫,mutual 恒 0。
type randomStrategy struct{ repo data.FriendRepo }

func (s randomStrategy) Name() string { return "random" }
func (s randomStrategy) Candidates(ctx context.Context, playerID uint64, exclude []uint64, limit int) ([]data.RecommendRow, error) {
	return s.repo.RecommendRandom(ctx, playerID, exclude, limit)
}

// buildStrategies 按 conf 名单装配策略链;未知名忽略;空名单退化为 [mutual, random]。
func buildStrategies(repo data.FriendRepo, names []string) []RecommendStrategy {
	if len(names) == 0 {
		names = []string{"mutual", "random"}
	}
	out := make([]RecommendStrategy, 0, len(names))
	for _, n := range names {
		switch n {
		case "mutual":
			out = append(out, mutualFriendStrategy{repo: repo})
		case "random":
			out = append(out, randomStrategy{repo: repo})
		}
	}
	if len(out) == 0 {
		out = append(out, mutualFriendStrategy{repo: repo}, randomStrategy{repo: repo})
	}
	return out
}
