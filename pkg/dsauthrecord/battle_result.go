// Package dsauthrecord 定义跨 DS 服务共享、与具体 Redis 客户端解耦的授权记录。
package dsauthrecord

import (
	"encoding/json"
	"fmt"
)

const battleResultReceiptVersion uint32 = 1

// BattleResultReceipt 是 battle_result 已权威接收结算的凭据。
// 它与 auth/battle key 使用同一 {match_id} slot；ended 心跳只能消费完全匹配的 receipt。
type BattleResultReceipt struct {
	Version       uint32 `json:"version"`
	MatchID       uint64 `json:"match_id"`
	AllocationID  string `json:"allocation_id"`
	PodName       string `json:"pod_name"`
	InstanceUID   string `json:"instance_uid"`
	InstanceEpoch uint32 `json:"instance_epoch"`
	Gen           uint64 `json:"gen"`
	JTI           string `json:"jti"`
	ExpMs         int64  `json:"exp_ms"`
	Kid           string `json:"kid"`
	TokenSHA256   string `json:"token_sha256"`
	WriterEpoch   uint32 `json:"writer_epoch"`
	RecordedAtMs  int64  `json:"recorded_at_ms"`
}

// BattleResultReceiptKey 返回与 Battle auth/battle 相同 Redis Cluster slot 的 key。
func BattleResultReceiptKey(matchID uint64) string {
	return fmt.Sprintf("pandora:ds:result-receipt:{%d}", matchID)
}

func (r BattleResultReceipt) Valid(nowMs int64) bool {
	return r.Version == battleResultReceiptVersion && r.MatchID != 0 && r.AllocationID != "" &&
		r.PodName != "" && r.InstanceUID != "" && r.InstanceEpoch != 0 && r.Gen != 0 &&
		r.JTI != "" && r.Kid != "" && r.TokenSHA256 != "" && r.WriterEpoch != 0 &&
		r.RecordedAtMs > 0 && r.RecordedAtMs <= nowMs && r.ExpMs > r.RecordedAtMs
}

func (r BattleResultReceipt) SameCredential(other BattleResultReceipt) bool {
	return r.Version == other.Version && r.MatchID == other.MatchID &&
		r.AllocationID == other.AllocationID && r.PodName == other.PodName &&
		r.InstanceUID == other.InstanceUID && r.InstanceEpoch == other.InstanceEpoch &&
		r.Gen == other.Gen && r.JTI == other.JTI && r.ExpMs == other.ExpMs &&
		r.Kid == other.Kid && r.TokenSHA256 == other.TokenSHA256 &&
		r.WriterEpoch == other.WriterEpoch
}

func MarshalBattleResultReceipt(receipt BattleResultReceipt) ([]byte, error) {
	if !receipt.Valid(receipt.RecordedAtMs) {
		return nil, fmt.Errorf("invalid battle result receipt")
	}
	return json.Marshal(receipt)
}

func UnmarshalBattleResultReceipt(payload []byte) (BattleResultReceipt, error) {
	var receipt BattleResultReceipt
	if len(payload) == 0 {
		return receipt, fmt.Errorf("empty battle result receipt")
	}
	if err := json.Unmarshal(payload, &receipt); err != nil {
		return receipt, fmt.Errorf("decode battle result receipt: %w", err)
	}
	return receipt, nil
}

// NewBattleResultReceipt 构造当前格式，避免服务各自手填版本。
func NewBattleResultReceipt(
	matchID uint64,
	allocationID, podName, instanceUID string,
	instanceEpoch uint32,
	gen uint64,
	jti string,
	expMs int64,
	kid, tokenSHA256 string,
	writerEpoch uint32,
	recordedAtMs int64,
) BattleResultReceipt {
	return BattleResultReceipt{
		Version: battleResultReceiptVersion, MatchID: matchID, AllocationID: allocationID,
		PodName: podName, InstanceUID: instanceUID, InstanceEpoch: instanceEpoch,
		Gen: gen, JTI: jti, ExpMs: expMs, Kid: kid, TokenSHA256: tokenSHA256,
		WriterEpoch: writerEpoch, RecordedAtMs: recordedAtMs,
	}
}
