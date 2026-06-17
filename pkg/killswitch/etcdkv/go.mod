module github.com/luyuancpp/pandora/pkg/killswitch/etcdkv

go 1.26.4

// Kill-Switch 的 etcd 开关源(opt-in,独立 module 隔离重型 etcd client 依赖)。
//
// ⚠️ 本 module 引入 go.etcd.io/etcd/client/v3,需 Codex 执行:
//   1. 把 `use ./pkg/killswitch/etcdkv` 加入根 go.work
//   2. 在本目录 `go mod tidy` 拉取 etcd client 并生成 go.sum
// 版本号(v3.5.x)可由 tidy 按可用版本微调。

require (
	github.com/go-kratos/kratos/v2 v2.9.2
	github.com/luyuancpp/pandora/pkg v0.0.0
	go.etcd.io/etcd/client/v3 v3.5.16
)

require (
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.3.2 // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
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
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// 复用本地 pkg 公共框架(对齐各 services 的 replace 写法)。
replace github.com/luyuancpp/pandora/pkg => ../..
