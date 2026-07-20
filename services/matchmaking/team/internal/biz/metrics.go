// Package biz —— team 服务业务级 prometheus 指标。
//
// 命名规范(docs/design/infra.md §10):
//
//	pandora_team_<metric>{<label>...}
//
// 强制 label:service / instance 由抓取端加,代码不写。
// 禁止高基数 label:player_id / team_id / invite_id 永远不能放 label。
package biz

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/luyuancpp/pandora/pkg/metrics"
)

// InvitePushFailed 统计邀请推送(kafka produce)丢帧次数。
//
// label:
//   - path:"dedicated"(独立 TeamInviteEvent, event_type=1,含 marshal 失败)
//     | "legacy"(TeamUpdateEvent reason=INVITE_SENT 承载的邀请)。低基数(2 值)。
//
// 触发场景:kafka broker 不可达 / produce 超时 / payload marshal 失败。
// 业务影响:邀请令牌已落库(Invite RPC 返回成功),但被邀请人收不到推送——
// 推送是弱依赖,不反向失败主流程;被邀请人靠 ListMyPendingInvites 拉取兜底。
// 此前只有 Warnw 日志,静默丢通知拖到用户报障才被发现,故必须计数 + 告警。
// 告警阈值:rate(...[5m]) > 0 即告警(正常应恒为 0)。
var InvitePushFailed = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "pandora_team_invite_push_failed_total",
		Help: "team 服务邀请推送发布失败的总次数(应恒为 0,> 0 即需要告警;被邀请人由 ListMyPendingInvites 拉取兜底)",
	},
	[]string{"path"},
)

func init() {
	metrics.Register(InvitePushFailed)
}
