// Package log 是 Pandora 服务的日志门面,基于 Kratos log.Logger interface + zap 实现。
//
// 设计目标:
//   - 统一所有服务的 service / trace_id / 业务字段约定(docs/design/infra.md §11)
//   - 提供一站式 Setup,业务侧不需要手写 zap.NewProduction
//   - 提供 With(ctx) 从 ctx 提取 trace_id / player_id 等业务字段(语义跟之前 logx 版一致)
//
// 框架决策(docs/design/pkg-copy-from-mmorpg.md §5):
//   - go-zero logx → Kratos log + zap(2026-06-04)
//   - Kratos log.Logger 是 interface,业务侧用 log.NewHelper(logger) 拿 Helper
package log

import (
	"context"
	"os"
	"strings"

	klog "github.com/go-kratos/kratos/v2/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/luyuancpp/pandora/pkg/version"
)

// ContextKey 是 log 包专用的 context key 类型,避免与业务 key 冲突。
type ContextKey string

const (
	CtxKeyTraceID  ContextKey = "trace_id"
	CtxKeyPlayerID ContextKey = "player_id"
	CtxKeyMatchID  ContextKey = "match_id"
	CtxKeyTeamID   ContextKey = "team_id"
)

// Setup 初始化日志系统,所有服务在 main() 开始处调用一次。
//
// 默认输出 JSON 到 stdout,Level=info;可用环境变量 LOG_LEVEL=debug|info|warn|error
// 覆盖(线上排障临时开 debug,不用重编)。
//
// 用法:
//
//	logger := log.Setup("login")
//	helper := log.NewHelper(logger)
//	helper.Info("service started")
func Setup(serviceName string) klog.Logger {
	encoderCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stack",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder, // 2006-01-02T15:04:05.000Z0700
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.Lock(os.Stdout),
		zap.NewAtomicLevelAt(levelFromEnv()),
	)

	zl := zap.New(core, zap.AddCaller(), zap.AddCallerSkip(3))

	// 包装成 Kratos log.Logger,挂上 service 字段
	logger := klog.With(NewZapLogger(zl),
		"service", serviceName,
	)

	// 也设成全局默认,业务侧 klog.DefaultLogger 可用
	klog.SetLogger(logger)

	// 启动即打印构建版本(编译期 -ldflags 注入,未注入则为 dev/unknown)。
	// 让每个线上 pod 都能从首行日志回答「我是哪个 git tag/commit 编出来的」。
	v := version.Get()
	klog.NewHelper(logger).Infow(
		"msg", "service starting",
		"version", v.Version,
		"commit", v.Commit,
		"build_time", v.BuildTime,
		"go_version", v.GoVersion,
	)

	return logger
}

// levelFromEnv 从 LOG_LEVEL 环境变量解析日志级别,空/非法值一律回落 info。
func levelFromEnv() zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		return zap.DebugLevel
	case "warn":
		return zap.WarnLevel
	case "error":
		return zap.ErrorLevel
	default:
		return zap.InfoLevel
	}
}

// NewHelper 是 klog.NewHelper 的别名,业务侧用它拿 Helper。
//
//	h := log.NewHelper(log.Setup("login"))
//	h.Info("hello")
//	h.Errorw("redis_failed", "err", err, "key", "k1")
func NewHelper(l klog.Logger) *klog.Helper {
	return klog.NewHelper(l)
}

// With 从 ctx 抽出标准业务字段,返回带这些字段的 Helper。
//
// 调用方:
//
//	log.With(ctx).Infow("match found", "mmr", 1234)
//
// 标准字段(infra.md §11):trace_id / player_id / match_id / team_id
func With(ctx context.Context) *klog.Helper {
	logger := klog.DefaultLogger
	kvs := []any{}

	if v := ctx.Value(CtxKeyTraceID); v != nil {
		kvs = append(kvs, "trace_id", v)
	}
	if v := ctx.Value(CtxKeyPlayerID); v != nil {
		kvs = append(kvs, "player_id", v)
	}
	if v := ctx.Value(CtxKeyMatchID); v != nil {
		kvs = append(kvs, "match_id", v)
	}
	if v := ctx.Value(CtxKeyTeamID); v != nil {
		kvs = append(kvs, "team_id", v)
	}

	if len(kvs) > 0 {
		logger = klog.With(logger, kvs...)
	}
	return klog.NewHelper(logger)
}

// WithTraceID 把 trace_id 塞进 ctx,后续 With(ctx) 自动带上。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, CtxKeyTraceID, traceID)
}

// WithPlayerID 把 player_id 塞进 ctx。
func WithPlayerID(ctx context.Context, playerID uint64) context.Context {
	return context.WithValue(ctx, CtxKeyPlayerID, playerID)
}

// WithMatchID 把 match_id 塞进 ctx。
func WithMatchID(ctx context.Context, matchID uint64) context.Context {
	return context.WithValue(ctx, CtxKeyMatchID, matchID)
}

// WithTeamID 把 team_id 塞进 ctx。
func WithTeamID(ctx context.Context, teamID uint64) context.Context {
	return context.WithValue(ctx, CtxKeyTeamID, teamID)
}
