package service

import (
	"context"
	"testing"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"
)

// TestMailPlayerRPCsRequireJWT 确保所有玩家邮件入口都在触达 repo/inventory 前
// 拒绝无 JWT 调用；使用 nil usecase 可机械证明鉴权顺序。
func TestMailPlayerRPCsRequireJWT(t *testing.T) {
	svc := NewMailService(nil, nil)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() (commonv1.ErrCode, error)
	}{
		{
			name: "列表",
			call: func() (commonv1.ErrCode, error) {
				resp, err := svc.ListMail(ctx, &mailv1.ListMailRequest{})
				return resp.GetCode(), err
			},
		},
		{
			name: "已读",
			call: func() (commonv1.ErrCode, error) {
				resp, err := svc.ReadMail(ctx, &mailv1.ReadMailRequest{MailId: 1})
				return resp.GetCode(), err
			},
		},
		{
			name: "领取",
			call: func() (commonv1.ErrCode, error) {
				resp, err := svc.ClaimMail(ctx, &mailv1.ClaimMailRequest{MailId: 1})
				return resp.GetCode(), err
			},
		},
		{
			name: "删除",
			call: func() (commonv1.ErrCode, error) {
				resp, err := svc.DeleteMail(ctx, &mailv1.DeleteMailRequest{MailId: 1})
				return resp.GetCode(), err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := tt.call()
			if err != nil {
				t.Fatalf("unexpected transport error: %v", err)
			}
			if code != commonv1.ErrCode_ERR_UNAUTHORIZED {
				t.Fatalf("无 JWT 必须拒绝: got=%s", code)
			}
		})
	}
}
