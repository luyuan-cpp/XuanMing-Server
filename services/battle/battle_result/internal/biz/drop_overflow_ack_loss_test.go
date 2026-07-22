package biz

import (
	"context"
	"testing"
)

// overflowAckLossMail 模拟邮件服务已接受写入、但第一次 RPC 响应丢失。调用方只能
// 保留 outbox 后重试，因此两次调用必须复用同一个幂等键。
type overflowAckLossMail struct {
	calls []grantCall
}

func (m *overflowAckLossMail) SendOverflowMail(_ context.Context, playerID uint64, itemConfigIDs []uint32, key string) error {
	m.calls = append(m.calls, grantCall{
		playerID: playerID,
		items:    append([]uint32(nil), itemConfigIDs...),
		key:      key,
	})
	if len(m.calls) == 1 {
		return simpleErr("mail response lost after accept")
	}
	return nil
}

// TestDropOverflowMailResponseLossRetriesWithStableIdempotencyKey 验证背包满转邮件时，
// 首次响应未知不丢 outbox，重试仍以同一业务键收敛，明确成功后才删除。
func TestDropOverflowMailResponseLossRetriesWithStableIdempotencyKey(t *testing.T) {
	repo := newFakeRepo()
	granter := &fakeGranter{capacityFull: true}
	mail := &overflowAckLossMail{}
	uc := newDropUsecaseWithMail(repo, granter, mail, []uint32{5001})
	ctx := context.Background()

	if _, err := uc.ReportResult(ctx, dropResult(704, []uint32{5001}, nil), 0); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	if n, err := uc.publishDropBatch(ctx); err != nil || n != 0 {
		t.Fatalf("unknown mail response should retain row: n=%d err=%v", n, err)
	}
	if len(repo.dropOutbox) != 1 || len(mail.calls) != 1 {
		t.Fatalf("first attempt state: rows=%d mail_calls=%d", len(repo.dropOutbox), len(mail.calls))
	}

	if n, err := uc.publishDropBatch(ctx); err != nil || n != 1 {
		t.Fatalf("retry should drain after explicit success: n=%d err=%v", n, err)
	}
	if len(repo.dropOutbox) != 0 || len(mail.calls) != 2 {
		t.Fatalf("retry state: rows=%d mail_calls=%d", len(repo.dropOutbox), len(mail.calls))
	}
	first, second := mail.calls[0], mail.calls[1]
	if first.key != "battle_drop:704:1" || second.key != first.key ||
		first.playerID != second.playerID || len(first.items) != 1 || len(second.items) != 1 ||
		first.items[0] != 5001 || second.items[0] != first.items[0] {
		t.Fatalf("retry did not preserve immutable mail identity: first=%+v second=%+v", first, second)
	}
}
