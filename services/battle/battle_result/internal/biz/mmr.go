// mmr.go — Elo MMR 计算(W4 ③,2026-06-06)。
//
// 不变量 §6:MMR 在 battle_result 算,DS 上报的 mmr_delta 不可信,一律被本层覆盖。
//
// 标准 Elo:
//
//	expectedA = 1 / (1 + 10^((avgB - avgA) / 400))
//	scoreA    = 胜 1 / 负 0 / 平 0.5
//	deltaA    = round(K * (scoreA - expectedA))
//	deltaB    = round(K * (scoreB - expectedB))   (K 相等时 deltaB = -deltaA)
//
// avgA / avgB 是两队当前 MMR 均值。player 服务未上线时各玩家取 BaseMMR,
// 此时 avgA=avgB → expected=0.5 → 胜 +K/2、负 -K/2(对称)。
package biz

import "math"

// winner_team 取值(对齐 proto BattleResult.winner_team)。
const (
	winnerTeamA    = 0 // A 队(team=0)胜
	winnerTeamB    = 1 // B 队(team=1)胜
	winnerTeamDraw = 2 // 平 / 无效
)

// eloDeltas 计算两队的 MMR 变化。
//
//	avgA/avgB:A/B 两队当前 MMR 均值
//	k:Elo K 系数
//	winnerTeam:0=A 胜 / 1=B 胜 / 其它=平
//
// 返回 (deltaA, deltaB):分别加到 A 队、B 队每个玩家身上。
func eloDeltas(avgA, avgB, k int, winnerTeam int32) (deltaA, deltaB int) {
	expectedA := 1.0 / (1.0 + math.Pow(10, float64(avgB-avgA)/400.0))
	expectedB := 1.0 - expectedA

	var scoreA, scoreB float64
	switch winnerTeam {
	case winnerTeamA:
		scoreA, scoreB = 1.0, 0.0
	case winnerTeamB:
		scoreA, scoreB = 0.0, 1.0
	default: // draw / invalid
		scoreA, scoreB = 0.5, 0.5
	}

	deltaA = int(math.Round(float64(k) * (scoreA - expectedA)))
	deltaB = int(math.Round(float64(k) * (scoreB - expectedB)))
	return deltaA, deltaB
}

// reasonForTeam 返回某队玩家的 player.update reason。
func reasonForTeam(team, winnerTeam int32) string {
	if winnerTeam != winnerTeamA && winnerTeam != winnerTeamB {
		return "draw"
	}
	if team == winnerTeam {
		return "win"
	}
	return "lose"
}
