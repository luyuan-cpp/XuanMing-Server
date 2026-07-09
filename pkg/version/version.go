// Package version 暴露服务的构建版本信息(编译期由 -ldflags -X 注入)。
//
// 设计目标:让每个线上 pod 都能回答「我是哪个 git commit / 哪个 tag 编出来的」,
// 实现「线上跑的二进制 ↔ git 某次提交」可追溯,而**不需要把二进制提交进仓库**。
//
// 注入方式(由 deploy/services/Dockerfile + tools/scripts/start.ps1 自动完成):
//
//	go build -ldflags "\
//	  -X github.com/luyuancpp/pandora/pkg/version.Version=v1.2.3 \
//	  -X github.com/luyuancpp/pandora/pkg/version.Commit=abc1234 \
//	  -X github.com/luyuancpp/pandora/pkg/version.BuildTime=2026-06-21T10:00:00Z"
//
// 不注入时(本地 go run / 直接 go build)保留下面的默认值,服务仍能正常启动。
package version

import (
	"fmt"
	"runtime"
)

// 这些变量由编译期 -ldflags -X 覆盖;未注入时用占位默认值。
// 注意:必须是包级 var(非 const),否则 ldflags 无法写入。
var (
	// Version 是语义化版本号,通常取 `git describe --tags`(如 v1.2.3 或 v1.2.3-5-gabc1234)。
	Version = "dev"

	// Commit 是构建所基于的 git 短提交号(如 abc1234)。
	Commit = "unknown"

	// BuildTime 是构建时间(UTC, RFC3339,如 2026-06-21T10:00:00Z)。
	BuildTime = "unknown"
)

// Info 是结构化的版本信息(便于打日志 / 暴露给 /version 接口)。
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

// Get 返回当前进程的版本信息。
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
	}
}

// String 返回单行可读版本串,适合启动日志。
//
//	version=v1.2.3 commit=abc1234 built=2026-06-21T10:00:00Z go=go1.26.5
func String() string {
	i := Get()
	return fmt.Sprintf("version=%s commit=%s built=%s go=%s",
		i.Version, i.Commit, i.BuildTime, i.GoVersion)
}
