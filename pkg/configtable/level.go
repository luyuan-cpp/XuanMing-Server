package configtable

import (
	"fmt"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// 关卡表手写伴生文件:表私有校验钩子 + 域方法。
// 视图结构与通用访问 API(All/ByID/Exists/Count/ByIDs/RandOne/Where/First)在
// level_table.gen.go(tools/configtable-gen 生成,勿手改)。

// MaxLevelTeamSize 是关卡表「队伍人数」上限(团队级硬约束,防撮合预分配爆内存)。
// team_size 是裸 uint32、支持热更,撮合按 need=2*teamSize 预分配票据切片
// (matchmaker greedyFormMatches);若允许任意大值,热更一张超大 team_size 且该 map
// 有票即可触发巨量 make([]T,0,need) 直接打爆 matchmaker(§16.5 容量/溢出边界)。
// 基线是 5v5;取 50(单局最多 100 人)给未来大型玩法留 10x 余量,同时把预分配钳在可控量级。
// 0 是合法值(表示沿用服务端全局 team_size 兜底),只挡上界。
const MaxLevelTeamSize = 50

// validateLevelRow 逐行业务校验(生成的 newLevelTable 调用;主键非零/唯一已由生成代码兜住)。
// 与生成阶段校验重复是有意的 fail-closed:服务端不信任产物一定出自本生成器。
func validateLevelRow(row *configpb.LevelRow) error {
	if row.GetAssetPath() == "" {
		return fmt.Errorf("关卡资源(asset_path)为空")
	}
	if row.GetCategory() == configpb.LevelCategory_LEVEL_CATEGORY_UNSPECIFIED {
		return fmt.Errorf("关卡类别(category)未填")
	}
	// 上限校验:整表全批校验一处失败即拒新版本、保留旧表(§9.15 加载失败不切换),
	// 把「热更超大 team_size 打爆撮合」挡在加载边界,而不是等撮合时预分配爆内存。
	if ts := row.GetTeamSize(); ts > MaxLevelTeamSize {
		return fmt.Errorf("队伍人数(team_size=%d)超过上限 %d(防撮合预分配爆内存)", ts, MaxLevelTeamSize)
	}
	return nil
}

// IsBattleLevel 是否「存在且为战斗 / 副本类关卡」——匹配入口的 map_id 准入门:
// 只挡类别错误(登录 / 选角 / 主城不可开局),不看「匹配列表显示」列,
// 后者是客户端 UI 展示位,dev / GM 直进测试关卡(如 4 战斗测试)仍须放行。
func (t *LevelTable) IsBattleLevel(id uint32) bool {
	row, ok := t.byID[id]
	return ok && row.GetCategory() == configpb.LevelCategory_LEVEL_CATEGORY_BATTLE
}
