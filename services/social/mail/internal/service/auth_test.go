// mail 服务层运营接口鉴权单测(2026-07-08 Send* 暴露修复配套)。
//
// 背景:Envoy 把 /pandora.mail.v1.MailService/ 整前缀路由给客户端 JWT 流量,而三个运营
// 发信 RPC(SendSystemMail/SendGuildMail/SendPersonalMail)原先无 system-only 兜底 →
// 任意登录玩家可自助群发 / 发带附件邮件。修复 = Envoy 精确 path 403 + 服务层 systemOnly
// 双保险(对齐 player 服务模式),本文件测服务层兜底:
//   - callerID==0 = 内网直连(运营工具 / battle_result 背包满转邮件,无 JWT 注入)→ 放行
//   - callerID>0  = 经 Envoy 的客户端(JWT sub 注入 x-pandora-player-id)→ 一律 PERMISSION_DENY
package service

import (
	"context"
	"testing"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"
)

// withCaller 构造带鉴权身份的 ctx(callerID>0 模拟客户端;0 = 内网直连不注入)。
func withCaller(callerID uint64) context.Context {
	ctx := context.Background()
	if callerID > 0 {
		ctx = plog.WithPlayerID(ctx, callerID)
	}
	return ctx
}

func TestSystemOnly(t *testing.T) {
	if code := systemOnly(withCaller(0)); code != commonv1.ErrCode_OK {
		t.Fatalf("systemOnly(内网直连) = %v, want OK", code)
	}
	if code := systemOnly(withCaller(1001)); code != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("systemOnly(客户端) = %v, want ERR_PERMISSION_DENY", code)
	}
}

// TestSendRPCs_ClientDenied 验证三个 Send* RPC 对客户端身份先鉴权后业务:
// uc/sf 全 nil,guard 拒绝路径不触碰业务层(不 panic 即证明鉴权在最前)。
func TestSendRPCs_ClientDenied(t *testing.T) {
	s := &MailService{}
	ctx := withCaller(1001)

	sysResp, err := s.SendSystemMail(ctx, &mailv1.SendSystemMailRequest{Title: "t", Body: "b"})
	if err != nil || sysResp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("SendSystemMail(客户端) = (%v, %v), want ERR_PERMISSION_DENY", sysResp.GetCode(), err)
	}
	guildResp, err := s.SendGuildMail(ctx, &mailv1.SendGuildMailRequest{GuildId: 1, Title: "t"})
	if err != nil || guildResp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("SendGuildMail(客户端) = (%v, %v), want ERR_PERMISSION_DENY", guildResp.GetCode(), err)
	}
	persResp, err := s.SendPersonalMail(ctx, &mailv1.SendPersonalMailRequest{ToPlayerId: 2002, Title: "t"})
	if err != nil || persResp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("SendPersonalMail(客户端) = (%v, %v), want ERR_PERMISSION_DENY", persResp.GetCode(), err)
	}
}
