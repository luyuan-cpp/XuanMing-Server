// bag_migrate.go — 旧 inventory 存量迁移用例
// (decision-revisit-bag-replay-semantics.md D5,bag-domain.md §10 phase 3)。
//
// 职责:游标批量枚举有存量的玩家 → 读 legacy 快照 → bag 库幂等落位仓库段 → 迁后对账。
// 单玩家失败只计数告警不阻断(bound 实例 fail-closed 等属预期拦截,逐个排障);
// 全部玩家跑完即收敛,重跑 no-op。配置门 legacy_migration_enabled 默认关,contract 阶段
// 旧写路径冻结后才准开启(时序纪律见 data/bag_migration.go 文件头)。
package biz

import (
	"context"

	"github.com/go-kratos/kratos/v2/log"

	"github.com/luyuancpp/pandora/pkg/errcode"
	bagv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/bag/v1"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

// LegacyBagSource 是 legacy 存量读取抽象(pandora_trade 库;MySQLInventoryRepo 实现)。
type LegacyBagSource interface {
	ListLegacyBagPlayers(ctx context.Context, afterPlayerID uint64, limit int) ([]uint64, error)
	LoadLegacyBagStock(ctx context.Context, playerID uint64) ([]*bagv1.BagItem, error)
}

// BagSeeder 是 bag 库迁移落位抽象(MySQLBagRepo 实现)。
type BagSeeder interface {
	SeedLegacyWarehouse(ctx context.Context, playerID uint64, items []*bagv1.BagItem, maxStack data.BagMaxStack) (bool, error)
	VerifyLegacyWarehouse(ctx context.Context, playerID uint64, legacy []*bagv1.BagItem) error
}

// BagMigrationSummary 一轮迁移的结果统计。
type BagMigrationSummary struct {
	Scanned  int // 枚举到的玩家数
	Migrated int // 本轮真实完成迁移的玩家数
	Skipped  int // 幂等闸已存在(此前已迁)的玩家数
	Failed   int // 迁移或对账失败的玩家数(已告警,重跑重试)
}

// BagMigrationUsecase 存量迁移用例。
type BagMigrationUsecase struct {
	legacy LegacyBagSource
	bag    BagSeeder
	cfg    conf.BagConf
	log    *log.Helper
}

// NewBagMigrationUsecase 构造。
func NewBagMigrationUsecase(legacy LegacyBagSource, bag BagSeeder, cfg conf.BagConf, logger log.Logger) *BagMigrationUsecase {
	return &BagMigrationUsecase{legacy: legacy, bag: bag, cfg: cfg, log: log.NewHelper(logger)}
}

// RunOnce 全量跑一轮迁移(游标直至枚举耗尽或 ctx 取消)。幂等:已迁玩家 no-op 计 Skipped。
func (u *BagMigrationUsecase) RunOnce(ctx context.Context) (BagMigrationSummary, error) {
	var (
		sum    BagMigrationSummary
		cursor uint64
	)
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		players, err := u.legacy.ListLegacyBagPlayers(ctx, cursor, u.cfg.MigrationBatch)
		if err != nil {
			return sum, err
		}
		if len(players) == 0 {
			return sum, nil
		}
		for _, playerID := range players {
			cursor = playerID
			sum.Scanned++
			if err := u.migrateOne(ctx, playerID, &sum); err != nil {
				// 单玩家失败不阻断整轮(bound 实例等预期拦截逐个排障;重跑重试)。
				sum.Failed++
				u.log.Errorw("msg", "bag_legacy_migration_player_failed", "player_id", playerID,
					"code", errcode.As(err), "err", err)
			}
		}
	}
}

// migrateOne 迁移单玩家:快照 → 落位 → (真实迁移时)对账。
func (u *BagMigrationUsecase) migrateOne(ctx context.Context, playerID uint64, sum *BagMigrationSummary) error {
	legacy, err := u.legacy.LoadLegacyBagStock(ctx, playerID)
	if err != nil {
		return err
	}
	migrated, err := u.bag.SeedLegacyWarehouse(ctx, playerID, legacy, u.cfg.ItemMaxStackOf)
	if err != nil {
		return err
	}
	if !migrated {
		sum.Skipped++
		return nil
	}
	// 对账用同一份快照(冻结窗口内 legacy 静止;对账失败按玩家计失败,资产已落位不回滚,
	// 漂移属冻结纪律被违反,需人工排障——绝不静默)。
	if verr := u.bag.VerifyLegacyWarehouse(ctx, playerID, legacy); verr != nil {
		return verr
	}
	sum.Migrated++
	return nil
}
