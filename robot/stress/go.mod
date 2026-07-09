module github.com/luyuancpp/pandora/robot/stress

go 1.26.5

// robot/stress —— 阶段 1 单 Cell ~40 万 CCU 压测客户端机群(stressbot)。
//
// 设计:docs/design/stress-single-cell-client.md。
// 职责:
//   - 起 N 个虚拟玩家(VU),真实走 login → 大厅心跳(locator/player/team/friend/chat/auction)
//     → 组队匹配确认 → battle_result 上报的业务链路,给单 Cell 后端施压。
//   - 直连各服务 gRPC 端口(50001-50022),注入 metadata x-pandora-player-id /
//     x-pandora-trace-id 绕过 Envoy;小比例 VU 走 Envoy 8443 作对照样本。
//   - 每分钟把聚合指标追加到 robot-stats.jsonl(stress-discipline §5 summarize 的输入之一)。
//
// 依赖刻意精简(只 grpc + protobuf + proto),日志用 stdlib log/slog,配置用 JSON,
// 避免引入 yaml / pkg 依赖树,保证 robot 机群可离线构建、单文件下发。
//
// ⚠️ 本模块只负责"施压客户端",不含任何清库 / k8s / Agones 操作;
//   跑测前的清库与 prom 快照由 tools/scripts/dev_tools.ps1 + stress_snap.ps1 负责。
require (
	github.com/luyuancpp/pandora/proto v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
)

// 本地 workspace 内的模块通过 replace 指向源码目录。
replace github.com/luyuancpp/pandora/proto => ../../proto
