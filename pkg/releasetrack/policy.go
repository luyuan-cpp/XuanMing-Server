// Package releasetrack 提供 stable/canary 的确定性 cohort 选择。
// 选择结果只是分配意图；当 canary 无容量回退 stable 时，调用方必须持久化
// 编排层权威回读到的实际 track，不能把 Policy.Select 的结果当最终事实。
package releasetrack

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

const (
	Stable = "stable"
	Canary = "canary"
)

type Policy struct {
	percent uint32
	seed    string
}

func New(percent uint32, seed string) (Policy, error) {
	if percent > 100 {
		return Policy{}, fmt.Errorf("canary_percent %d out of range [0,100]", percent)
	}
	if percent > 0 && seed == "" {
		return Policy{}, errors.New("canary_seed required when canary_percent > 0")
	}
	return Policy{percent: percent, seed: seed}, nil
}

func (p Policy) Select(id uint64) string {
	if p.percent == 0 || id == 0 {
		return Stable
	}
	if p.percent == 100 {
		return Canary
	}
	sum := sha256.Sum256([]byte(p.seed + ":" + strconv.FormatUint(id, 10)))
	if binary.BigEndian.Uint64(sum[:8])%100 < uint64(p.percent) {
		return Canary
	}
	return Stable
}

func Valid(track string) bool { return track == Stable || track == Canary }
