module github.com/luyuancpp/pandora/services/battle/ds_allocator

go 1.26.5

// W4 ② ds_allocator 服务(Pandora 第 6 个 Kratos 业务服,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.11):战斗 DS 调度(Agones GameServer)
//   AllocateBattle / ReleaseBattle / Heartbeat / ListBattles。
//   W4 ② 阶段用 MockGameServerAllocator(返回假 pod/addr)+ Redis DS 状态镜像;
//   真 Agones GameServerAllocation CRD 接入留后续环境就绪步(W4 ③+)。
//
// 依赖来源:
//   - pkg/        (公共框架,go.work use)
//   - proto/      (ds/v1 + common/v1)
//   - Kratos v2.9.2 / go-redis/v9(DS 镜像 + 心跳超时 ZSET 扫描)
//   - alicebob/miniredis/v2 (data 单测)

require (
	github.com/alicebob/miniredis/v2 v2.33.0
	github.com/go-kratos/kratos/v2 v2.9.2
	github.com/google/uuid v1.6.0
	github.com/luyuancpp/pandora/pkg v0.0.0-00010101000000-000000000000
	github.com/luyuancpp/pandora/proto v0.0.0-00010101000000-000000000000
	github.com/redis/go-redis/v9 v9.16.0
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
)

require (
	dario.cat/mergo v1.0.0 // indirect
	github.com/IBM/sarama v1.43.1 // indirect
	github.com/alicebob/gopher-json v0.0.0-20200520072559-a9ecdc9d1d3a // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/eapache/go-resiliency v1.6.0 // indirect
	github.com/eapache/go-xerial-snappy v0.0.0-20230731223053-c322873962e3 // indirect
	github.com/eapache/queue v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-kratos/aegis v0.2.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/go-playground/form/v4 v4.2.0 // indirect
	github.com/golang/snappy v0.0.4 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/hashicorp/errwrap v1.0.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-uuid v1.0.3 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/lufia/plan9stats v0.0.0-20230326075908-cb1d2100619a // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/power-devops/perfstat v0.0.0-20221212215047-62379fc7944b // indirect
	github.com/prometheus/client_golang v1.21.1 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.62.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20201227073835-cf1acfcdf475 // indirect
	github.com/shirou/gopsutil/v3 v3.23.6 // indirect
	github.com/shoenig/go-m1cpu v0.1.6 // indirect
	github.com/tklauser/go-sysconf v0.3.11 // indirect
	github.com/tklauser/numcpus v0.6.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	github.com/yusufpapurcu/wmi v1.2.3 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// 本地 workspace 内的模块通过 replace 指向源码目录。
replace (
	github.com/luyuancpp/pandora/pkg => ../../../pkg
	github.com/luyuancpp/pandora/proto => ../../../proto
)
