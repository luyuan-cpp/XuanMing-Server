package configtable

import (
	"context"
	"strings"

	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	configv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// AdminService 是配置表热更的通用内部 gRPC 实现。
// 服务必须仅在内部端口注册；带玩家身份的调用纵深拒绝。
type AdminService struct {
	configv1.UnimplementedConfigTableAdminServiceServer
	store     *Store
	activeDir string
}

// NewAdminService 构造配置表热更入口。store 必须已经完成启动首载。
func NewAdminService(store *Store, activeDir string) *AdminService {
	return &AdminService{store: store, activeDir: activeDir}
}

// ReloadConfigTable 重读 active 目录；Store 保证全批校验成功才原子切换，失败保留旧快照。
func (s *AdminService) ReloadConfigTable(ctx context.Context, req *configv1.ReloadConfigTableRequest) (*configv1.ReloadConfigTableResponse, error) {
	if pmw.PlayerIDFromContext(ctx) != 0 {
		return &configv1.ReloadConfigTableResponse{
			Code:   commonv1.ErrCode_ERR_PERMISSION_DENY,
			Detail: "player-facing calls are not allowed",
		}, nil
	}
	currentVersion := func() uint64 {
		if s != nil && s.store != nil {
			if tables := s.store.Tables(); tables != nil {
				return tables.Version
			}
		}
		return 0
	}
	if s == nil || s.store == nil || s.activeDir == "" {
		return &configv1.ReloadConfigTableResponse{
			Code:          commonv1.ErrCode_ERR_INVALID_STATE,
			ActiveVersion: currentVersion(),
			Detail:        "config table store is not initialized",
		}, nil
	}

	res, err := s.store.Load(s.activeDir, req.GetExpectVersion())
	if err != nil {
		return &configv1.ReloadConfigTableResponse{
			Code:          commonv1.ErrCode_ERR_INVALID_STATE,
			ActiveVersion: currentVersion(),
			Detail:        err.Error(),
		}, nil
	}
	detail := ""
	if !res.Reloaded {
		detail = "version unchanged, no-op"
	} else if len(res.Warnings) > 0 {
		detail = strings.Join(res.Warnings, "; ")
	}
	return &configv1.ReloadConfigTableResponse{
		Code:          commonv1.ErrCode_OK,
		ActiveVersion: res.Version,
		Reloaded:      res.Reloaded,
		Detail:        detail,
	}, nil
}
