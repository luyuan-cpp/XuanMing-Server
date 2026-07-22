package biz

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"
	"github.com/luyuancpp/pandora/services/social/mail/internal/data"
)

type claimBoundaryRepo struct {
	data.MailRepo
	playerID    uint64
	mailID      uint64
	payload     []byte
	claimed     bool
	recordCalls int
	statusCalls int
}

func (r *claimBoundaryRepo) GetClaimablePayload(_ context.Context, playerID, mailID uint64, _ int64) ([]byte, bool, error) {
	if playerID != r.playerID || mailID != r.mailID {
		return nil, false, nil
	}
	return r.payload, true, nil
}

func (r *claimBoundaryRepo) HasClaimed(_ context.Context, playerID, mailID uint64) (bool, error) {
	return playerID == r.playerID && mailID == r.mailID && r.claimed, nil
}

func (r *claimBoundaryRepo) GetClaimState(_ context.Context, playerID, mailID uint64) (bool, bool, error) {
	return playerID == r.playerID && mailID == r.mailID && r.claimed, false, nil
}

func (r *claimBoundaryRepo) RecordClaim(_ context.Context, playerID, mailID uint64) (bool, error) {
	r.recordCalls++
	if playerID != r.playerID || mailID != r.mailID {
		return false, errors.New("unexpected claim identity")
	}
	if r.claimed {
		return false, nil
	}
	r.claimed = true
	return true, nil
}

func (r *claimBoundaryRepo) SetPersonalStatus(context.Context, uint64, uint64, int32) error {
	r.statusCalls++
	return nil
}

type claimBoundaryItemGranter struct {
	calls int
	keys  []string
	err   error
}

func (g *claimBoundaryItemGranter) Grant(_ context.Context, _ uint64, _ []*mailv1.MailAttachment, key string) error {
	g.calls++
	g.keys = append(g.keys, key)
	return g.err
}

type claimBoundaryInstanceGranter struct {
	calls     int
	keys      []string
	failCount int
	err       error
}

func (g *claimBoundaryInstanceGranter) GrantInstances(_ context.Context, _ uint64, _ []uint32, key string) error {
	g.calls++
	g.keys = append(g.keys, key)
	if g.failCount > 0 {
		g.failCount--
		return g.err
	}
	return nil
}

func claimBoundaryPayload(t *testing.T, attachments ...*mailv1.MailAttachment) []byte {
	t.Helper()
	payload, err := proto.Marshal(&mailv1.MailContentStorageRecord{
		Title: "边界邮件", Attachments: attachments, InstanceGrantKey: "battle_drop:77:9",
	})
	if err != nil {
		t.Fatalf("marshal claim payload: %v", err)
	}
	return payload
}

// TestClaimMailCapacityFailureKeepsClaimRetryable 覆盖混合附件的关键补偿边界：
// 可堆叠附件已调用成功后，实例背包满必须保持整封未领取；重试时两条资产腿复用
// 原幂等键，实例容量恢复后才能记录 claim。
func TestClaimMailCapacityFailureKeepsClaimRetryable(t *testing.T) {
	const playerID, mailID = uint64(9), uint64(77)
	repo := &claimBoundaryRepo{
		playerID: playerID,
		mailID:   mailID,
		payload: claimBoundaryPayload(t,
			stackAtt(3001, 5),
			instAtt(5001, 1),
		),
	}
	stackGranter := &claimBoundaryItemGranter{}
	instanceGranter := &claimBoundaryInstanceGranter{
		failCount: 1,
		err:       errcode.New(errcode.ErrInventoryCapacityFull, "instance bag full"),
	}
	uc := NewMailUsecase(repo, testCfg(), stackGranter)
	uc.SetInstanceGranter(instanceGranter)

	attachments, err := uc.ClaimMail(context.Background(), playerID, mailID, 1000)
	if errcode.As(err) != errcode.ErrInventoryCapacityFull {
		t.Fatalf("实例背包满错误必须原样传播: got=%v", err)
	}
	if attachments != nil {
		t.Fatalf("未完整发放时不得返回实发清单: %+v", attachments)
	}
	if repo.claimed || repo.recordCalls != 0 || repo.statusCalls != 0 {
		t.Fatalf("容量不足时必须保持未领取: claimed=%v record=%d status=%d",
			repo.claimed, repo.recordCalls, repo.statusCalls)
	}

	attachments, err = uc.ClaimMail(context.Background(), playerID, mailID, 1001)
	if err != nil {
		t.Fatalf("容量恢复后重试应成功: %v", err)
	}
	if len(attachments) != 2 || !repo.claimed || repo.recordCalls != 1 || repo.statusCalls != 1 {
		t.Fatalf("成功重试后领取状态不符: attachments=%d claimed=%v record=%d status=%d",
			len(attachments), repo.claimed, repo.recordCalls, repo.statusCalls)
	}
	if stackGranter.calls != 2 || instanceGranter.calls != 2 {
		t.Fatalf("重试必须重新驱动两条幂等资产腿: stack=%d instance=%d",
			stackGranter.calls, instanceGranter.calls)
	}
	if stackGranter.keys[0] != stackGranter.keys[1] || stackGranter.keys[0] != "mail:77:9" {
		t.Fatalf("可堆叠资产腿重试键漂移: %v", stackGranter.keys)
	}
	if instanceGranter.keys[0] != instanceGranter.keys[1] || instanceGranter.keys[0] != "battle_drop:77:9" {
		t.Fatalf("实例资产腿重试键漂移: %v", instanceGranter.keys)
	}
}

// TestClaimMailStackGrantErrorPropagatesWithoutClaim 依赖超时等非业务错误同样不能
// 被吞掉或提前标记已领，否则附件会永久丢失。
func TestClaimMailStackGrantErrorPropagatesWithoutClaim(t *testing.T) {
	const playerID, mailID = uint64(3), uint64(88)
	repo := &claimBoundaryRepo{
		playerID: playerID,
		mailID:   mailID,
		payload:  claimBoundaryPayload(t, stackAtt(3001, 1)),
	}
	granter := &claimBoundaryItemGranter{err: context.DeadlineExceeded}
	uc := NewMailUsecase(repo, testCfg(), granter)

	_, err := uc.ClaimMail(context.Background(), playerID, mailID, 1000)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inventory transport error must propagate: %v", err)
	}
	if repo.claimed || repo.recordCalls != 0 || repo.statusCalls != 0 {
		t.Fatalf("inventory 失败后不得记录领取: claimed=%v record=%d status=%d",
			repo.claimed, repo.recordCalls, repo.statusCalls)
	}
}
