package session

// 文件说明：人格漂移调节器（设计宪法 §4「人会变」/ 沙盘 §8.7 生命周期）。
// 因「接管 / 衰老 / 重大经历」等边界触发，让单位的八维人格（unit.Personality）在严格步长内悄然迁移——
// 体现「经历会沉淀成性情」而非一夜剧变。
//
// 三条硬约束（与设计宪法对齐，均有单测守护）：
//   1. 步长封顶：单次调用·单维变化幅度 ≤ driftPerStepCap（0.03）；单日·单维累计变化 ≤ driftPerDayCap（0.10）。
//   2. 确定性：每维的方向与幅度仅由 sessionID+turn+actor+维名+reason 的 FNV-64a 哈希派生，禁用全局 math/rand，
//      同输入必同输出、可复放（与 combat_roll 同纪律）。
//   3. 留痕不直改受保护字段：Personality 非受保护字段（不在 HP/Hunger/Morale/Loyalty/LivesRemaining/Mood 之列，
//      statuslint 不拦），但每次漂移必经标准事件留痕（reason=PERSONALITY_DRIFT，经 EmitProcessEvent 旁路，不走
//      status.Mutator——Mutator 只管受保护数值字段）。
//
// 上下限 clamp：每维恒夹在 [personalityFloor, personalityCeil]（[0.05,0.95]，与 unit.GeneratePersonality 同界）。
// best-effort：缺依赖 / 单位读不到 / 写库失败 → 优雅返回（漂移不影响主循环；调用方以 _ 忽略错误即可）。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

const (
	// driftPerStepCap 单次调用、单维度允许的最大变化幅度（绝对值）。
	driftPerStepCap = 0.03
	// driftPerDayCap 单个 UTC 自然日内、单维度允许的累计变化幅度（绝对值上限）。
	driftPerDayCap = 0.10
	// personalityFloor / personalityCeil 人格各维的硬边界（与 unit.GeneratePersonality 的 normalizedTrait 同界）。
	personalityFloor = 0.05
	personalityCeil  = 0.95
)

// PersonalityDriftReason 标识一次人格漂移的成因（喂哈希派生 + 留痕 payload，便于复盘「为什么变」）。
type PersonalityDriftReason string

// 常量定义区：登记内置漂移成因（接管 / 衰老 / 重大经历）。新增成因在此追加即可，纯字符串、不影响存档。
const (
	// DriftReasonIntervention 玩家直接接管/嘱咐了她一次——被接管者的性情会被这次干预悄然牵引。
	DriftReasonIntervention PersonalityDriftReason = "intervention"
	// DriftReasonAging 自然衰老——随在世日久，胆气/锐气缓降、沉稳渐增（与 unit.lives 的衰减同向但更细）。
	DriftReasonAging PersonalityDriftReason = "aging"
	// DriftReasonOrdeal 重大经历（战创/丧亲/夙愿得偿/背叛…）——一桩刻骨之事会沉淀成性情。
	DriftReasonOrdeal PersonalityDriftReason = "ordeal"
)

// driftDimension 描述一个可漂移的人格维度：取值器 / 写回器 / 维名（喂哈希与留痕）。
// 用取值/写回闭包而非反射，既避开 reflect 的不确定性，也让「哪些维可漂、漂多少」一目了然、可单测。
type driftDimension struct {
	Name string
	Get  func(*unit.Personality) float64
	Set  func(*unit.Personality, float64)
	// Bias ∈ [-1,1]：该成因下这一维的漂移倾向。正=偏向上调、负=偏向下调、0=中性（方向纯随机）。
	Bias func(PersonalityDriftReason) float64
}

// personalityDimensions 返回八维人格的漂移描述子（顺序稳定 → 哈希与遍历确定性）。
func personalityDimensions() []driftDimension {
	return []driftDimension{
		{Name: "courage", Get: func(p *unit.Personality) float64 { return p.Courage }, Set: func(p *unit.Personality, v float64) { p.Courage = v },
			Bias: func(r PersonalityDriftReason) float64 {
				if r == DriftReasonAging || r == DriftReasonOrdeal {
					return -0.5 // 衰老/创伤：胆气缓降
				}
				return 0
			}},
		{Name: "loyalty", Get: func(p *unit.Personality) float64 { return p.Loyalty }, Set: func(p *unit.Personality, v float64) { p.Loyalty = v },
			Bias: func(r PersonalityDriftReason) float64 {
				if r == DriftReasonIntervention {
					return 0.3 // 被你接管/嘱咐：忠诚倾向小幅被牵引（方向仍含随机，避免单调）
				}
				return 0
			}},
		{Name: "aggression", Get: func(p *unit.Personality) float64 { return p.Aggression }, Set: func(p *unit.Personality, v float64) { p.Aggression = v },
			Bias: func(r PersonalityDriftReason) float64 {
				if r == DriftReasonOrdeal {
					return 0.3 // 重大经历：锐气易被激出（也可能反向，故偏置不满）
				}
				return 0
			}},
		{Name: "prudence", Get: func(p *unit.Personality) float64 { return p.Prudence }, Set: func(p *unit.Personality, v float64) { p.Prudence = v },
			Bias: func(r PersonalityDriftReason) float64 {
				if r == DriftReasonAging {
					return 0.4 // 衰老：愈发审慎
				}
				return 0
			}},
		{Name: "sociability", Get: func(p *unit.Personality) float64 { return p.Sociability }, Set: func(p *unit.Personality, v float64) { p.Sociability = v }, Bias: func(PersonalityDriftReason) float64 { return 0 }},
		{Name: "integrity", Get: func(p *unit.Personality) float64 { return p.Integrity }, Set: func(p *unit.Personality, v float64) { p.Integrity = v }, Bias: func(PersonalityDriftReason) float64 { return 0 }},
		{Name: "stability", Get: func(p *unit.Personality) float64 { return p.Stability }, Set: func(p *unit.Personality, v float64) { p.Stability = v },
			Bias: func(r PersonalityDriftReason) float64 {
				switch r {
				case DriftReasonAging:
					return 0.4 // 衰老：愈发沉稳
				case DriftReasonOrdeal:
					return -0.4 // 创伤：心绪更易动摇
				default:
					return 0
				}
			}},
		{Name: "ambition", Get: func(p *unit.Personality) float64 { return p.Ambition }, Set: func(p *unit.Personality, v float64) { p.Ambition = v }, Bias: func(PersonalityDriftReason) float64 { return 0 }},
	}
}

// driftDelta 计算某维在本次漂移里的确定性增量，并把它夹进「步长 + 当日剩余额度」双重上限。
//
//   - magnitude：基础幅度 ∈ (0, driftPerStepCap]，由哈希派生（每维不同），保证「不同维、不同回合各有微差」。
//   - direction：方向 ∈ {-1,+1}，由哈希派生但被 bias 牵引——|bias| 越大越倾向某向，bias=0 时纯随机对半。
//   - 最终幅度再被 remainingToday（当日该维剩余可漂额度，≥0）截断，确保单日单维累计不超 driftPerDayCap。
//
// 纯函数、确定性（仅依赖入参），可单测。返回的增量绝对值恒 ≤ min(driftPerStepCap, remainingToday)。
func driftDelta(sessionID string, turn int, actorID string, reason PersonalityDriftReason, dim driftDimension, remainingToday float64) float64 {
	if remainingToday <= 0 {
		return 0 // 当日该维额度已耗尽
	}
	salt := fmt.Sprintf("%s|%d|%s|%s|%s|drift", sessionID, turn, actorID, dim.Name, reason)
	roll := driftRoll(salt)               // [0,1)
	dirRoll := driftRoll(salt + "|dir")   // [0,1) 决定方向
	magnitude := driftPerStepCap * roll   // (0, 0.03]
	bias := clampUnit(dim.Bias(reason))   // [-1,1]
	// bias 把方向阈值从 0.5 偏移：bias>0 时落在「上调」区间的概率更高。
	threshold := 0.5 - bias*0.5
	direction := 1.0
	if dirRoll < threshold {
		direction = -1.0
	}
	delta := direction * magnitude
	// 当日剩余额度截断（保符号）。
	if math.Abs(delta) > remainingToday {
		if delta < 0 {
			delta = -remainingToday
		} else {
			delta = remainingToday
		}
	}
	return delta
}

// driftRoll 从 salt 派生 [0,1) 的稳定随机值（FNV-64a，与全局 math/rand 隔离，可复放）。
func driftRoll(salt string) float64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(salt))
	return float64(hasher.Sum64()%1_000_000) / 1_000_000
}

// clampUnit 把值夹到 [-1,1]。
func clampUnit(v float64) float64 {
	if v < -1 {
		return -1
	}
	if v > 1 {
		return 1
	}
	return v
}

// clampTrait 把人格维值夹到 [personalityFloor, personalityCeil] 并保留两位小数（与初始生成同精度）。
func clampTrait(v float64) float64 {
	if v < personalityFloor {
		v = personalityFloor
	}
	if v > personalityCeil {
		v = personalityCeil
	}
	return math.Round(v*100) / 100
}

// driftAmount 是一次漂移里单维的明细（留痕 payload + 回写）。
type driftAmount struct {
	Dimension string  `json:"dimension"`
	Before    float64 `json:"before"`
	After     float64 `json:"after"`
	Delta     float64 `json:"delta"`
}

// ApplyPersonalityDrift 对一个单位施加一次人格漂移（导出供 Wire 在合适边界调用——见文件尾签名说明）。
//
// 语义：
//   - 读出单位 → 对八维各算确定性增量（受步长 0.03 / 当日累计 0.10 双上限 + [0.05,0.95] clamp 约束）→ 写回 → 留痕。
//   - 当日累计额度按「该单位今日已发生的 PERSONALITY_DRIFT 事件 payload 中各维 |delta| 之和」逐维核算
//     （UTC 自然日，字符串区间过滤 occurred_at，复用 fate.pendingBudgetExhausted 同款不依赖 SQL 日期函数的写法）。
//   - 全程 best-effort：缺依赖 / 单位读不到 / 全维无可漂额度 → 优雅返回（不报错、不写库、不影响主循环）。
//
// 返回本次实际发生的各维明细（可空切片表示无漂移）与错误（仅在写库失败时非 nil；调用方通常以 _ 忽略）。
func (service *Service) ApplyPersonalityDrift(ctx context.Context, sessionID string, unitID string, reason PersonalityDriftReason, turn int) ([]driftAmount, error) {
	if service == nil || service.db == nil || service.units == nil {
		return nil, nil
	}
	unitID = strings.TrimSpace(unitID)
	if unitID == "" {
		return nil, nil
	}
	if reason == "" {
		reason = DriftReasonOrdeal
	}
	if ctx == nil {
		ctx = context.Background()
	}

	record, err := service.units.GetByID(ctx, unitID)
	if err != nil {
		return nil, nil // 读不到（已离场/跨会话）→ best-effort no-op
	}

	usedToday := service.driftUsedToday(ctx, unitID) // 维名 → 今日已用累计 |delta|

	amounts := make([]driftAmount, 0, 8)
	for _, dim := range personalityDimensions() {
		remaining := driftPerDayCap - usedToday[dim.Name]
		if remaining <= 0 {
			continue
		}
		before := dim.Get(&record.Personality)
		delta := driftDelta(sessionID, turn, unitID, reason, dim, remaining)
		if delta == 0 {
			continue
		}
		after := clampTrait(before + delta)
		realDelta := after - before
		if realDelta == 0 {
			continue // 被 clamp 顶到边界、无实际变化
		}
		dim.Set(&record.Personality, after)
		amounts = append(amounts, driftAmount{Dimension: dim.Name, Before: before, After: after, Delta: realDelta})
	}

	if len(amounts) == 0 {
		return nil, nil // 无任何可漂动 → 不写库、不留痕
	}

	if err := service.units.Save(ctx, record); err != nil {
		return nil, fmt.Errorf("save drifted unit: %w", err)
	}

	// 标准事件留痕（PERSONALITY_DRIFT，流程事件旁路；Personality 非受保护字段，故不走 status.Mutator）。
	def, _ := events.Lookup(events.ReasonPersonalityDrift)
	payload := map[string]any{
		"reason":  string(reason),
		"changes": amounts,
		"summary": def.DefaultReasonText,
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		Code:        events.ReasonPersonalityDrift,
		Category:    events.CategoryLifecycle,
		Payload:     payload,
		Tick:        turn,
	}); err != nil {
		// 漂移已落库；留痕失败只吞错（不回滚单位写——审计缺一条胜过状态被回退）。
		return amounts, nil
	}

	// 实时推送（best-effort）：让前端「性情流转」提示带出本次变化。
	service.pushRealtime(sessionID, "personality_drift", map[string]any{
		"unit_id": unitID,
		"reason":  string(reason),
		"changes": amounts,
	})

	return amounts, nil
}

// driftUsedToday 统计某单位「今日（UTC 自然日）」已发生的人格漂移在各维上的累计 |delta|。
// 复用 fate.pendingBudgetExhausted 同款「字符串区间过滤 occurred_at」写法：不依赖任何 SQL 日期函数、跨方言安全。
// best-effort：查询/解析失败按「未用额度」处理（返回空 map → 当日额度视为满，保守放行漂移）。
func (service *Service) driftUsedToday(ctx context.Context, unitID string) map[string]float64 {
	used := make(map[string]float64, 8)
	if service == nil || service.db == nil || unitID == "" {
		return used
	}
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lo := dayStart.Format(time.RFC3339Nano)
	hi := dayStart.Add(24 * time.Hour).Format(time.RFC3339Nano)
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT payload_json FROM events
		 WHERE actor_unit_id = ? AND reason_code = ? AND occurred_at >= ? AND occurred_at < ?`,
		unitID, string(events.ReasonPersonalityDrift), lo, hi,
	)
	if err != nil {
		return used
	}
	defer rows.Close()
	for rows.Next() {
		var payloadJSON string
		if scanErr := rows.Scan(&payloadJSON); scanErr != nil {
			continue
		}
		for dim, mag := range parseDriftMagnitudes(payloadJSON) {
			used[dim] += mag
		}
	}
	return used
}

// parseDriftMagnitudes 从一条漂移事件 payload 还原各维的 |delta|（用于当日累计核算）。
// 解析失败 / 字段缺失 → 返回空 map（该条按 0 计，best-effort 不阻断）。
func parseDriftMagnitudes(payloadJSON string) map[string]float64 {
	out := make(map[string]float64)
	var p struct {
		Changes []driftAmount `json:"changes"`
	}
	if payloadJSON == "" {
		return out
	}
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return out
	}
	for _, c := range p.Changes {
		out[c.Dimension] += math.Abs(c.Delta)
	}
	return out
}
