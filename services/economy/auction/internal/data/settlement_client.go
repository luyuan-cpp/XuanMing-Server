// settlement_client.go 实现 biz.SettlementLedger:把一笔撮合成交经 gRPC 交给 inventory
// 服务做「卖↔买双方资产原子对转 + match_id 幂等」(不变量 §9.2 / §9.7)。
//
// 接线(对齐 chat/team_reader、friend/locator_client 直连模式):
//   - main.go 用 pkg/grpcclient.MustDialInsecure 拨号(内网 insecure;无 JWT → inventory 侧 callerID==0,
//     SettleAuctionMatch 是系统接口只认内网直连);inventory_addr 未配且 allow_noop_settlement=true 时 main 才退回 NoopSettlementLedger,否则 fail-fast。
//   - 成交价(被动挂单价)= MatchRecord.Price 作为单价传给 inventory,总价由 inventory 端溢出安全乘。
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	inventoryv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/inventory/v1"
)

// GrpcInventoryLedger 用 inventory 服务 gRPC client 实现 biz.SettlementLedger。
type GrpcInventoryLedger struct {
	conn *grpc.ClientConn
	cli  inventoryv1.InventoryServiceClient
}

// NewGrpcInventoryLedger 直连 inventory 服务 endpoint(host:port,内网 insecure)。
func NewGrpcInventoryLedger(inventoryAddr string) *GrpcInventoryLedger {
	conn := grpcclient.MustDialInsecure(inventoryAddr)
	return &GrpcInventoryLedger{conn: conn, cli: inventoryv1.NewInventoryServiceClient(conn)}
}

// Close 关闭底层连接。
func (g *GrpcInventoryLedger) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// Settle 调 inventory.SettleAuctionMatch 完成本笔成交的资产对转(幂等键 = match_id)。
//
//   - inventory 返回 OK              → nil(结算成功 / 幂等回放)
//   - 返回 ERR_INVENTORY_INSUFFICIENT → ErrAuctionInsufficient(买家金币 / 卖家道具不足)
//   - 其它非 OK code                 → 原样透传该错误码(便于上游定位)
//   - gRPC 传输错误                  → 原样返回(撮合中止,剩余不挂簿)
func (g *GrpcInventoryLedger) Settle(ctx context.Context, m *MatchRecord) error {
	resp, err := g.cli.SettleAuctionMatch(ctx, &inventoryv1.SettleAuctionMatchRequest{
		MatchId:      m.MatchID,
		SellerId:     m.SellerID,
		BuyerId:      m.BuyerID,
		ItemConfigId: m.ItemConfigID,
		Quantity:     m.Quantity,
		UnitPrice:    m.Price,
	})
	if err != nil {
		return err
	}
	switch resp.GetCode() {
	case commonv1.ErrCode_OK:
		return nil
	case commonv1.ErrCode_ERR_INVENTORY_INSUFFICIENT:
		return errcode.New(errcode.ErrAuctionInsufficient,
			"auction settle insufficient match=%d seller=%d buyer=%d", m.MatchID, m.SellerID, m.BuyerID)
	default:
		return errcode.New(errcode.Code(resp.GetCode()),
			"auction settle failed match=%d code=%d", m.MatchID, int32(resp.GetCode()))
	}
}
