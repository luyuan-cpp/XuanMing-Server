// mail_client.go — battle_result 调 mail.SendPersonalMail 把背包满溢出的战斗装备掉落转邮件(2026-07-08)。
//
// 接线对齐 inventory_client:内网 insecure 直连,无 JWT(系统接口)。
// 传 instance_grant_key = battle_drop:{match_id}:{player_id}(与直发 GrantInstances 相同源键):
// 邮件领取时走 GrantInstances 用同键去重,使直发链与邮件领取链共享幂等键 → 至多一次(即便偶发重复邮件)。
// 装备附件用 instance 形态(oneof):领取时逐件铸造独立实例(保持装备=唯一实例语义;
// 实例唯一 ID 由 inventory 领取时雪花生成,铸出默认未鉴定)。
package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/grpcclient"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"
)

// 溢出邮件标题/正文(运营可后续改配置;此处内置最小可用文案)。
const (
	overflowMailTitle = "战斗掉落"
	overflowMailBody  = "背包已满,战斗掉落的装备已放入邮件,请清理背包后领取。"
)

// GrpcMailSender 用 mail 服务 gRPC client 实现 biz.MailSender。
type GrpcMailSender struct {
	conn *grpc.ClientConn
	cli  mailv1.MailServiceClient
}

// NewGrpcMailSender 直连 mail 服务 endpoint(host:port,内网 insecure)。
func NewGrpcMailSender(mailAddr string) *GrpcMailSender {
	conn := grpcclient.MustDialInsecure(mailAddr)
	return &GrpcMailSender{conn: conn, cli: mailv1.NewMailServiceClient(conn)}
}

// Close 关闭底层连接。
func (g *GrpcMailSender) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

// SendOverflowMail 把溢出装备(itemConfigIDs 逐件)按 config_id 分组成 instance 形态附件,
// 调 mail.SendPersonalMail 发个人邮件。grantKey 传源键 battle_drop:{match}:{player},
// 领取时 GrantInstances 用它去重(与直发链共享 → 至多一次)。
func (g *GrpcMailSender) SendOverflowMail(ctx context.Context, playerID uint64, itemConfigIDs []uint32, grantKey string) error {
	if len(itemConfigIDs) == 0 {
		return nil
	}
	atts := groupInstanceAttachments(itemConfigIDs)
	resp, err := g.cli.SendPersonalMail(ctx, &mailv1.SendPersonalMailRequest{
		ToPlayerId:       playerID,
		Title:            overflowMailTitle,
		Body:             overflowMailBody,
		Attachments:      atts,
		InstanceGrantKey: grantKey,
	})
	if err != nil {
		return err
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "mail send personal code=%d", resp.GetCode())
	}
	return nil
}

// groupInstanceAttachments 把逐件展开的配置 ID 按 config_id 聚合成 count,拼 instance 形态附件。
func groupInstanceAttachments(itemConfigIDs []uint32) []*mailv1.MailAttachment {
	order := make([]uint32, 0, len(itemConfigIDs))
	counts := make(map[uint32]uint32, len(itemConfigIDs))
	for _, id := range itemConfigIDs {
		if _, ok := counts[id]; !ok {
			order = append(order, id)
		}
		counts[id]++
	}
	atts := make([]*mailv1.MailAttachment, 0, len(order))
	for _, id := range order {
		atts = append(atts, &mailv1.MailAttachment{
			Body: &mailv1.MailAttachment_Instance{
				Instance: &mailv1.InstanceAttachment{ItemConfigId: id, Count: counts[id]},
			},
		})
	}
	return atts
}
