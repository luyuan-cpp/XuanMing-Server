// Package biz 是 mail 服务的业务逻辑层(2026-06-29)。
//
// 职责(docs/design/mail.md):
//   - ListMail:个人邮件(写扩散)+ 系统/公会邮件(channel+watermark 拉取)合并视图,
//     拉取后推进游标(last_sys/last_guild),实现"看过的不重复拉、过期的不拉"
//   - ReadMail:个人邮件置已读;系统/公会邮件推进游标
//   - ClaimMail:附件领取,player_mail_claim 幂等(同 mail+player 只发一次)
//   - SendSystemMail/SendGuildMail:只插一行(零写扩散,僵尸/退游不登录即零成本)
//   - SendPersonalMail:写收件人收件箱(离线可达)
//
// 客户端只拿 Mail / MailAttachment 视图(CLAUDE.md §14):正文+附件存 payload blob,
// 服务端解包成最小视图返回。
package biz

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"

	"github.com/luyuancpp/pandora/services/social/mail/internal/conf"
	"github.com/luyuancpp/pandora/services/social/mail/internal/data"
)

// ItemGranter 把附件入背包(由 inventory 服务实现,幂等键防重发)。
type ItemGranter interface {
	Grant(ctx context.Context, playerID uint64, atts []*mailv1.MailAttachment, idempotencyKey string) error
}

// InstanceGranter 把"装备实例型"附件(as_instance=true)按 count 逐件铸造为独立实例
// (inventory.GrantInstances,含随机词条),幂等键防重发。战斗装备掉落背包满转邮件用此路径,
// 保持装备=唯一实例语义。实现可为 nil:未配 inventory 时装备型附件领取直接报错(不误发成堆叠)。
type InstanceGranter interface {
	GrantInstances(ctx context.Context, playerID uint64, itemConfigIDs []uint32, idempotencyKey string) error
}

// MailUsecase 是 mail 服务业务逻辑核心。
type MailUsecase struct {
	repo        data.MailRepo
	cfg         conf.MailConf
	granter     ItemGranter
	instGranter InstanceGranter
}

// NewMailUsecase 构造。granter 为 nil 时仅允许 AllowNoopGrant 配置下空领(测试用)。
// instGranter 用 setter 注入(SetInstanceGranter),保持构造签名不变。
func NewMailUsecase(repo data.MailRepo, cfg conf.MailConf, granter ItemGranter) *MailUsecase {
	return &MailUsecase{repo: repo, cfg: cfg, granter: granter}
}

// SetInstanceGranter 注入装备实例发放器(领取 as_instance 附件用,nil-safe)。
// nil / 不调用 = 装备型附件领取报错(不会误发成可堆叠计数,守住装备实例不变量)。
func (u *MailUsecase) SetInstanceGranter(g InstanceGranter) {
	u.instGranter = g
}

// 分页上限(决策:docs/design/decision-revisit-list-pagination.md)。
const (
	defaultPageLimit = 50
	maxPageLimit     = 100
)

// clampLimit 把 0 归默认、超上限收敛。
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// ListMail 合并三类邮件,分页拉取个人邮件,首页(cursor=0)拼系统/公会 watermark 增量。
// nextCursor 为本页末个人邮件 mail_id;0=个人邮件无更多。
func (u *MailUsecase) ListMail(ctx context.Context, playerID uint64, nowMs int64, cursor uint64, limit int) ([]*mailv1.Mail, uint64, error) {
	limit = clampLimit(limit)
	lastSys, lastGuild, err := u.repo.GetCursor(ctx, playerID)
	if err != nil {
		return nil, 0, err
	}

	var out []*mailv1.Mail
	maxSys, maxGuild := lastSys, lastGuild

	personal, err := u.repo.ListPersonal(ctx, playerID, nowMs, cursor, limit)
	if err != nil {
		return nil, 0, err
	}
	var nextCursor uint64
	if len(personal) == limit && limit > 0 {
		nextCursor = personal[len(personal)-1].MailID
	}
	for _, m := range personal {
		out = append(out, toMail(m, mailv1.MailChannel_MAIL_CHANNEL_PERSONAL, m.Status, m.Claimed))
	}

	// 系统/公会邮件靠 watermark 天然有界,仅首页拼接,翻页只走个人邮件。
	if cursor != 0 {
		return out, nextCursor, nil
	}

	sys, err := u.repo.ListSysSince(ctx, lastSys, nowMs)
	if err != nil {
		return nil, 0, err
	}
	for _, m := range sys {
		out = append(out, u.toChannelMail(ctx, playerID, m, mailv1.MailChannel_MAIL_CHANNEL_SYSTEM))
		if m.MailID > maxSys {
			maxSys = m.MailID
		}
	}

	if gid, ok, err := u.repo.GetPlayerGuild(ctx, playerID); err != nil {
		return nil, 0, err
	} else if ok {
		guildMails, err := u.repo.ListGuildSince(ctx, gid, lastGuild, nowMs)
		if err != nil {
			return nil, 0, err
		}
		for _, m := range guildMails {
			out = append(out, u.toChannelMail(ctx, playerID, m, mailv1.MailChannel_MAIL_CHANNEL_GUILD))
			if m.MailID > maxGuild {
				maxGuild = m.MailID
			}
		}
	}

	if maxSys > lastSys || maxGuild > lastGuild {
		if err := u.repo.AdvanceCursor(ctx, playerID, maxSys, maxGuild); err != nil {
			return nil, 0, err
		}
	}
	return out, nextCursor, nil
}

// ReadMail 个人邮件置已读;系统/公会邮件靠游标(ListMail 已推进),此处幂等返回。
func (u *MailUsecase) ReadMail(ctx context.Context, playerID, mailID uint64) error {
	return u.repo.SetPersonalStatus(ctx, playerID, mailID, data.StatusRead)
}

// ClaimMail 领附件,幂等。返回实发清单(无附件返回空)。
//
// 安全:GetClaimablePayload 按 channel 校验领取人权限(个人=收件人本人 / 系统=任意 /
// 公会=当前会员)+ 生效区间,越权直接 NotFound。
// 顺序:先校验 → 先调 inventory 入库(幂等键 mail:{mail}:{player},inventory 自身幂等)→
// 入库成功后写 player_mail_claim 标记;crash 在写标记前不致丢奖(下次重领靠 inventory 幂等去重)。
func (u *MailUsecase) ClaimMail(ctx context.Context, playerID, mailID uint64, nowMs int64) ([]*mailv1.MailAttachment, error) {
	payload, found, err := u.repo.GetClaimablePayload(ctx, playerID, mailID, nowMs)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errcode.New(errcode.ErrMailNotFound, "mail %d not found or not claimable", mailID)
	}
	rec := &mailv1.MailContentStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "decode mail %d: %v", mailID, err)
	}
	if len(rec.GetAttachments()) == 0 {
		return nil, errcode.New(errcode.ErrMailNoAttachment, "mail %d has no attachment", mailID)
	}
	if claimed, err := u.repo.HasClaimed(ctx, playerID, mailID); err != nil {
		return nil, err
	} else if claimed {
		return rec.GetAttachments(), errcode.New(errcode.ErrMailAlreadyClaimed, "mail %d already claimed", mailID)
	}
	// 入库:幂等键保证重复领取/重试不重发(资产不变量 §7)。
	// 附件分两类:可堆叠(as_instance=false → GrantItems)+ 装备实例(as_instance=true → GrantInstances)。
	// 各用独立幂等键,重领/重试各自去重;先发可堆叠再铸实例,任一步失败下次重领靠幂等去重不重发。
	stackAtts, instAtts := partitionAttachments(rec.GetAttachments())
	if len(stackAtts) > 0 {
		key := fmt.Sprintf("mail:%d:%d", mailID, playerID)
		if u.granter != nil {
			if err := u.granter.Grant(ctx, playerID, stackAtts, key); err != nil {
				return nil, err
			}
		} else if !u.cfg.AllowNoopGrant {
			return nil, errcode.New(errcode.ErrInternal, "inventory granter unavailable")
		}
	}
	if len(instAtts) > 0 {
		key := rec.GetInstanceGrantKey()
		if key == "" {
			key = fmt.Sprintf("mail_inst:%d:%d", mailID, playerID)
		}
		if u.instGranter != nil {
			if err := u.instGranter.GrantInstances(ctx, playerID, expandInstanceConfigIDs(instAtts), key); err != nil {
				return nil, err
			}
		} else if !u.cfg.AllowNoopGrant {
			return nil, errcode.New(errcode.ErrInternal, "instance granter unavailable")
		}
	}
	// 入库成功后再记 claim;此处即便失败,下次重领被 inventory 幂等去重,不会重发
	if _, err := u.repo.RecordClaim(ctx, playerID, mailID); err != nil {
		return nil, err
	}
	// 个人邮件置 claimed(系统/公会靠 player_mail_claim 表)
	_ = u.repo.SetPersonalStatus(ctx, playerID, mailID, data.StatusClaimed)
	return rec.GetAttachments(), nil
}

// DeleteMail 删个人邮件。
func (u *MailUsecase) DeleteMail(ctx context.Context, playerID, mailID uint64) error {
	return u.repo.DeletePersonal(ctx, playerID, mailID)
}

// SendSystemMail 插一行系统邮件,返回 mail_id。
func (u *MailUsecase) SendSystemMail(ctx context.Context, mailID uint64, title, body string, atts []*mailv1.MailAttachment, startMs, endMs, nowMs int64) (uint64, error) {
	payload, err := u.buildPayload(title, body, atts, "")
	if err != nil {
		return 0, err
	}
	endMs = u.defaultEnd(startMs, endMs, nowMs)
	if err := u.repo.InsertSysMail(ctx, mailID, startMs, endMs, payload); err != nil {
		return 0, err
	}
	return mailID, nil
}

// SendGuildMail 插一行公会邮件。
func (u *MailUsecase) SendGuildMail(ctx context.Context, mailID, guildID uint64, title, body string, atts []*mailv1.MailAttachment, startMs, endMs, nowMs int64) (uint64, error) {
	if guildID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "guild_id required")
	}
	payload, err := u.buildPayload(title, body, atts, "")
	if err != nil {
		return 0, err
	}
	endMs = u.defaultEnd(startMs, endMs, nowMs)
	if err := u.repo.InsertGuildMail(ctx, mailID, guildID, startMs, endMs, payload); err != nil {
		return 0, err
	}
	return mailID, nil
}

// SendPersonalMail 写收件人收件箱(离线可达)。instanceGrantKey 非空时存入 payload,
// 领取 as_instance 附件走 GrantInstances 用此键去重(战斗掉落转邮件与直发共享源键 → 至多一次)。
// expireMs=0 时补默认 TTL(一切邮件生命有限,sweep 清理的前提);写入侧上限见 InsertPersonalMail。
func (u *MailUsecase) SendPersonalMail(ctx context.Context, mailID, toPlayerID uint64, title, body string, atts []*mailv1.MailAttachment, expireMs, nowMs int64, instanceGrantKey string) (uint64, error) {
	if toPlayerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "to_player_id required")
	}
	payload, err := u.buildPayload(title, body, atts, instanceGrantKey)
	if err != nil {
		return 0, err
	}
	if expireMs == 0 {
		expireMs = nowMs + int64(u.cfg.DefaultPersonalTtlDays)*86400_000
	}
	if err := u.repo.InsertPersonalMail(ctx, mailID, toPlayerID, expireMs, payload, u.cfg.MaxInboxSize); err != nil {
		return 0, err
	}
	return mailID, nil
}

// partitionAttachments 把附件按 as_instance 分成可堆叠(stack)与装备实例(inst)两组。
func partitionAttachments(atts []*mailv1.MailAttachment) (stack, inst []*mailv1.MailAttachment) {
	for _, a := range atts {
		if a.GetAsInstance() {
			inst = append(inst, a)
		} else {
			stack = append(stack, a)
		}
	}
	return stack, inst
}

// expandInstanceConfigIDs 把装备实例型附件按 count 逐件展开成配置 ID 列表(count 份 → count 个元素)。
// count<=1 视为 1 件;供 inventory.GrantInstances 逐件铸造独立实例。
func expandInstanceConfigIDs(atts []*mailv1.MailAttachment) []uint32 {
	out := make([]uint32, 0, len(atts))
	for _, a := range atts {
		n := a.GetCount()
		if n == 0 {
			n = 1
		}
		for i := uint32(0); i < n; i++ {
			out = append(out, a.GetItemConfigId())
		}
	}
	return out
}

func (u *MailUsecase) defaultEnd(startMs, endMs, nowMs int64) int64 {
	if endMs != 0 {
		return endMs
	}
	base := startMs
	if base == 0 {
		base = nowMs
	}
	return base + int64(u.cfg.DefaultSysTtlDays)*86400_000
}

func (u *MailUsecase) buildPayload(title, body string, atts []*mailv1.MailAttachment, instanceGrantKey string) ([]byte, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errcode.New(errcode.ErrInvalidArg, "title required")
	}
	if utf8.RuneCountInString(title) > u.cfg.MaxTitleLen {
		return nil, errcode.New(errcode.ErrInvalidArg, "title too long")
	}
	if utf8.RuneCountInString(body) > u.cfg.MaxBodyLen {
		return nil, errcode.New(errcode.ErrInvalidArg, "body too long")
	}
	if len(atts) > u.cfg.MaxAttachments {
		return nil, errcode.New(errcode.ErrInvalidArg, "too many attachments")
	}
	rec := &mailv1.MailContentStorageRecord{Title: title, Body: body, Attachments: atts, InstanceGrantKey: instanceGrantKey}
	return proto.Marshal(rec)
}

func (u *MailUsecase) toChannelMail(ctx context.Context, playerID uint64, m data.MailRow, ch mailv1.MailChannel) *mailv1.Mail {
	claimed, _ := u.repo.HasClaimed(ctx, playerID, m.MailID)
	status := mailv1.MailStatus_MAIL_STATUS_READ // 拉取即视为已读
	if claimed {
		status = mailv1.MailStatus_MAIL_STATUS_CLAIMED
	}
	mail := decodePayload(m.Payload)
	mail.MailId = m.MailID
	mail.Channel = ch
	mail.Status = status
	mail.Claimed = claimed
	mail.CreatedMs = m.CreatedMs
	mail.ExpireMs = m.EndMs
	return mail
}

func toMail(m data.MailRow, ch mailv1.MailChannel, status int32, claimed bool) *mailv1.Mail {
	mail := decodePayload(m.Payload)
	mail.MailId = m.MailID
	mail.Channel = ch
	mail.Status = mailv1.MailStatus(status)
	mail.Claimed = claimed
	mail.CreatedMs = m.CreatedMs
	mail.ExpireMs = m.ExpireMs
	return mail
}

func decodePayload(payload []byte) *mailv1.Mail {
	rec := &mailv1.MailContentStorageRecord{}
	_ = proto.Unmarshal(payload, rec)
	return &mailv1.Mail{
		Title:       rec.GetTitle(),
		Body:        rec.GetBody(),
		Attachments: rec.GetAttachments(),
	}
}
