// owner_client.go — login 调 owner 权威(owner-authority.md migrate ⑤,2026-07-22)。
//
// 接线对齐既有内网 gRPC 客户端:insecure 直连,无 JWT(系统接口)。
// login 只需要 Query + Release(登出释放,compare-delete 自己);Begin/Admit 归
// hub_allocator(签票统一出口)与 ds_allocator(READY 交付/census)。
package data

import (
	"context"
	"time"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	ownerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/owner/v1"
)

// ownerRPCTimeout 单次 owner 调用上限(登出链路弱依赖,快速失败放行)。
const ownerRPCTimeout = 2 * time.Second

// OwnerReleaseView 登出释放所需的最小记录视图。
type OwnerReleaseView struct {
	OwnerEpoch  uint64
	OwnerType   int8
	OperationID string
}

// GrpcOwnerReleaser 用 owner 服务 gRPC client 实现 biz.OwnerReleaser。
type GrpcOwnerReleaser struct {
	conn *grpc.ClientConn
	cli  ownerv1.OwnerServiceClient
}

// NewGrpcOwnerReleaser 直连 owner 服务 endpoint(host:port,内网 insecure)。
func NewGrpcOwnerReleaser(ownerAddr string) *GrpcOwnerReleaser {
	conn := grpcclient.MustDialInsecure(ownerAddr)
	return &GrpcOwnerReleaser{conn: conn, cli: ownerv1.NewOwnerServiceClient(conn)}
}

// Close 关闭底层连接。
func (g *GrpcOwnerReleaser) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// QueryOwner 读当前记录(登出释放前的 compare 依据)。
func (g *GrpcOwnerReleaser) QueryOwner(ctx context.Context, playerID uint64) (OwnerReleaseView, error) {
	callCtx, cancel := context.WithTimeout(ctx, ownerRPCTimeout)
	defer cancel()
	resp, err := g.cli.QueryOwner(callCtx, &ownerv1.QueryOwnerRequest{PlayerId: playerID})
	if err != nil {
		return OwnerReleaseView{}, err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return OwnerReleaseView{}, errcode.New(errcode.Code(resp.GetCode()), "owner query rejected player=%d", playerID)
	}
	rec := resp.GetRecord()
	return OwnerReleaseView{
		OwnerEpoch:  rec.GetOwnerEpoch(),
		OwnerType:   int8(rec.GetOwnerType()),
		OperationID: rec.GetOperationId(),
	}, nil
}

// ReleaseOwner 释放(epoch+operation 匹配才生效;迟到调用 owner 侧幂等 no-op)。
func (g *GrpcOwnerReleaser) ReleaseOwner(ctx context.Context, playerID, ownerEpoch uint64, operationID string) error {
	callCtx, cancel := context.WithTimeout(ctx, ownerRPCTimeout)
	defer cancel()
	resp, err := g.cli.ReleaseOwner(callCtx, &ownerv1.ReleaseOwnerRequest{
		PlayerId:    playerID,
		OwnerEpoch:  ownerEpoch,
		OperationId: operationID,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "owner release rejected player=%d", playerID)
	}
	return nil
}
