// Package grpcstats 提供一个 gRPC unary server 拦截器,采集每个方法的流量
// 统计(请求/响应字节数、调用次数、平均/最大延迟),按周期通过 logx
// 打印 TopN 报告。运行时通过 Enable/Disable 切换采集开关。
//
// 直接复用自 mmorpg/go/shared/grpcstats/。
//
// 使用:
//
//	collector := grpcstats.New(grpcstats.Options{})
//	server.AddUnaryInterceptors(collector.UnaryServerInterceptor())
//	collector.Enable(0)             // 0 = 永不自动关
//	collector.Enable(30*time.Minute) // 30 分钟后自动关
//	collector.Disable()
//
// 也可通过环境变量启用:
//
//	GRPC_TRAFFIC_STATS_ENABLED=1
//	GRPC_TRAFFIC_STATS_INTERVAL_SECONDS=30
//	GRPC_TRAFFIC_STATS_AUTO_DISABLE_MINUTES=60
package grpcstats

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	klog "github.com/go-kratos/kratos/v2/log"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// MethodStats 是单个 gRPC 方法的原子计数器集合。
type MethodStats struct {
	RecvCount  atomic.Int64
	RecvBytes  atomic.Int64
	RespBytes  atomic.Int64
	LatencySum atomic.Int64 // 微秒
	MaxReqSize atomic.Int64
	MaxLatency atomic.Int64 // 微秒
}

// MethodSnapshot 是 MethodStats 的快照。
type MethodSnapshot struct {
	FullMethod string
	RecvCount  int64
	RecvBytes  int64
	RespBytes  int64
	LatencyAvg time.Duration
	MaxReqSize int64
	MaxLatency time.Duration
}

func (s MethodSnapshot) TotalBytes() int64 { return s.RecvBytes + s.RespBytes }

// Options 配置 Collector。
type Options struct {
	// ReportInterval 报告周期,默认 30s。环境变量 GRPC_TRAFFIC_STATS_INTERVAL_SECONDS 覆盖。
	ReportInterval time.Duration
	// TopN 每次报告列出的方法数,默认 20。
	TopN int
}

// Collector 采集每方法的 gRPC 流量统计。
type Collector struct {
	enabled atomic.Bool
	methods sync.Map // fullMethod string → *MethodStats

	reportInterval time.Duration
	topN           int
	autoDisable    time.Time

	mu         sync.Mutex
	stopReport chan struct{}
	reporting  bool
}

// New 创建 Collector。环境变量 GRPC_TRAFFIC_STATS_ENABLED=1 时立即启动采集。
func New(opts Options) *Collector {
	interval := opts.ReportInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if envVal := os.Getenv("GRPC_TRAFFIC_STATS_INTERVAL_SECONDS"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v > 0 && v <= 3600 {
			interval = time.Duration(v) * time.Second
		}
	}

	topN := opts.TopN
	if topN <= 0 {
		topN = 20
	}

	c := &Collector{
		reportInterval: interval,
		topN:           topN,
	}

	if os.Getenv("GRPC_TRAFFIC_STATS_ENABLED") == "1" {
		autoMinutes := 0
		if envVal := os.Getenv("GRPC_TRAFFIC_STATS_AUTO_DISABLE_MINUTES"); envVal != "" {
			if v, err := strconv.Atoi(envVal); err == nil && v > 0 {
				autoMinutes = v
			}
		}
		c.Enable(time.Duration(autoMinutes) * time.Minute)
	}

	return c
}

// Enable 启动采集。autoDisableAfter > 0 时,经过该时长后自动停止。
func (c *Collector) Enable(autoDisableAfter time.Duration) {
	c.methods.Range(func(key, _ any) bool {
		c.methods.Delete(key)
		return true
	})

	c.mu.Lock()
	defer c.mu.Unlock()

	if autoDisableAfter > 0 {
		c.autoDisable = time.Now().Add(autoDisableAfter)
	} else {
		c.autoDisable = time.Time{}
	}

	c.enabled.Store(true)

	if !c.reporting {
		c.stopReport = make(chan struct{})
		c.reporting = true
		go c.reportLoop(c.stopReport)
	}

	klog.Infof("[grpcstats] Enabled. interval=%v auto_disable=%v", c.reportInterval, autoDisableAfter)
}

// Disable 停止采集并打印最后一次报告。
func (c *Collector) Disable() {
	c.enabled.Store(false)

	c.mu.Lock()
	if c.reporting {
		close(c.stopReport)
		c.reporting = false
	}
	c.mu.Unlock()

	c.reportOnce()
	klog.Info("[grpcstats] Disabled.")
}

// IsEnabled 返回当前是否在采集。
func (c *Collector) IsEnabled() bool {
	return c.enabled.Load()
}

// UnaryServerInterceptor 返回采集统计的 gRPC 拦截器。
func (c *Collector) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if !c.enabled.Load() {
			return handler(ctx, req)
		}

		reqSize := int64(0)
		if msg, ok := req.(proto.Message); ok {
			reqSize = int64(proto.Size(msg))
		}

		start := time.Now()
		resp, err := handler(ctx, req)
		latency := time.Since(start)

		respSize := int64(0)
		if msg, ok := resp.(proto.Message); ok {
			respSize = int64(proto.Size(msg))
		}

		stats := c.getOrCreateStats(info.FullMethod)
		stats.RecvCount.Add(1)
		stats.RecvBytes.Add(reqSize)
		stats.RespBytes.Add(respSize)
		stats.LatencySum.Add(latency.Microseconds())

		for {
			cur := stats.MaxReqSize.Load()
			if reqSize <= cur || stats.MaxReqSize.CompareAndSwap(cur, reqSize) {
				break
			}
		}

		latUs := latency.Microseconds()
		for {
			cur := stats.MaxLatency.Load()
			if latUs <= cur || stats.MaxLatency.CompareAndSwap(cur, latUs) {
				break
			}
		}

		return resp, err
	}
}

func (c *Collector) getOrCreateStats(fullMethod string) *MethodStats {
	if v, ok := c.methods.Load(fullMethod); ok {
		return v.(*MethodStats)
	}
	v, _ := c.methods.LoadOrStore(fullMethod, &MethodStats{})
	return v.(*MethodStats)
}

func (c *Collector) reportLoop(stop chan struct{}) {
	ticker := time.NewTicker(c.reportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.mu.Lock()
			autoDisable := c.autoDisable
			c.mu.Unlock()

			if !autoDisable.IsZero() && time.Now().After(autoDisable) {
				c.enabled.Store(false)
				klog.Info("[grpcstats] Auto-disabled after timeout.")
				c.reportOnce()
				c.mu.Lock()
				c.reporting = false
				c.mu.Unlock()
				return
			}

			c.reportOnce()
		}
	}
}

func (c *Collector) reportOnce() {
	var snapshots []MethodSnapshot

	c.methods.Range(func(key, value any) bool {
		method := key.(string)
		stats := value.(*MethodStats)

		recvCount := stats.RecvCount.Swap(0)
		recvBytes := stats.RecvBytes.Swap(0)
		respBytes := stats.RespBytes.Swap(0)
		latencySum := stats.LatencySum.Swap(0)
		maxReqSize := stats.MaxReqSize.Swap(0)
		maxLatency := stats.MaxLatency.Swap(0)

		if recvCount == 0 {
			return true
		}

		snapshots = append(snapshots, MethodSnapshot{
			FullMethod: method,
			RecvCount:  recvCount,
			RecvBytes:  recvBytes,
			RespBytes:  respBytes,
			LatencyAvg: time.Duration(latencySum/recvCount) * time.Microsecond,
			MaxReqSize: maxReqSize,
			MaxLatency: time.Duration(maxLatency) * time.Microsecond,
		})
		return true
	})

	if len(snapshots) == 0 {
		// 无流量不打日志:该报告是周期性的,空窗口刷 "no traffic" 只会淹没有效日志
		return
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].TotalBytes() > snapshots[j].TotalBytes()
	})

	var totalRecv, totalResp int64
	var totalCount int64
	for _, s := range snapshots {
		totalRecv += s.RecvBytes
		totalResp += s.RespBytes
		totalCount += s.RecvCount
	}

	klog.Infof("[grpcstats] window=%v total_calls=%d req_bytes=%s resp_bytes=%s",
		c.reportInterval, totalCount, formatBytes(totalRecv), formatBytes(totalResp))

	n := len(snapshots)
	if n > c.topN {
		n = c.topN
	}
	for i := 0; i < n; i++ {
		s := snapshots[i]
		klog.Infof("[grpcstats]   %s count=%d req=%s resp=%s avg_lat=%v max_lat=%v max_req=%s",
			s.FullMethod, s.RecvCount,
			formatBytes(s.RecvBytes), formatBytes(s.RespBytes),
			s.LatencyAvg, s.MaxLatency,
			formatBytes(s.MaxReqSize))
	}

	if len(snapshots) > c.topN {
		klog.Infof("[grpcstats]   ... and %d more methods", len(snapshots)-c.topN)
	}
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return strconv.FormatFloat(float64(b)/float64(1<<30), 'f', 1, 64) + "GB"
	case b >= 1<<20:
		return strconv.FormatFloat(float64(b)/float64(1<<20), 'f', 1, 64) + "MB"
	case b >= 1<<10:
		return strconv.FormatFloat(float64(b)/float64(1<<10), 'f', 1, 64) + "KB"
	default:
		return strconv.FormatInt(b, 10) + "B"
	}
}

// ShortMethod 把 "/package.Service/Method" 截短成 "Service/Method"。
func ShortMethod(fullMethod string) string {
	if idx := strings.LastIndex(fullMethod, "/"); idx >= 0 {
		parts := strings.SplitN(fullMethod[1:], "/", 2)
		if len(parts) == 2 {
			svc := parts[0]
			if dot := strings.LastIndex(svc, "."); dot >= 0 {
				svc = svc[dot+1:]
			}
			return svc + "/" + parts[1]
		}
	}
	return fullMethod
}
