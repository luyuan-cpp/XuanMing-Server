// sweep_test.go — 过期清理单测:归档/直删分流、默认 TTL、各表 cutoff 计算(2026-07-21)。
package biz

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"

	"github.com/luyuancpp/pandora/services/social/mail/internal/data"
)

func mustPayload(t *testing.T, atts []*mailv1.MailAttachment) []byte {
	t.Helper()
	b, err := proto.Marshal(&mailv1.MailContentStorageRecord{Title: "t", Body: "b", Attachments: atts})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// TestSendPersonalMailDefaultTTL expire_ms=0 时补默认 TTL;显式值原样保留。
func TestSendPersonalMailDefaultTTL(t *testing.T) {
	repo := newFakeMailRepo()
	uc := NewMailUsecase(repo, testCfg(), &fakeItemGranter{})

	const now = int64(1_000_000)
	id, err := uc.SendPersonalMail(context.Background(), 1, 9, "标题", "正文", nil, 0, now, "")
	if err != nil {
		t.Fatalf("SendPersonalMail err: %v", err)
	}
	want := now + 30*dayMs // testCfg DefaultPersonalTtlDays=30
	if got := repo.personal[id].expireMs; got != want {
		t.Fatalf("default ttl: expire_ms = %d, want %d", got, want)
	}
	if repo.personal[id].maxInbox != 200 {
		t.Fatalf("max inbox not passed through, got %d", repo.personal[id].maxInbox)
	}

	id2, err := uc.SendPersonalMail(context.Background(), 2, 9, "标题", "正文", nil, 777, now, "")
	if err != nil {
		t.Fatalf("SendPersonalMail err: %v", err)
	}
	if got := repo.personal[id2].expireMs; got != 777 {
		t.Fatalf("explicit expire_ms overwritten: got %d, want 777", got)
	}
}

// TestSendSysMailEndClamped 系统邮件 end_ms 钳到「创建时刻 + claim_retention_days」内:
// claim 行按创建时刻 + 180 天清理,可领窗口不得超过它 —— 否则 claim 行先消失,而
// inventory 幂等流水只保留 90 天(§9.24),超长邮件将可重复领奖。
func TestSendSysMailEndClamped(t *testing.T) {
	repo := newFakeMailRepo()
	uc := NewMailUsecase(repo, testCfg(), &fakeItemGranter{})

	const now = int64(1_000_000)

	// 显式 end 超过 180 天 → 钳到 now + 180 天
	if _, err := uc.SendSystemMail(context.Background(), 1, "t", "b", nil, 0, now+400*dayMs, now); err != nil {
		t.Fatalf("SendSystemMail err: %v", err)
	}
	if want := now + 180*dayMs; repo.sysEnd != want {
		t.Fatalf("end not clamped: got %d want %d", repo.sysEnd, want)
	}

	// end=0 默认 TTL(7 天)不受钳制影响
	if _, err := uc.SendSystemMail(context.Background(), 2, "t", "b", nil, 0, 0, now); err != nil {
		t.Fatalf("SendSystemMail err: %v", err)
	}
	if want := now + 7*dayMs; repo.sysEnd != want {
		t.Fatalf("default end wrong: got %d want %d", repo.sysEnd, want)
	}

	// start 定得比「创建 + 180 天」还晚 → 钳后窗口无效(end<=start),fail-fast 拒收
	if _, err := uc.SendSystemMail(context.Background(), 3, "t", "b", nil, now+200*dayMs, 0, now); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("invalid window code=%v err=%v, want ErrInvalidArg", errcode.As(err), err)
	}
}

// TestPartitionExpired 归档/直删分流:已领直删;未领带附件归档;无附件直删;坏 payload 保守归档。
func TestPartitionExpired(t *testing.T) {
	withAtt := []data.ExpiredPersonalRow{
		{MailID: 1, PlayerID: 10, Status: data.StatusClaimed}, // 已领 → 直删
		{MailID: 2, PlayerID: 10, Status: data.StatusUnread},  // 未领带附件 → 归档
		{MailID: 3, PlayerID: 11, Status: data.StatusRead},    // 已读无附件 → 直删
		{MailID: 4, PlayerID: 12, Status: data.StatusUnread},  // 坏 payload → 保守归档
	}
	withAtt[1].Payload = mustPayload(t, []*mailv1.MailAttachment{stackAtt(3001, 1)})
	withAtt[2].Payload = mustPayload(t, nil)
	withAtt[3].Payload = []byte{0xff, 0xfe, 0x01} // 非法 proto

	archive, deleteIDs := partitionExpired(withAtt)

	if len(deleteIDs) != 4 {
		t.Fatalf("all rows must be deleted, got %v", deleteIDs)
	}
	if len(archive) != 2 || archive[0].MailID != 2 || archive[1].MailID != 4 {
		t.Fatalf("archive partition wrong: %+v", archive)
	}
}

// TestSweepExpiredCutoffs 各表 cutoff:过期缓冲 7 天、claim 截断 180 天(按雪花 ID)、归档保留 90 天。
func TestSweepExpiredCutoffs(t *testing.T) {
	repo := newFakeMailRepo()
	uc := NewMailUsecase(repo, testCfg(), &fakeItemGranter{})

	// nowMs 取雪花 Epoch 之后一段,保证 claim cutoff 秒 > Epoch(MinIDAt 非零可断言)
	nowMs := (int64(snowflake.Epoch) + 365*86400) * 1000
	repo.expired = []data.ExpiredPersonalRow{
		{MailID: 5, PlayerID: 1, Status: data.StatusClaimed},
	}
	uc.SweepExpired(context.Background(), nowMs)

	wantBefore := nowMs - 7*dayMs
	if repo.sysEndBefore != wantBefore || repo.guildEndBefore != wantBefore {
		t.Fatalf("channel cutoff wrong: sys=%d guild=%d want %d", repo.sysEndBefore, repo.guildEndBefore, wantBefore)
	}
	if len(repo.deletedIDs) != 1 || repo.deletedIDs[0] != 5 || len(repo.archivedRows) != 0 {
		t.Fatalf("personal sweep wrong: deleted=%v archived=%d", repo.deletedIDs, len(repo.archivedRows))
	}
	wantClaimCutoff := snowflake.MinIDAt((nowMs - 180*dayMs) / 1000)
	if wantClaimCutoff == 0 || repo.claimsBeforeID != wantClaimCutoff {
		t.Fatalf("claim cutoff wrong: got %d want %d", repo.claimsBeforeID, wantClaimCutoff)
	}
	if repo.purgeDays != 90 {
		t.Fatalf("archive retention wrong: got %d want 90", repo.purgeDays)
	}
}
