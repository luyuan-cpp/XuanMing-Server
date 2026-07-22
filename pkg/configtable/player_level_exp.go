package configtable

import (
	"fmt"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// player_level_exp.go — PlayerLevelExpTable 手写伴生文件。
// 首次由 configtable-gen 创建(仅当文件不存在),此后归人维护,生成器不再覆盖。
// 表私有的逐行业务校验写在 validatePlayerLevelExpRow;域方法(业务语义查询)也加在本文件。

// validatePlayerLevelExpRow 逐行业务校验(生成的 newPlayerLevelExpTable 调用;
// 主键非零/唯一已由生成代码兜住,类型/必填/枚举已由生成器在导表阶段校验,
// 这里只写服务端仍须 fail-closed 的业务约束;无约束保持 return nil)。
func validatePlayerLevelExpRow(row *configpb.PlayerLevelExpRow) error {
	if row.GetId() != row.GetLevel() {
		return fmt.Errorf("ID %d 必须与等级 %d 一致", row.GetId(), row.GetLevel())
	}
	return nil
}

// ValidateCurve 校验玩家等级表的整表业务不变量。调用方必须把它注册为 Store validator，
// 使启动首载与每次热更都走同一门禁，坏批次不会替换当前快照。
func (t *PlayerLevelExpTable) ValidateCurve() error {
	if t == nil || t.Count() == 0 {
		return fmt.Errorf("玩家等级经验表为空")
	}
	if t.Count() < 2 {
		return fmt.Errorf("玩家等级经验表至少需要 Lv1-Lv2,实为 %d 级", t.Count())
	}
	if t.Count() > 200 {
		return fmt.Errorf("玩家等级经验表等级数 %d 超过上限 200", t.Count())
	}

	var expectedCumulative uint64
	for level := uint32(1); level <= uint32(t.Count()); level++ {
		row, ok := t.ByID(level)
		if !ok {
			return fmt.Errorf("玩家等级经验表缺少 Lv%d(等级必须从 1 连续递增)", level)
		}
		if row.GetLevel() != level {
			return fmt.Errorf("玩家等级经验表 ID=%d 的等级为 %d", level, row.GetLevel())
		}
		if uint64(row.GetCumulativeExp()) != expectedCumulative {
			return fmt.Errorf("Lv%d 到达本级累计经验=%d,按前级累计应为 %d",
				level, row.GetCumulativeExp(), expectedCumulative)
		}

		isMax := level == uint32(t.Count())
		if isMax {
			if row.GetUpgradeExp() != 0 {
				return fmt.Errorf("末级 Lv%d 的升级所需经验必须为 0,实为 %d", level, row.GetUpgradeExp())
			}
			continue
		}
		if row.GetUpgradeExp() == 0 {
			return fmt.Errorf("非末级 Lv%d 的升级所需经验必须大于 0", level)
		}
		expectedCumulative += uint64(row.GetUpgradeExp())
		if expectedCumulative > uint64(^uint32(0)) {
			return fmt.Errorf("Lv%d 后累计经验 %d 超过 uint32 上限", level, expectedCumulative)
		}
	}
	return nil
}

// ExperienceCurve 返回 AdvanceExperience 使用的 Lv1→末级曲线快照。
// 表在 Store.Load 时已通过 ValidateCurve；返回新切片，调用方可安全持有到本次事务结束。
func (t *PlayerLevelExpTable) ExperienceCurve() []uint64 {
	if t == nil || t.Count() < 2 {
		return nil
	}
	curve := make([]uint64, 0, t.Count()-1)
	for level := uint32(1); level < uint32(t.Count()); level++ {
		row, ok := t.ByID(level)
		if !ok {
			return nil
		}
		curve = append(curve, uint64(row.GetUpgradeExp()))
	}
	return curve
}

// MaxLevel 返回当前批次的最高玩家等级。
func (t *PlayerLevelExpTable) MaxLevel() int32 {
	if t == nil || t.Count() == 0 {
		return 0
	}
	return int32(t.Count())
}
