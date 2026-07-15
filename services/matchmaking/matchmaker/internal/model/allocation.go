// Package model 放 matchmaker biz/data 共享的传输无关领域值。
package model

import "github.com/luyuancpp/pandora/pkg/placement"

// BattleAllocation 是 allocator 返回的完整、不可降级 DS 绑定。Address 只用于 Travel；
// placement/票据必须绑定 Target 的 pod/UID/epoch/allocation/release-track 全身份。
type BattleAllocation struct {
	Address string
	Target  placement.Target
}
