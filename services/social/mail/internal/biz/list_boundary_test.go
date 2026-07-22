package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/services/social/mail/internal/data"
)

type listBoundaryMailRepo struct {
	data.MailRepo
	personal      []data.MailRow
	system        []data.MailRow
	personalCalls int
	systemCalls   int
	lastCursor    uint64
	lastLimit     int
	advanceCalls  int
	advancedSys   uint64
	advancedGuild uint64
}

func (r *listBoundaryMailRepo) GetCursor(context.Context, uint64) (uint64, uint64, error) {
	return 10, 20, nil
}

func (r *listBoundaryMailRepo) ListPersonal(_ context.Context, _ uint64, _ int64, cursor uint64, limit int) ([]data.MailRow, error) {
	r.personalCalls++
	r.lastCursor = cursor
	r.lastLimit = limit
	if len(r.personal) > limit {
		return r.personal[:limit], nil
	}
	return r.personal, nil
}

func (r *listBoundaryMailRepo) ListSysSince(context.Context, uint64, int64) ([]data.MailRow, error) {
	r.systemCalls++
	return r.system, nil
}

func (r *listBoundaryMailRepo) GetPlayerGuild(context.Context, uint64) (uint64, bool, error) {
	return 0, false, nil
}

func (r *listBoundaryMailRepo) HasClaimed(context.Context, uint64, uint64) (bool, error) {
	return false, nil
}

func (r *listBoundaryMailRepo) AdvanceCursor(_ context.Context, _ uint64, sysMax, guildMax uint64) error {
	r.advanceCalls++
	r.advancedSys = sysMax
	r.advancedGuild = guildMax
	return nil
}

// TestListMailPaginationBoundaries 覆盖首页 channel 合并、个人邮件游标、默认/最大
// limit 与 next_cursor 边界，防翻页时重复拼系统邮件或返回无界列表。
func TestListMailPaginationBoundaries(t *testing.T) {
	t.Run("首页恰好一页并推进系统游标", func(t *testing.T) {
		repo := &listBoundaryMailRepo{
			personal: []data.MailRow{{MailID: 30}, {MailID: 20}},
			system:   []data.MailRow{{MailID: 40}},
		}
		uc := NewMailUsecase(repo, testCfg(), nil)

		mails, next, err := uc.ListMail(context.Background(), 7, 1000, 0, 2)
		if err != nil {
			t.Fatalf("ListMail first page: %v", err)
		}
		if len(mails) != 3 || next != 20 {
			t.Fatalf("首页合并/游标不符: mails=%d next=%d", len(mails), next)
		}
		if repo.systemCalls != 1 || repo.advanceCalls != 1 || repo.advancedSys != 40 || repo.advancedGuild != 20 {
			t.Fatalf("channel 水位推进不符: sys_calls=%d advance=%d sys=%d guild=%d",
				repo.systemCalls, repo.advanceCalls, repo.advancedSys, repo.advancedGuild)
		}
	})

	t.Run("翻页只查个人邮件且零limit归默认", func(t *testing.T) {
		repo := &listBoundaryMailRepo{personal: []data.MailRow{{MailID: 10}}}
		uc := NewMailUsecase(repo, testCfg(), nil)

		mails, next, err := uc.ListMail(context.Background(), 7, 1000, 20, 0)
		if err != nil {
			t.Fatalf("ListMail next page: %v", err)
		}
		if len(mails) != 1 || next != 0 || repo.lastCursor != 20 || repo.lastLimit != defaultPageLimit {
			t.Fatalf("翻页参数/结果不符: mails=%d next=%d cursor=%d limit=%d",
				len(mails), next, repo.lastCursor, repo.lastLimit)
		}
		if repo.systemCalls != 0 || repo.advanceCalls != 0 {
			t.Fatalf("个人邮件翻页不得重复拉 channel: sys=%d advance=%d", repo.systemCalls, repo.advanceCalls)
		}
	})

	t.Run("超大limit收敛", func(t *testing.T) {
		repo := &listBoundaryMailRepo{}
		uc := NewMailUsecase(repo, testCfg(), nil)

		if _, _, err := uc.ListMail(context.Background(), 7, 1000, 1, 10_000); err != nil {
			t.Fatalf("ListMail clamped page: %v", err)
		}
		if repo.lastLimit != maxPageLimit {
			t.Fatalf("limit must clamp to %d, got %d", maxPageLimit, repo.lastLimit)
		}
	})
}
