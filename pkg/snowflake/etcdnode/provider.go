package etcdnode

import (
	"context"
	"io"
	"os"

	klog "github.com/go-kratos/kratos/v2/log"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/snowflake"
)

// noopCloser 是 static 模式下返回的空 Closer(无 etcd lease 可释放)。
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// ProvideSnowflake 按 sf.NodeIDSource 一次性装配服务的雪花发号器,
// 把「static 直接发号」与「etcd 抢占独占 nodeID + fencing 退出」两条路径收敛成一个调用,
// 让每个服务 main.go 接线只需一行,避免各自重复实现 Lost() 退出契约而漏写导致双活发重号。
//
// 语义:
//   - sf.NodeIDSource 为空 / "static":用 staticNodeID(来自 node.node_id)本地发号;
//     返回的 io.Closer 是 no-op,不引入任何 etcd 依赖行为。
//   - sf.NodeIDSource == "etcd":调用 Acquire 在 etcd 里抢占一个独占 nodeID,并**在内部**
//     起一个 goroutine 履行 fencing 契约——一旦 Holder.Lost()(续租失败 / lease 被 revoke)
//     就 os.Exit(1) 主动退出,交给 k8s 重新拉起重新抢号,杜绝同 nodeID 双活。返回的
//     io.Closer 即 *Holder,进程正常退出时 Close() 会 revoke lease 让 nodeID 立即可复用。
//
// 用法(进入多副本阶段的服务 main.go):
//
//	sf, sfCloser, err := etcdnode.ProvideSnowflake(ctx, serviceName, cfg.Node.NodeId, cfg.Snowflake)
//	if err != nil {
//	    helper.Errorw("msg", "snowflake_provider_failed", "err", err)
//	    os.Exit(1)
//	}
//	defer func() { _ = sfCloser.Close() }()
//
// static 模式下 err 恒为 nil;只有 etcd 模式在 etcd 不可达 / 号段占满时才返回 err。
func ProvideSnowflake(
	ctx context.Context,
	service string,
	staticNodeID uint32,
	sf config.SnowflakeConf,
) (*snowflake.Node, io.Closer, error) {
	if sf.NodeIDSource != "etcd" {
		return snowflake.NewNode(uint64(staticNodeID)), noopCloser{}, nil
	}

	holder, err := Acquire(ctx, Config{
		Endpoints:   sf.EtcdEndpoints,
		Service:     etcdKeyService(service, sf.EtcdServiceName),
		Prefix:      sf.EtcdPrefix,
		LeaseTTLSec: sf.EtcdLeaseTTLSec,
	})
	if err != nil {
		return nil, nil, err
	}

	// fencing 契约(见 package doc):失租必须停止发号并退出进程。集中在此实现,
	// 各服务无需自行 select Lost(),避免漏写导致同 nodeID 双活发重号。
	go func() {
		<-holder.Lost()
		klog.Errorf("[snowflake] nodeID lease LOST service=%s node_id=%d — exiting to avoid dual-active",
			service, holder.NodeID())
		os.Exit(1)
	}()

	return holder.Node(), holder, nil
}

// MustProvideSnowflake 是 ProvideSnowflake 的便捷版:自管 context,失败(仅 etcd 模式可能)
// 直接 os.Exit(1)。static 模式恒不失败,退化为 snowflake.NewNode。
//
// 让每个服务 main.go 接线只需两行、且无需引入 context:
//
//	sf, sfCloser := etcdnode.MustProvideSnowflake(serviceName, cfg.Node.NodeId, cfg.Snowflake)
//	defer func() { _ = sfCloser.Close() }()
func MustProvideSnowflake(
	service string,
	staticNodeID uint32,
	sf config.SnowflakeConf,
) (*snowflake.Node, io.Closer) {
	node, closer, err := ProvideSnowflake(context.Background(), service, staticNodeID, sf)
	if err != nil {
		klog.Errorf("[snowflake] MustProvideSnowflake failed service=%s err=%v", service, err)
		os.Exit(1)
	}
	return node, closer
}

// etcdKeyService 决定 etcd key 命名空间:显式配了 etcd_service_name 用它,否则用服务名。
// 不同服务类型各自一套 [0, MaxNodeID) 空间,故 node_id 可跨服务复用、同服务内唯一。
func etcdKeyService(service, override string) string {
	if override != "" {
		return override
	}
	return service
}
