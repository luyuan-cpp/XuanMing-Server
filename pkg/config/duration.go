// Package config — Duration 包装类型(W3 ⑥,2026-06-05)。
//
// 背景:
//
// Kratos config 走 yaml→map[string]interface{}→json→struct 三步反序列化,
// 中间 json.Unmarshal 阶段 time.Duration(int64)无法接受 "5s"/"24h"
// 这种字符串(JSON 期望数字),所以原生 time.Duration 字段不能在 yaml 里写时长字符串。
//
// 本类型把 time.Duration 包一层,实现 UnmarshalJSON / MarshalJSON,既能解
// 字符串 "5s",也向后兼容旧 yaml/json 写的纯数字 nanoseconds(兜底)。
//
// 使用方式(在 pkg/config 内或下游业务 conf 结构):
//
//	type RedisConf struct {
//	    DialTimeout config.Duration `yaml:"dial_timeout" json:"dial_timeout"`
//	}
//
// 业务代码取标准库类型:
//
//	rdb := redis.NewClient(&redis.Options{
//	    DialTimeout: cfg.Node.RedisClient.DialTimeout.Std(),
//	})
//
// 默认值写法:
//
//	if c.DialTimeout == 0 {
//	    c.DialTimeout = config.Duration(2 * time.Second)
//	}
package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration 是 time.Duration 的包装类型,补齐 JSON 字符串反序列化能力。
//
// 零值 Duration(0) == 0ns,跟 time.Duration 零值同义,业务侧 Defaults() 直接判断
// `if c.X == 0` 即可。
type Duration time.Duration

// Std 返回标准库 time.Duration,便于直接传给 stdlib / 第三方库 API。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String 复用 time.Duration 的可读格式("5s" / "1h30m" / "500ms")。
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON 始终输出带引号的字符串形式("5s"),便于配置回写 / 日志展示。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON 接受:
//   - 字符串:"5s" / "1h30m" / "0s" / "-2m"(time.ParseDuration 规则);
//   - 数字:5000000000 → 5 秒(向后兼容旧 yaml/json 写法,单位 ns)。
//
// 空串("")按 0 处理,便于 yaml 写空值占位;非法字符串(如 "abc"/"5"
// 这种无单位)返回错误并附 input,便于 ops 排错。
func (d *Duration) UnmarshalJSON(b []byte) error {
	// null → 保持零值
	if string(b) == "null" {
		return nil
	}
	// 试 string 路径
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s == "" {
			*d = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("config.Duration: invalid duration string %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	// 试 int64 路径(向后兼容)
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*d = Duration(n)
		return nil
	}
	return fmt.Errorf("config.Duration: expect string like \"5s\" or int64 nanoseconds, got %s", string(b))
}
