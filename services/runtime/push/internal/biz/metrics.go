// Package biz —— W3 ④ 二次修复(Opus 审查 R2):push 服务业务级 prometheus 指标。
//
// 命名规范(docs/design/infra.md §10):
//
//	pandora_push_<metric>{<label>...}
//
// 强制 label:service / instance 由抓取端加,代码不写。
// 禁止高基数 label:player_id 永远不能放 label。
package biz

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/luyuancpp/pandora/pkg/metrics"
)

// OfflineAppendFailed 统计 KafkaConsumer.handle 中 offline.Append 失败次数。
//
// label:
//   - topic:具体 push topic 名(总数 ≤ 6,低基数)
//
// 触发场景:
//   - redis 不可达 / 超时
//   - PushFrame proto Marshal 失败(理论不会发生,只 defensive)
//
// 业务影响:这一帧没有任何持久化,客户端按 last_seen_ms 重连也补不回来。
// kafka 仍然 ack 该消息(handle 返 errcode 9301,kafkax 已配置 ack-all 策略),
// 退化为有损推送。告警阈值:rate(...[5m]) > 0 即告警(因为正常应为 0)。
var OfflineAppendFailed = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "pandora_push_offline_append_failed_total",
		Help: "push 服务 KafkaConsumer 把离线帧写入 redis ZSET 失败的总次数(应恒为 0,> 0 即需要告警)",
	},
	[]string{"topic"},
)

func init() {
	metrics.Register(OfflineAppendFailed)
}
