module github.com/luyuancpp/pandora/pkg/leader/etcdleader

go 1.26.5

// 后台单例任务的 etcd 选举器(opt-in,独立 module 隔离重型 etcd client 依赖)。
//
// 为什么单独成 module:与 pkg/snowflake/etcdnode、pkg/killswitch/etcdkv 同理——
// go.etcd.io/etcd/client/v3(含 concurrency 子包)依赖较重,不让核心 pkg 及所有业务服务
// 无条件背上 etcd client。只有真正进入多副本、需要"仅一个副本跑后台单例循环"(如 matchmaker
// 撮合循环)的服务,才在 main 里 import 本包。
//
// ⚠️ 本 module 引入 go.etcd.io/etcd/client/v3,需 Codex 执行:
//   1. 把 `use ./pkg/leader/etcdleader` 加入根 go.work(Claude 已写入,复核即可)
//   2. 在本目录 `go mod tidy` 拉取 etcd client 并生成 go.sum
// 版本号(v3.5.x)对齐 pkg/snowflake/etcdnode,可由 tidy 按可用版本微调。

require (
	github.com/go-kratos/kratos/v2 v2.9.2
	go.etcd.io/etcd/client/v3 v3.5.16
)

require (
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.3.2 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	go.etcd.io/etcd/api/v3 v3.5.16 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.16 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/grpc v1.79.3 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
