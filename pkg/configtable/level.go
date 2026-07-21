package configtable

import (
	"fmt"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// 关卡表手写伴生文件:表私有校验钩子 + 域方法。
// 视图结构与通用访问 API(All/ByID/Exists/Count/ByIDs/RandOne/Where/First)在
// level_table.gen.go(tools/configtable-gen 生成,勿手改)。

// validateLevelRow 逐行业务校验(生成的 newLevelTable 调用;主键非零/唯一已由生成代码兜住)。
// 与生成阶段校验重复是有意的 fail-closed:服务端不信任产物一定出自本生成器。
func validateLevelRow(row *configpb.LevelRow) error {
	if row.GetAssetPath() == "" {
		return fmt.Errorf("关卡资源(asset_path)为空")
	}
	if row.GetCategory() == configpb.LevelCategory_LEVEL_CATEGORY_UNSPECIFIED {
		return fmt.Errorf("关卡类别(category)未填")
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
