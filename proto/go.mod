module github.com/luyuancpp/pandora/proto

go 1.26.4

// Pandora 协议 module。
//
// 只包含 `gen/go/...` 下 buf 生成的 .pb.go / *_grpc.pb.go / *_http.pb.go。
// 业务服务在自己的 go.mod 里通过 `replace` 指向本地 `proto/`,在 go.work 里 `use ./proto`。
//
// 不允许在这个 module 里手写 .go 文件 —— 全是生成产物,人改任何手写代码会被下次 buf generate 覆盖。

require (
	github.com/go-kratos/kratos/v2 v2.9.2
	google.golang.org/genproto/googleapis/api v0.0.0-20251202230838-ff82c1b0f217
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/go-kratos/aegis v0.2.0 // indirect
	github.com/go-playground/form/v4 v4.2.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
