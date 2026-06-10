package session

// 文件说明：生命周期三分类的衰老死亡结算（分区大世界阶段4 §6）——世界新陈代谢的「逝去」侧。
//
// 三分类（unit.LifecycleClass，落库于 Identity）：
//   - protagonist（玩家主角）：Age 完全冻结、永不衰老死亡（穿越时代的恒定主角）。
//   - functional（固定功能性 NPC：商人/传送/任务发布/铁匠）：Age 冻结、永不死亡（世界服务恒定可用）。
//   - mortal（普通 NPC：路人/村民/野外散人/子嗣 + 旧档/空 class 的保守归类）：Age 随部署边界增长、高龄按确定性掷骰自然死亡。
//
// 主角**绝对不死**双保险（硬约束）：① LifecycleClass==protagonist（faction_spawn/mainworld 落库时标）；
//   ② 在 state.PlayerUnitIDs 里（即便旧档主角空 class 也兜住）。两条任一命中即跳过死亡判定，绝不夺命。
//
// 触发点：settleExecutionToDeploymentBoundary（Execution→Deployment 回合边界）末尾调 settleAgingBestEffort，
//   与怀孕/信鸽/饥饿/记忆衰减同处批结算。生产恒走异步，该方法同步/异步两路共用（F4 H2）。
//
// flag 渐进（QUNXIANG_AGING 默认关）：关时 settleAgingBestEffort 直接 no-op、零行为——避免默认就开始死人改变现有体验；
//   运行时可经 featureflags 覆盖开启，开后才有衰老→年龄增长→自然死亡的新陈代谢。
//
// 死亡落地：命中→unit.ApplyNaturalDeath（lives 白名单路径，受保护字段合规）→ Save → best-effort 死讯路由（WorldizeDeath）
//   + 血脉传承（inheritLegacyItems）+ 入世界编年史（chronicleHeroDied）+ 单位级编年史 death 记一笔。
//   全 best-effort：单点失败绝不阻断其余单位与回合推进；最外层 panic 兜底，绝不阻断回合边界。
//
// 确定性随机：用 FNV(sessionID+turn+unitID) 取 [0,1) 掷骰，禁全局 rand（与 combat_roll 同口径，可复现）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strings"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
)

// 衰老节奏与自然死亡参数（确定性、纯代码）。
const (
	// agingNaturalDeathMinAge 是开始有自然死亡风险的年龄下限——低于此恒不老死（年轻人不会无端暴毙）。
	agingNaturalDeathMinAge = 65
	// agingAgeHardCap 是年龄硬上限——到此（仍未掷中）则强制老死，避免极端长寿挂着不走。
	agingAgeHardCap = 110
	// agingDeathProbAt65 / agingDeathProbPerYear：自然死亡概率随龄线性递增的基线与斜率——
	// p(age) = base + (age-min)*perYear，夹到 [0, agingDeathProbCap]。65 岁 ≈2%、每长一岁 +1.2%、85 岁 ≈26%。
	agingDeathProbAt65    = 0.02
	agingDeathProbPerYear = 0.012
	// agingDeathProbCap 是单次掷骰死亡概率上限（即便极高龄，单边界也不必然死，留叙事缓冲）。
	agingDeathProbCap = 0.45
)

// agingEnabled 判定衰老死亡是否开启（QUNXIANG_AGING，默认关）。
// 关时 settleAgingBestEffort 直接 no-op，NPC 年龄静止、永不老死；开后才在回合边界做 Age++ + 自然死亡掷骰。
func agingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_AGING"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// agingDeathRoll 据 (sessionID, turn, unitID) 取确定性 [0,1) 掷骰（FNV-32a，禁全局 rand，同输入同序、可复现）。
func agingDeathRoll(sessionID string, turn int, unitID string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte("aging_death"))
	_, _ = hasher.Write([]byte(sessionID))
	_, _ = hasher.Write([]byte(unitID))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", turn)))
	return float64(hasher.Sum32()%10000) / 10000
}

// naturalDeathProbability 算某年龄的单边界自然死亡概率（纯函数、确定性、可测）：
// 低于 min 恒 0；到硬上限恒 1（强制老死）；之间随龄线性递增并夹到 [0, cap]。
func naturalDeathProbability(age int) float64 {
	if age < agingNaturalDeathMinAge {
		return 0
	}
	if age >= agingAgeHardCap {
		return 1.0 // 硬上限：强制老死，避免极端长寿挂着不走
	}
	p := agingDeathProbAt65 + float64(age-agingNaturalDeathMinAge)*agingDeathProbPerYear
	if p < 0 {
		p = 0
	}
	if p > agingDeathProbCap {
		p = agingDeathProbCap
	}
	return p
}

// removeUnitID 返回剔除指定 id 后的新切片（保序、确定性，不改入参底层数组——返回新切片赋回 state 字段即可）。
// 用于死亡链把逝者从 state.WildUnitIDs/AmbientUnitIDs 摘除（避命运地图留「幽灵」token）。id 不在则原样返回（长度不变）。
func removeUnitID(ids []string, id string) []string {
	if len(ids) == 0 || id == "" {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, current := range ids {
		if current != id {
			out = append(out, current)
		}
	}
	return out
}

// isProtagonistUnit 判定某单位是否为「绝对不死」的玩家主角（双保险）：LifecycleClass==protagonist **或** 在 PlayerUnitIDs。
// 任一命中即返 true——即便旧档主角空 class，PlayerUnitIDs 仍兜住，绝不夺命（硬约束）。
func isProtagonistUnit(state *State, rec *unit.Record) bool {
	if rec == nil {
		return false
	}
	if rec.Identity.LifecycleClass == unit.LifecycleProtagonist {
		return true
	}
	if state != nil {
		for _, id := range state.PlayerUnitIDs {
			if id == rec.ID {
				return true
			}
		}
	}
	return false
}

// settleAgingBestEffort 在 Execution→Deployment 回合边界对每个单位做衰老结算（分区大世界阶段4 §6）。
//   - protagonist / functional：Age 完全冻结、永不死亡（显式跳过——年龄非受保护字段，跳过即不增不死）。
//   - mortal（含旧档/空 class 的保守归类）：Age++；高龄按确定性掷骰自然死亡，命中走死亡全链（含双保险护主角）。
//
// flag 默认关（QUNXIANG_AGING）→ 直接 no-op、零行为。全程 best-effort + 最外层 panic 兜底：绝不阻断回合边界。
// 注意：只对活着的单位结算（已 dead 的跳过）；units 是边界批结算持有的单位副本切片，Age++ 后随 units.Save 落库。
func (service *Service) settleAgingBestEffort(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || state == nil || len(units) == 0 {
		return
	}
	if !agingEnabled() {
		return // flag 默认关：零行为（避免默认就开始死人改变现有体验）
	}
	// 最外层 panic 兜底：衰老结算绝不阻断回合边界（与其余边界钩子同 best-effort 纪律）。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("settleAgingBestEffort panic recovered (session=%s): %v", state.ID, r)
		}
	}()

	turn := state.TurnState.Turn
	for index := range units {
		record := &units[index]
		if record.ID == "" || record.Status.LifeState == unit.LifeStateDead {
			continue
		}
		// 主角绝对不死（双保险）+ functional 永不死：Age 冻结、跳过死亡判定。
		if isProtagonistUnit(state, record) || record.Identity.LifecycleClass == unit.LifecycleFunctional {
			continue
		}
		// 此处为 mortal（含旧档/空 class 的保守归类，LifecycleClass.IsMortal()==true）：Age++（年龄非受保护字段，直改合规）。
		record.Identity.Age++
		age := record.Identity.Age
		if age < agingNaturalDeathMinAge {
			// 未到高龄：只增龄、不掷死亡。落库年龄增长（best-effort）。
			if err := service.units.Save(ctx, *record); err != nil {
				log.Printf("settleAgingBestEffort: save age++ unit=%s: %v", record.ID, err)
			}
			continue
		}
		// 高龄：确定性掷骰自然死亡（概率随龄递增）。
		if agingDeathRoll(state.ID, turn, record.ID) >= naturalDeathProbability(age) {
			// 未命中：增龄落库，继续活着。
			if err := service.units.Save(ctx, *record); err != nil {
				log.Printf("settleAgingBestEffort: save aged unit=%s: %v", record.ID, err)
			}
			continue
		}
		// 命中自然死亡：走死亡全链（best-effort，单点失败不阻断其余单位）。
		service.applyNaturalDeathBestEffort(ctx, state, record)
	}
}

// applyNaturalDeathBestEffort 把一个 mortal NPC 的自然老死落地并触发死亡全链（§6）：
//
//	① unit.ApplyNaturalDeath（lives 白名单路径）→ Save 落库永久死亡（受保护字段合规）。
//	② 血脉传承：inheritLegacyItems 把传家遗物转给在乎死者的继承人（best-effort，无继承人则归档）。
//	③ 死讯路由：WorldizeDeath 把死讯按相关性路由进「在乎她的人」的命运收件箱。
//	④ 入世界编年史：chronicleHeroDied 记一笔「寿终正寝」（仅 WorldID 非空时；旧单图档 no-op）。
//	⑤ 单位级编年史：给逝者记一笔 death（her own 传记锚点）。
//	⑥ 命运 feed：appendLog 让世界舞台看见「老者逝去」。
//
// 全 best-effort：每步各自吞错；②~⑥ 即便失败，①已落库的死亡仍成立（新陈代谢的逝去侧已完成）。
func (service *Service) applyNaturalDeathBestEffort(ctx context.Context, state *State, record *unit.Record) {
	if service == nil || state == nil || record == nil {
		return
	}
	deceasedName := record.DisplayName()
	// ① 永久死亡（lives 白名单路径，受保护字段合规）→ Save 落库。
	if err := unit.ApplyNaturalDeath(record); err != nil {
		return // 已 dead 或 nil：幂等返回，不重复处理
	}
	if service.units != nil {
		if err := service.units.Save(ctx, *record); err != nil {
			log.Printf("applyNaturalDeathBestEffort: save dead unit=%s: %v", record.ID, err)
			return // 落库失败：死亡未成立，不触发后续传承/路由（避免「半死」状态扇出）
		}
	}
	// ①.5 把逝者从世界众生名册里摘除（命运地图舞台数据干净，不留「幽灵」token）：mortal NPC 老死后其 id 仍挂在
	// state.WildUnitIDs（romance 野阵营子嗣）/state.AmbientUnitIDs（村民/路人，§6.1 修复后会老死），而快照装配只按 ZoneID
	// 过滤、不滤 LifeState==Dead，于是死者会持续上图当「世间众生」。死亡链摘 id 是数据正源（前端再加 active 过滤兜底）。
	// 与 romance 招募野人 removeID 同口径；摘除随边界后的 sessions.Save 持久化（state 是指针，同步/异步两路都保存）。
	state.WildUnitIDs = removeUnitID(state.WildUnitIDs, record.ID)
	state.AmbientUnitIDs = removeUnitID(state.AmbientUnitIDs, record.ID)
	// ② 血脉传承（best-effort，绝不影响死亡成立）。
	_, _ = service.inheritLegacyItems(ctx, state, *record)
	// ③ 死讯路由进「在乎她的人」收件箱（best-effort）。死因短语作归因句。
	_, _ = service.WorldizeDeath(ctx, state.ID, *record, deceasedName+"寿数已尽，安然离世。")
	// ④ 入世界编年史（仅 WorldID 非空时；内部 gate）。
	service.chronicleHeroDied(ctx, strings.TrimSpace(state.WorldID), state.TurnState.Turn, record.ID, deceasedName, "寿终正寝")
	// ⑤ 单位级编年史（逝者传记锚点，best-effort）。
	_ = service.recordChronicleEntry(ctx, state.ID, record.ID, state.TurnState.Turn, "death",
		fmt.Sprintf("我活到了 %d 岁，在第 %d 回合的某个寻常日子里，安静地走完了一生。", record.Identity.Age, state.TurnState.Turn))
	// ⑥ 命运 feed：世界舞台看见老者逝去（世界新陈代谢的可见叙事）。
	appendLog(state, "aging", fmt.Sprintf("%s寿终正寝，享年 %d 岁。一个时代的微小角落，悄然翻篇。", deceasedName, record.Identity.Age), record.ID, "")
}
