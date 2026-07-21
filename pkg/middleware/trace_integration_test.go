// trace_integration_test.go — 真实 Kratos gRPC server/client 回环集成测试(§16.7)。
//
// 复现 2026-07-21 ds_allocator Heartbeat 崩溃的完整链路:server handler 返回后,
// 由请求 ctx 经 context.WithoutCancel 派生的后台 goroutine 再经挂 Trace/Metrics
// middleware 的 Kratos client 发下游 RPC——此时 gRPC 正在发送原响应(遍历
// ReplyHeader metadata Map)。旧 Trace 实现会在 client hop 写同一 Map,
// -race 直接报 concurrent map iteration and map write;修复后必须全程无 race。
//
// 注意 handler 里的 context.WithoutCancel(ctx) 是「业务侧误用」的还原,不是推荐写法
// (业务侧应一律用 plog.Detach);本测试锁的是 middleware 层在误用下也不崩的兜底安全。
package middleware

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v2/transport"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	testEchoService = "pandora.test.TraceEcho"
	testPingMethod  = "/pandora.test.TraceEcho/Ping"
	testFanMethod   = "/pandora.test.TraceEcho/Fan"
)

// traceEchoServer 是测试服务实现:Ping 是叶子调用;Fan 在返回前 spawn 后台下游 Ping。
type traceEchoServer struct {
	conn *grpc.ClientConn // 指向本服务自身的 Kratos client conn(测试装配后注入)
	wg   sync.WaitGroup   // 等待所有后台下游调用结束,避免测试退出后仍有 RPC 在飞
}

func (s *traceEchoServer) Ping(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *traceEchoServer) Fan(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	// 补一个自定义回程 header,保证响应发送路径确实要遍历/合并 ReplyHeader Map。
	if tr, ok := transport.FromServerContext(ctx); ok {
		tr.ReplyHeader().Set("x-test-fan", "1")
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// 故意用 WithoutCancel 继承请求 ctx(含 server transport),还原线上误用姿势。
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		out := new(emptypb.Empty)
		_ = s.conn.Invoke(cctx, testPingMethod, &emptypb.Empty{}, out)
	}()
	return &emptypb.Empty{}, nil
}

// testEchoDesc 手写 grpc.ServiceDesc,免去为测试引入 proto 生成物。
var testEchoDesc = grpc.ServiceDesc{
	ServiceName: testEchoService,
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Ping", Handler: testUnaryHandler(testPingMethod, func(s *traceEchoServer, ctx context.Context, in *emptypb.Empty) (*emptypb.Empty, error) {
			return s.Ping(ctx, in)
		})},
		{MethodName: "Fan", Handler: testUnaryHandler(testFanMethod, func(s *traceEchoServer, ctx context.Context, in *emptypb.Empty) (*emptypb.Empty, error) {
			return s.Fan(ctx, in)
		})},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "trace_integration_test",
}

func testUnaryHandler(
	fullMethod string,
	invoke func(*traceEchoServer, context.Context, *emptypb.Empty) (*emptypb.Empty, error),
) func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(emptypb.Empty)
		if err := dec(in); err != nil {
			return nil, err
		}
		impl := srv.(*traceEchoServer)
		if interceptor == nil {
			return invoke(impl, ctx, in)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: fullMethod}
		return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
			return invoke(impl, ctx, req.(*emptypb.Empty))
		})
	}
}

// TestTraceBackgroundRPCConcurrentWithResponseSend:必须 -race 跑。
// 反复调用 Fan:每次 handler 返回(触发响应 header 发送)都与后台下游 Ping 的
// client middleware 并发。旧 Trace 实现在此场景写正在发送的 ReplyHeader → race;
// 修复后全程干净,且所有请求成功。
func TestTraceBackgroundRPCConcurrentWithResponseSend(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test, skipped in -short")
	}

	impl := &traceEchoServer{}
	srv := kgrpc.NewServer(
		kgrpc.Address("127.0.0.1:0"),
		kgrpc.Middleware(Trace(), Metrics(), Logging()),
	)
	srv.RegisterService(&testEchoDesc, impl)

	endpoint, err := srv.Endpoint()
	if err != nil {
		t.Fatalf("server endpoint: %v", err)
	}
	go func() { _ = srv.Start(context.Background()) }()
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	conn, err := kgrpc.DialInsecure(dialCtx,
		kgrpc.WithEndpoint(endpoint.Host),
		kgrpc.WithMiddleware(Trace(), Metrics(), Logging()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	impl.conn = conn

	for i := 0; i < 300; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out := new(emptypb.Empty)
		if err := conn.Invoke(ctx, testFanMethod, &emptypb.Empty{}, out); err != nil {
			cancel()
			t.Fatalf("Fan #%d: %v", i, err)
		}
		cancel()
	}
	impl.wg.Wait()
}
