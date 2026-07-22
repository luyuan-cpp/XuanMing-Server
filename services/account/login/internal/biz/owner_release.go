// owner_release.go — owner 迁移登出释放接口(owner-authority.md migrate ⑤,2026-07-22)。
package biz

import (
	"context"

	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

// OwnerReleaser 登出时释放 owner 记录(弱依赖:失败仅告警,不影响登出)。
// 由 data.GrpcOwnerReleaser 实现;可为 nil(未配 owner_addr → 不接 owner,行为不变)。
type OwnerReleaser interface {
	QueryOwner(ctx context.Context, playerID uint64) (data.OwnerReleaseView, error)
	ReleaseOwner(ctx context.Context, playerID, ownerEpoch uint64, operationID string) error
}

// SetOwnerReleaser 注入 owner 释放器(nil-safe)。
func (u *LoginUsecase) SetOwnerReleaser(r OwnerReleaser) {
	u.ownerReleaser = r
}
