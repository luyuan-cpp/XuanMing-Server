module github.com/luyuancpp/pandora/services/runtime/push

go 1.25.0

// W3 ④ push 服务(2026-06-05 真实化,W2 ⑤ mock tick 已退役)。
//
// 依赖来源:
//   - pkg/ (公共框架,go.work use + replace)
//   - proto/gen/go/pandora/push/v1 (PushService 协议生成)
//   - proto/gen/go/pandora/common/v1 (错误码)
//   - Kratos v2.9.2 / google.golang.org/grpc(server stream API)
//   - IBM/sarama v1.43.1 (KafkaConsumer,每 topic 一个,共享 GroupID)
//   - redis/go-redis/v9 (RedisOfflineCacheRepo,ZSET pandora:push:offline:%d,5min TTL)
//   - alicebob/miniredis/v2 (单测 redis 内存实现,不需 docker)
//
// 当前行为:
//   - Subscribe 走 Envoy jwt_authn → x-pandora-player-id header → pmw.AuthOptional()
//   - 消费 pandora.team.update / pandora.match.progress / pandora.chat.private 三个 topic
//   - 在线玩家直推 stream;send 失败或离线 → 写 redis ZSET;客户端重连按 last_seen_ms 补推

require (
	github.com/alicebob/miniredis/v2 v2.33.0
	github.com/go-kratos/kratos/v2 v2.9.2
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
	github.com/go-playground/form/v4 v4.2.0 // indirect
	github.com/golang/snappy v0.0.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
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
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/prometheus/client_golang v1.21.1 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.62.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20201227073835-cf1acfcdf475 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
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
// 这样 `go mod tidy` 不会去 vcs 抓远端版本(远端目前没 tag),
// 同时 `go build`(在 go.work 下)也能正常解析。
replace (
	github.com/luyuancpp/pandora/pkg => ../../../pkg
	github.com/luyuancpp/pandora/proto => ../../../proto
)
