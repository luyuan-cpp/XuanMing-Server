// sweep_test.go — 保留期清理单测:入参透传(保留天数/批量)+ 单表失败不阻断其余表(2026-07-21)。
package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
)

// sweepRecordingRepo 记录两个清理方法的入参,可注入错误(嵌入 fakeRepo 补齐其余接口)。
type sweepRecordingRepo struct {
	*fakeRepo
	ledgerDays, ledgerLimit int
	escrowDays, escrowLimit int
	ledgerErr               error
	ledgerCalls, escrowCalls int
}

func (r *sweepRecordingRepo) DeleteLedgerBefore(_ context.Context, retentionDays, limit int) (int64, error) {
	r.ledgerCalls++
	r.ledgerDays, r.ledgerLimit = retentionDays, limit
	if r.ledgerErr != nil {
		return 0, r.ledgerErr
	}
	return 3, nil
}

func (r *sweepRecordingRepo) DeleteClosedEscrowBefore(_ context.Context, retentionDays, limit int) (int64, error) {
	r.escrowCalls++
	r.escrowDays, r.escrowLimit = retentionDays, limit
	return 2, nil
}

// TestSweepRetentionPassesConfig 保留天数与批量按配置透传到 data 层。
func TestSweepRetentionPassesConfig(t *testing.T) {
	repo := &sweepRecordingRepo{fakeRepo: newFakeRepo()}
	uc := NewInventoryUsecase(repo, conf.InventoryConf{
		LedgerRetentionDays: 90, EscrowRetentionDays: 90, SweepBatch: 500,
	})

	uc.SweepRetention(context.Background())

	if repo.ledgerCalls != 1 || repo.ledgerDays != 90 || repo.ledgerLimit != 500 {
		t.Fatalf("ledger sweep 入参错: calls=%d days=%d limit=%d", repo.ledgerCalls, repo.ledgerDays, repo.ledgerLimit)
	}
	if repo.escrowCalls != 1 || repo.escrowDays != 90 || repo.escrowLimit != 500 {
		t.Fatalf("escrow sweep 入参错: calls=%d days=%d limit=%d", repo.escrowCalls, repo.escrowDays, repo.escrowLimit)
	}
}

// TestSweepRetentionContinuesOnError ledger 清理失败不阻断 escrow 清理(彼此独立,下一轮重试)。
func TestSweepRetentionContinuesOnError(t *testing.T) {
	repo := &sweepRecordingRepo{
		fakeRepo:  newFakeRepo(),
		ledgerErr: errcode.New(errcode.ErrInternal, "mysql down"),
	}
	uc := NewInventoryUsecase(repo, conf.InventoryConf{
		LedgerRetentionDays: 90, EscrowRetentionDays: 90, SweepBatch: 500,
	})

	uc.SweepRetention(context.Background())

	if repo.ledgerCalls != 1 {
		t.Fatalf("ledger sweep 未调用: calls=%d", repo.ledgerCalls)
	}
	if repo.escrowCalls != 1 {
		t.Fatalf("ledger 失败后 escrow sweep 被阻断: calls=%d", repo.escrowCalls)
	}
}
