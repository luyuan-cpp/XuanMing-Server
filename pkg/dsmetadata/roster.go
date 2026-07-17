// Package dsmetadata 规范化投递给 Dedicated Server 的权威 metadata。
package dsmetadata

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

const MaxBattleRosterPlayers = 128

// MaxCombatFactionID 与 DS Camp 编码边界一致：Camp 0/1 保留，玩家阵营写为 faction+2。
// 限制到 MaxInt32-2，保证 UE int32 Camp 转换永不溢出。
const MaxCombatFactionID uint32 = 1<<31 - 3

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

// CanonicalCombatFactions 严格校验 roster 到 match-local 战斗阵营的一一映射，并返回
// 按 player_id 升序的 canonical roster 与 `player=faction,...` annotation。
// factionByPlayer 必须精确覆盖 roster：缺失、额外、0 player_id 或越界均拒绝。
func CanonicalCombatFactions(playerIDs []uint64, factionByPlayer map[uint64]uint32) ([]uint64, string, error) {
	canonicalPlayers, _, err := CanonicalRoster(playerIDs)
	if err != nil {
		return nil, "", err
	}
	if len(factionByPlayer) != len(canonicalPlayers) {
		return nil, "", errors.New("combat factions must exactly cover battle roster")
	}
	parts := make([]string, len(canonicalPlayers))
	for i, playerID := range canonicalPlayers {
		factionID, ok := factionByPlayer[playerID]
		if !ok {
			return nil, "", errors.New("combat factions missing battle roster player")
		}
		if factionID > MaxCombatFactionID {
			return nil, "", errors.New("combat faction_id exceeds DS camp range")
		}
		parts[i] = strconv.FormatUint(playerID, 10) + "=" + strconv.FormatUint(uint64(factionID), 10)
	}
	return canonicalPlayers, strings.Join(parts, ","), nil
}
