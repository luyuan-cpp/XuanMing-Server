// player 服务层鉴权辅助单测(2026-07-08 IDOR 修复配套)。
//
// 只测纯鉴权分流(selfPlayerID / resolvePlayerID / systemOnly),不碰 repo/usecase:
//   - 客户端身份经 pmw.PlayerIDFromContext 读 ctx(这里用 plog.WithPlayerID 注入模拟 Envoy 注入)。
//   - callerID==0 = 内网直连(无网关注入),callerID>0 = 经 Envoy 的客户端。
package service

import (
	"context"
	"testing"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
)

// withCaller 构造带鉴权身份的 ctx(callerID>0 模拟客户端;0 = 内网直连不注入)。
func withCaller(callerID uint64) context.Context {
	ctx := context.Background()
	if callerID > 0 {
		ctx = plog.WithPlayerID(ctx, callerID)
	}
	return ctx
}

func TestSelfPlayerID(t *testing.T) {
	tests := []struct {
		name     string
		caller   uint64
		reqID    uint64
		wantID   uint64
		wantCode commonv1.ErrCode
	}{
		{"未鉴权直连拒", 0, 1001, 0, commonv1.ErrCode_ERR_UNAUTHORIZED},
		{"客户端自己(body一致)", 1001, 1001, 1001, commonv1.ErrCode_OK},
		{"客户端 body=0 回落自身", 1001, 0, 1001, commonv1.ErrCode_OK},
		{"客户端伪造他人 player_id 拒", 1001, 2002, 0, commonv1.ErrCode_ERR_PERMISSION_DENY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotCode := selfPlayerID(withCaller(tt.caller), tt.reqID)
			if gotID != tt.wantID || gotCode != tt.wantCode {
				t.Fatalf("selfPlayerID(caller=%d, req=%d) = (%d,%v), want (%d,%v)",
					tt.caller, tt.reqID, gotID, gotCode, tt.wantID, tt.wantCode)
			}
		})
	}
}

func TestResolvePlayerID(t *testing.T) {
	tests := []struct {
		name     string
		caller   uint64
		reqID    uint64
		wantID   uint64
		wantCode commonv1.ErrCode
	}{
		{"内网直连信任 body", 0, 1001, 1001, commonv1.ErrCode_OK},
		{"内网直连 body=0 非法", 0, 0, 0, commonv1.ErrCode_ERR_INVALID_ARG},
		{"客户端读自己", 1001, 1001, 1001, commonv1.ErrCode_OK},
		{"客户端 body=0 回落自身", 1001, 0, 1001, commonv1.ErrCode_OK},
		{"客户端读他人拒", 1001, 2002, 0, commonv1.ErrCode_ERR_PERMISSION_DENY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotCode := resolvePlayerID(withCaller(tt.caller), tt.reqID)
			if gotID != tt.wantID || gotCode != tt.wantCode {
				t.Fatalf("resolvePlayerID(caller=%d, req=%d) = (%d,%v), want (%d,%v)",
					tt.caller, tt.reqID, gotID, gotCode, tt.wantID, tt.wantCode)
			}
		})
	}
}

func TestSystemOnly(t *testing.T) {
	if code := systemOnly(withCaller(0)); code != commonv1.ErrCode_OK {
		t.Fatalf("systemOnly(内网直连) = %v, want OK", code)
	}
	if code := systemOnly(withCaller(1001)); code != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("systemOnly(客户端) = %v, want ERR_PERMISSION_DENY", code)
	}
}
