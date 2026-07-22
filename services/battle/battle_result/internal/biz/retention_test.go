// retention_test.go — 保留期清理 biz 层单测(§9.24;SQL 行为由 data 层真 MySQL 集成测试覆盖)。
//
// 覆盖:每轮小批量循环删到短批(积压追平不靠单批,审计 P1 吞吐)、
// 陈年未结算水位行每轮告警探测(永不自动清理但不能静默)。
package biz

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/conf"
)

func retentionUsecase(repo *fakeRepo, batch int) *BattleResultUsecase {
	cfg := conf.BattleConf{
		EloKFactor: 32, BaseMMR: 1500,
		HistoryRetentionDays: 90,
		RetentionSweepBatch:  batch,
	}
	return NewBattleResultUsecase(repo, NewStaticMMRReader(cfg.BaseMMR), &fakePusher{}, nil, cfg)
}

// 每轮必须小批量循环删到短批为止:只删单批的话默认 200 场/小时追不平生产流入,
// 积压只增不减(审计 P1)。满批(=batch)→ 继续;短批(<batch)→ 本轮追平收手。
func TestRetentionSweep_DrainsBacklogUntilShortBatch(t *testing.T) {
	repo := newFakeRepo()
	repo.purgeBattlesResults = []int64{2, 2, 1} // 两个满批 + 一个短批 → 恰好 3 次调用
	repo.purgeProgressResults = []int64{2, 0}   // 一个满批 + 一个空批 → 恰好 2 次调用
	uc := retentionUsecase(repo, 2)

	uc.sweepRetentionOnce(context.Background())

	if repo.purgeBattlesCalls != 3 {
		t.Fatalf("battles purge calls=%d want=3 (drain until short batch)", repo.purgeBattlesCalls)
	}
	if repo.purgeProgressCalls != 2 {
		t.Fatalf("progress purge calls=%d want=2 (drain until short batch)", repo.purgeProgressCalls)
	}
	if repo.staleUnsettledCalls != 1 {
		t.Fatalf("stale unsettled probe calls=%d want=1 (must run every sweep)", repo.staleUnsettledCalls)
	}
}

// 零值配置防御:未过 Defaults 的 batch=0 不得死循环(n<batch 永假)。
func TestRetentionSweep_ZeroBatchDoesNotSpin(t *testing.T) {
	repo := newFakeRepo()
	uc := retentionUsecase(repo, 0)

	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		uc.sweepRetentionOnce(workerCtx)
		close(done)
	}()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		// 先取消 worker，避免失败分支遗留自旋 goroutine；随后以 Fatal 明确结束用例。
		cancel()
		t.Fatal("zero-batch sweep did not finish before the 1s deadline")
	}
	if repo.purgeBattlesCalls != 1 || repo.purgeProgressCalls != 1 {
		t.Fatalf("zero-batch sweep calls battles=%d progress=%d want 1/1",
			repo.purgeBattlesCalls, repo.purgeProgressCalls)
	}
}

// ctx 取消中断排空循环(优雅停机不拖批)。
func TestRetentionSweep_CanceledContextStopsDrain(t *testing.T) {
	repo := newFakeRepo()
	repo.purgeBattlesResults = []int64{2, 2, 2, 2}
	uc := retentionUsecase(repo, 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	uc.sweepRetentionOnce(ctx)

	if repo.purgeBattlesCalls != 0 {
		t.Fatalf("canceled ctx must stop drain before first batch, calls=%d", repo.purgeBattlesCalls)
	}
}
