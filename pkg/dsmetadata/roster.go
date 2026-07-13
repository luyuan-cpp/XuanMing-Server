// Package dsmetadata 规范化投递给 Dedicated Server 的权威 metadata。
package dsmetadata

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

const MaxBattleRosterPlayers = 128

// CanonicalRoster 返回升序去重的 player IDs 与逗号分隔十进制 annotation。
// 空、0、超过硬上限一律拒绝；调用方不得截断 roster 后继续分配。
func CanonicalRoster(playerIDs []uint64) ([]uint64, string, error) {
	if len(playerIDs) == 0 {
		return nil, "", errors.New("battle roster must be non-empty")
	}
	ids := append([]uint64(nil), playerIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	write := 0
	for _, id := range ids {
		if id == 0 {
			return nil, "", errors.New("battle roster contains zero player_id")
		}
		if write > 0 && ids[write-1] == id {
			continue
		}
		ids[write] = id
		write++
	}
	ids = ids[:write]
	if len(ids) > MaxBattleRosterPlayers {
		return nil, "", errors.New("battle roster exceeds 128 players")
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(id, 10)
	}
	return ids, strings.Join(parts, ","), nil
}
