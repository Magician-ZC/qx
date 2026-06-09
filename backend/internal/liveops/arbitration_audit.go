// 文件说明：零和监控审计（反 P2W 的**观测闸**；设计 docs/产品方案PRD.md §8 + 工程不变量「反 P2W 红线」）。
// 扫某世界某回合区间里的仲裁/争夺结局事件（CROSS_CONTEST_WIN/LOSE、ECONOMY_LOOT_ARBITRATED），
// 按 actor 的**付费态**分组，算每组胜率。若付费组胜率 > 60% 即判 IssueDetected——这意味着付费可能在不公平地赢，
// 需要人工复核 Score 来源是否被污染。
//
// 重要状态（观测闸，非「已生效红线」）：本审计依赖**生产者**在排他仲裁/分赃处用 EmitProcessEvent 发
// ReasonCrossContestWin(胜者)/ReasonCrossContestLose(败者)/ReasonEconomyLootArbitrated，**且带 Scope.WorldID+Scope.Tick**——
// 唯有这些结局事件带 world_id+tick 落库，本审计的区间查询才能命中真实行。生产者落 Scope 之前，本审计**查不到任何行、恒空转**：
// 此时绝不能把空结果呈现为「已验证无 P2W」——那是危险的假信心。故 queryOutcomes 空 rows / 样本不足 / 付费分组不可比 时，
// 一律置 Inconclusive=true 并在 Note 写明「审计未接通，结论不可作为安全判据」。
//
// 关键澄清（不变量）：付费态来自 billing，但**绝不进** arbitration.Resolve 的 Score。本审计是**事后观测**——
// 看付费用户有没有赢得太多（机制上不该），不是让付费赢。PaidResolver 仅用于事后分组观测、绝不进 Score。审计纯只读，无任何写、无任何对局影响。
//
// 为什么 60%：随机零和下两组期望胜率各 ~50%；付费组系统性 >60% 是「Score 被付费污染」的强信号。
// 注意小样本噪声：审计同时返回样本量，红线只在样本足够（默认 ≥ minSampleForRedline）时才置 IssueDetected。

package liveops

import (
	"context"
	"database/sql"
	"fmt"

	"qunxiang/backend/internal/engine/events"
)

// PaidWinRateRedline 是付费组胜率红线阈值：超过即判 P2W 嫌疑。
const PaidWinRateRedline = 0.60

// minSampleForRedline 是触发红线所需的最小付费组样本量（避免小样本噪声误报）。
const minSampleForRedline = 20

// winReasonCodes 是「在零和争夺中胜出」的 reason code 集合（events.reason_code）。
// 这些是 arbitration.Resolve 仲裁后留痕的胜方事件——审计只看这些与对应的败方事件来算胜率。
var winReasonCodes = []string{
	string(events.ReasonCrossContestWin),
	string(events.ReasonEconomyLootArbitrated),
}

// loseReasonCodes 是「在零和争夺中落败」的 reason code 集合（events.reason_code）。
var loseReasonCodes = []string{
	string(events.ReasonCrossContestLose),
}

// GroupStat 是一个分组（付费/非付费）的胜负统计。
type GroupStat struct {
	Wins    int     `json:"wins"`
	Losses  int     `json:"losses"`
	Total   int     `json:"total"`
	WinRate float64 `json:"win_rate"` // wins/total；total=0 → 0
}

// ArbitrationAuditReport 是一次零和审计的结果。
type ArbitrationAuditReport struct {
	WorldID          string    `json:"world_id"`
	TurnStart        int       `json:"turn_start"`
	TurnEnd          int       `json:"turn_end"`
	Paid             GroupStat `json:"paid"`              // 付费组胜负
	NonPaid          GroupStat `json:"non_paid"`          // 非付费组胜负
	IssueDetected    bool      `json:"issue_detected"`    // 付费组胜率 > 红线且样本足量
	RedlineRate      float64   `json:"redline_rate"`      // 红线阈值（PaidWinRateRedline）
	SampleSufficient bool      `json:"sample_sufficient"` // 付费组样本是否达红线判定门槛
	Inconclusive     bool      `json:"inconclusive"`      // 审计未接通/不可比：结论不可作为安全判据（杜绝把假阴呈现为「已验证无 P2W」）
	Note             string    `json:"note"`              // 人读结论
}

// outcomeRow 是一条争夺结局（actor + 胜/负）。
type outcomeRow struct {
	actorID string
	won     bool
}

// AuditArbitration 扫描某世界 [turnStart, turnEnd] 区间的仲裁结局事件，按付费态分组算胜率，判红线。
// 守卫：服务未就绪 / 区间非法（start>end）→ 错误。无数据 → 空报告（各组 0，不判红线）。
func (s *LiveopsService) AuditArbitration(ctx context.Context, worldID string, turnStart, turnEnd int) (ArbitrationAuditReport, error) {
	report := ArbitrationAuditReport{
		WorldID:     worldID,
		TurnStart:   turnStart,
		TurnEnd:     turnEnd,
		RedlineRate: PaidWinRateRedline,
	}
	if !s.ready() {
		return report, fmt.Errorf("liveops audit arbitration: service not ready")
	}
	if turnStart > turnEnd {
		return report, fmt.Errorf("liveops audit arbitration: turn_start %d > turn_end %d", turnStart, turnEnd)
	}

	rows, err := s.queryOutcomes(ctx, worldID, turnStart, turnEnd)
	if err != nil {
		return report, err
	}

	// 按付费态分组累计胜负。付费态查询对每个不同 actor 只问一次（缓存），避免重复 billing 查。
	paidCache := map[string]bool{}
	for _, r := range rows {
		paid, ok := paidCache[r.actorID]
		if !ok {
			paid = s.resolvePaid(ctx, r.actorID)
			paidCache[r.actorID] = paid
		}
		group := &report.NonPaid
		if paid {
			group = &report.Paid
		}
		group.Total++
		if r.won {
			group.Wins++
		} else {
			group.Losses++
		}
	}

	finalizeGroup(&report.Paid)
	finalizeGroup(&report.NonPaid)

	report.SampleSufficient = report.Paid.Total >= minSampleForRedline

	// 「审计未接通」与「分组不可比」优先判定，避免把假阴呈现为「已验证无 P2W」。
	// ① 区间内零结局事件：生产者尚未在仲裁/分赃处带 Scope.WorldID+Scope.Tick 落库，区间查询恒空——审计未接通。
	// ② 仅单组有样本（付费组或非付费组为空）：无对照，无法判断付费有没有不公平地赢——不可比。
	switch {
	case report.Paid.Total == 0 && report.NonPaid.Total == 0:
		report.Inconclusive = true
		report.Note = "该区间未见仲裁结局事件（CROSS_CONTEST_WIN/LOSE / ECONOMY_LOOT_ARBITRATED）：" +
			"生产者尚未在排他仲裁/分赃处带 Scope.WorldID+Scope.Tick 落库，审计未接通，结论不可作为安全判据（非「已验证无 P2W」）。"
		return report, nil
	case report.Paid.Total == 0 || report.NonPaid.Total == 0:
		report.Inconclusive = true
		report.Note = fmt.Sprintf("付费分组不可比（付费 %d / 非付费 %d，仅单组有样本）：无对照无法判断付费是否不公平地赢，审计未接通，结论不可作为安全判据。",
			report.Paid.Total, report.NonPaid.Total)
		return report, nil
	}

	if report.SampleSufficient && report.Paid.WinRate > PaidWinRateRedline {
		report.IssueDetected = true
		report.Note = fmt.Sprintf("付费组胜率 %.1f%% 超过红线 %.0f%%（样本 %d）：疑似付费污染 Score，需人工复核仲裁来源。",
			report.Paid.WinRate*100, PaidWinRateRedline*100, report.Paid.Total)
	} else if !report.SampleSufficient {
		// 付费组样本不足以判红线：胜率仅供参考，且不构成「已验证无 P2W」——置 Inconclusive 以免误读为安全。
		report.Inconclusive = true
		report.Note = fmt.Sprintf("付费组样本不足（%d < %d）：胜率仅供参考，审计未达判定门槛，结论不可作为安全判据。", report.Paid.Total, minSampleForRedline)
	} else {
		report.Note = fmt.Sprintf("付费组胜率 %.1f%% 在红线 %.0f%% 内（样本 %d、有非付费对照）：本区间未见 P2W 迹象。",
			report.Paid.WinRate*100, PaidWinRateRedline*100, report.Paid.Total)
	}
	return report, nil
}

// queryOutcomes 读区间内的胜/负结局事件（events 表，按 world_id + tick 区间过滤）。
// 胜方事件（winReasonCodes）记 won=true，败方事件（loseReasonCodes）记 won=false；空 actor 跳过。
func (s *LiveopsService) queryOutcomes(ctx context.Context, worldID string, turnStart, turnEnd int) ([]outcomeRow, error) {
	codes := append(append([]string{}, winReasonCodes...), loseReasonCodes...)
	placeholders := make([]string, len(codes))
	args := make([]any, 0, len(codes)+3)
	args = append(args, worldID, turnStart, turnEnd)
	for i, c := range codes {
		placeholders[i] = "?"
		args = append(args, c)
	}
	query := fmt.Sprintf(`
		SELECT actor_unit_id, reason_code
		FROM events
		WHERE world_id = ? AND tick >= ? AND tick <= ? AND reason_code IN (%s) AND actor_unit_id IS NOT NULL
		ORDER BY tick ASC, id ASC`, joinComma(placeholders))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("liveops audit query outcomes: %w", err)
	}
	defer rows.Close()

	winSet := map[string]bool{}
	for _, c := range winReasonCodes {
		winSet[c] = true
	}
	var out []outcomeRow
	for rows.Next() {
		var actor sql.NullString
		var reason string
		if err := rows.Scan(&actor, &reason); err != nil {
			return nil, fmt.Errorf("liveops audit scan outcome: %w", err)
		}
		if !actor.Valid || actor.String == "" {
			continue
		}
		out = append(out, outcomeRow{actorID: actor.String, won: winSet[reason]})
	}
	return out, rows.Err()
}

// finalizeGroup 计算 WinRate（total=0 → 0）。
func finalizeGroup(g *GroupStat) {
	if g.Total > 0 {
		g.WinRate = float64(g.Wins) / float64(g.Total)
	}
}

// joinComma 用 ", " 连接占位符（避免 import strings 仅为一处 Join）。
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
