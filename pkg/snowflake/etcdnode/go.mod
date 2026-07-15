module github.com/luyuancpp/pandora/pkg/snowflake/etcdnode

go 1.26.5

// Snowflake nodeID 的 etcd Lease 自动分配器(opt-in,独立 module 隔离重型 etcd client 依赖)。
//
// 为什么单独成 module:与 pkg/killswitch/etcdkv 同理——go.etcd.io/etcd/client/v3 依赖较重,
// 不让核心 pkg/snowflake 及所有业务服务无条件背上 etcd client。只有真正进入 k8s 多副本
// 动态扩缩、需要自动分配 nodeID 的服务,才在 main 里 import 本包。
//
// ⚠️ 本 module 引入 go.etcd.io/etcd/client/v3,需 Codex 执行:
//   1. 把 `use ./pkg/snowflake/etcdnode` 加入根 go.work
//   2. 在本目录 `go mod tidy` 拉取 etcd client 并生成 go.sum
// 版本号(v3.5.x)对齐 pkg/killswitch/etcdkv,可由 tidy 按可用版本微调。

require (
	github.com/go-kratos/kratos/v2 v2.9.2
	github.com/luyuancpp/pandora/pkg v0.0.0
	go.etcd.io/etcd/client/v3 v3.5.16
)

require (
	github.com/IBM/sarama v1.43.1 // indirect
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.3.2 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/eapache/go-resiliency v1.6.0 // indirect
	github.com/eapache/go-xerial-snappy v0.0.0-20230731223053-c322873962e3 // indirect
	github.com/eapache/queue v1.1.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/golang/snappy v0.0.4 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-uuid v1.0.3 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20201227073835-cf1acfcdf475 // indirect
	go.etcd.io/etcd/api/v3 v3.5.16 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.16 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/grpc v1.79.3 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// 复用本地 pkg 公共框架(对齐 pkg/killswitch/etcdkv 的 replace 写法)。
replace github.com/luyuancpp/pandora/pkg => ../..
