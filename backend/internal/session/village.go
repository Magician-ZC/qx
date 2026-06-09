package session

// 文件说明：把 villageseed 生成的「出生即 20 人关系网」持久化进本局（设计宪法 §4.5）。
// 复用既有底座：unit.BootstrapRecord 建人、units.Save 落库、world.Join 入世界、applyRelationShift 织关系。
// 出身/种子记忆/人生目标写进 Biography（决策 prompt 会读到）；秘密与目标作为锚由 M2.3 的 relevance_anchors 接手。
// 出生仇怨/宿敌另写入可查询的结构化记忆（rememberBirthBond/rememberBirthSelf 调既有 rememberUnitWithSource API），
// 让「出生即结仇」进入记忆检索/衰减/闪回链路，而不只停在关系行与锚里。
// 主战局**默认**为玩家身边织这张 20 人关系网（mainVillageEnabled 默认开，QUNXIANG_MAIN_VILLAGE=false 才关），
// 让命运层一开局就有锚可点；seedVillageForSession 自带幂等守卫（sessionAlreadyHasVillage），重连/重复建局不重复造人。

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strings"

	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/villageseed"
	"qunxiang/backend/internal/world"
)

// villageBirthTurn 是出生关系网记忆的回合标签：村庄在建局即成形，记忆挂在第 1 回合（最早的人生底色）。
const villageBirthTurn = 1

// SeededVillager 是一名已落库的村民，连同其原始种子档案（供上层接锚/叙事）。
type SeededVillager struct {
	UnitID  string
	Member  villageseed.Member
	WorldID string
}

// SeedVillage 确定性地为某局生成并落库 20 人出生关系网，全部接入 worldID（worldID 可空=不入世界）。
// 返回每名村民及其原始档案。同一 (worldID, seed) 重复调用，人是确定性一致的（但会新建行——调用方应只在建局时调一次）。
func (service *Service) SeedVillage(ctx context.Context, sessionID string, factionID string, worldID string, seed int64) ([]SeededVillager, error) {
	if service == nil || service.units == nil || service.db == nil {
		return nil, fmt.Errorf("seed village: missing dependencies")
	}
	v := villageseed.Generate(worldID, seed)

	records := make([]unit.Record, len(v.Members))
	out := make([]SeededVillager, 0, len(v.Members))
	for i, m := range v.Members {
		rec := unit.BootstrapRecord(seed+int64(i)*1009, sessionID, factionID, m.Name)
		rec.Personality = unit.Personality{
			Courage:     m.Traits.Courage,
			Loyalty:     m.Traits.Loyalty,
			Aggression:  m.Traits.Aggression,
			Prudence:    m.Traits.Prudence,
			Sociability: m.Traits.Sociability,
			Integrity:   m.Traits.Integrity,
			Stability:   m.Traits.Stability,
			Ambition:    m.Traits.Ambition,
		}
		rec.Identity.Gender = m.Gender
		rec.Identity.Age = m.Age
		rec.Identity.Lineage = m.Archetype
		rec.Identity.Biography = fmt.Sprintf("%s出身。%s 心里一直惦记着一件事：%s。", m.Archetype, m.SeedMemory, m.LifeGoal)
		// 按出身原型/人生目标精化六维野心（覆盖 BootstrapRecord 的 wanderer 基线，让村民野心有出身倾向）。
		rec.Ambition = unit.DeriveAmbition(seed+int64(i)*1009, m.Archetype, m.LifeGoal)
		if err := service.units.Save(ctx, rec); err != nil {
			return out, fmt.Errorf("save villager %d: %w", i, err)
		}
		if worldID != "" {
			_ = world.Join(ctx, service.db, worldID, rec.ID, "inhabitant", dbdialect.For(service.db))
		}
		records[i] = rec
		out = append(out, SeededVillager{UnitID: rec.ID, Member: m, WorldID: worldID})
	}

	// 织出生关系网（四轴按种子的关系类型一次性落到位），并把强关系沉淀为持久「债仇爱」锚。
	for _, b := range v.Bonds {
		if b.From < 0 || b.From >= len(records) || b.To < 0 || b.To >= len(records) {
			continue
		}
		src := records[b.From]
		tgt := records[b.To]
		if _, err := service.applyRelationShift(ctx, nil, &src, &tgt, relationDelta{
			Trust: b.Trust, Fear: b.Fear, Affection: b.Affection, Rivalry: b.Rivalry,
		}, "出生关系网·"+b.Kind); err != nil {
			return out, fmt.Errorf("seed bond %d->%d: %w", b.From, b.To, err)
		}
		// 强烈的爱/仇沉淀为持久 debt_grudge_love 锚（即使关系行后续变动，这份「在乎」也留痕）。
		intensity := absFloat(b.Affection) + absFloat(b.Rivalry) + absFloat(b.Fear)
		if intensity >= 8 {
			_ = service.UpsertAnchor(ctx, src.ID, relevance.DebtGrudgeLove, tgt.ID, clampFloat(intensity/24.0, 0, 1), b.Kind+"："+tgt.Identity.Name, relationAnchorHalfLife)
		}
		// 把这份出生恩怨写成可检索/可衰减/可闪回的真实记忆：源村民「记得」对方与这段关系。
		// 仇怨（宿敌/猜忌/债主）情绪权重更高、重要度加成更大，让它能被记忆链路召回并触发闪回。
		// best-effort：记忆写入失败绝不打断 20 人关系网生成（仇怨记忆是关系网之上的增量层）。
		service.rememberBirthBond(ctx, &records[b.From], worldID, seed, b, tgt.Identity.Name)
	}

	// 每人的人生目标沉淀为持久 goal 锚（M3/事件标注目标后即可命中；现在先建好）。
	// 同时把「定调的种子记忆」写成真实记忆——这是每名村民最早的人生底色，可被检索/衰减/闪回。
	for i, m := range v.Members {
		_ = service.UpsertAnchor(ctx, records[i].ID, relevance.Goal, "goal:"+records[i].ID, clampFloat(0.5+m.Traits.Ambition*0.5, 0, 1), m.LifeGoal, 0)
		service.rememberBirthSelf(ctx, &records[i], m)
	}
	return out, nil
}

// birthBondMemorySource 按关系类型把出生恩怨归到记忆来源标签：
// 仇怨类（宿敌/猜忌/债主）走 village_birth_feud，便于上层按来源筛「与谁结仇」；温情类走 village_birth_bond。
func birthBondMemorySource(kind string) (source string, isFeud bool) {
	switch kind {
	case "宿敌", "猜忌", "债主":
		return "village_birth_feud", true
	default:
		return "village_birth_bond", false
	}
}

// birthBondMemoryText 确定性地把一条出生关系渲染成一句带对方名字与恩怨语义的记忆文本。
// 文本含「仇/恨/信任/背叛」等关键词，使下游 inferMemoryCategory/inferMemoryEmotionWeight 能正确归类、加权，
// 从而让出生仇怨真正进入记忆检索/衰减/闪回链路（而不是只在关系行与锚里）。
// 确定性：同一 (worldID, seed, bond) 永远渲染同一句话（用项目约定的 FNV-64a 哈希挑措辞变体，不用全局随机）。
func birthBondMemoryText(worldID string, seed int64, b villageseed.Bond, targetName string) string {
	salt := fmt.Sprintf("birthbond|%s|%d|%d->%d|%s", worldID, seed, b.From, b.To, b.Kind)
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	sum := h.Sum64()
	pick := func(variants []string) string { return variants[sum%uint64(len(variants))] }

	switch b.Kind {
	case "宿敌":
		return pick([]string{
			fmt.Sprintf("我和%s是宿敌，这份仇我一辈子也忘不了。", targetName),
			fmt.Sprintf("一提起%s我就来气——我们之间的仇，迟早要了断。", targetName),
			fmt.Sprintf("%s是我的死对头，我恨不得避开她，又咽不下这口气。", targetName),
		})
	case "猜忌":
		return pick([]string{
			fmt.Sprintf("我一直信不过%s，她让我心里发怵，得提防着点。", targetName),
			fmt.Sprintf("%s和我之间总隔着层猜忌，我怕她哪天会背叛我。", targetName),
			fmt.Sprintf("我对%s始终留着戒心，谁知道她安的什么心。", targetName),
		})
	case "债主":
		return pick([]string{
			fmt.Sprintf("我欠了%s一笔债，每次见她都抬不起头，又怕又愧。", targetName),
			fmt.Sprintf("%s是我的债主，她攥着我的把柄，我不敢得罪。", targetName),
			fmt.Sprintf("我还欠着%s的人情债，这事一直压在我心头。", targetName),
		})
	case "青梅竹马":
		return fmt.Sprintf("%s是我的青梅竹马，我们约好长大谁也不许先走，我打心底信任她。", targetName)
	case "血亲手足":
		return fmt.Sprintf("%s是我的血亲手足，打断骨头连着筋，我护她胜过护自己。", targetName)
	case "生死之交":
		return fmt.Sprintf("我和%s是生死之交，是能把后背交给对方的人。", targetName)
	case "恩师":
		return fmt.Sprintf("%s是教过我本事的恩师，这份师恩我得记着。", targetName)
	case "暗恋":
		return fmt.Sprintf("我偷偷喜欢着%s，这份心事一直没敢说出口。", targetName)
	default:
		return fmt.Sprintf("我和%s之间有一段说不清的%s。", targetName, b.Kind)
	}
}

// rememberBirthBond 把一条出生关系写成源村民的真实结构化记忆（best-effort，失败不打断建局）。
// 仇怨类记忆给 +2 重要度加成（更难衰减、更易闪回），温情类给 +1；都带对方名字以便记忆链路检索关联。
func (service *Service) rememberBirthBond(ctx context.Context, src *unit.Record, worldID string, seed int64, b villageseed.Bond, targetName string) {
	if service == nil || src == nil || strings.TrimSpace(targetName) == "" {
		return
	}
	source, isFeud := birthBondMemorySource(b.Kind)
	boost := 1
	if isFeud {
		boost = 2
	}
	summary := birthBondMemoryText(worldID, seed, b, targetName)
	service.rememberUnitWithSourceBestEffort(ctx, src, villageBirthTurn, summary, source, boost)
}

// rememberBirthSelf 把村民「定调的种子记忆」写成真实记忆——最早的人生底色，可被检索/衰减/闪回。
// best-effort：失败不打断建局。来源 village_birth_self，给 +1 重要度加成。
func (service *Service) rememberBirthSelf(ctx context.Context, rec *unit.Record, m villageseed.Member) {
	if service == nil || rec == nil {
		return
	}
	summary := strings.TrimSpace(m.SeedMemory)
	if summary == "" {
		return
	}
	service.rememberUnitWithSourceBestEffort(ctx, rec, villageBirthTurn, summary, "village_birth_self", 1)
}

// SeedVillageBestEffort 是 onboarding 用的吞错包装：调 SeedVillage 生成 20 人关系网，
// 失败只记日志、返回已落库人数，绝不让上层（/api/units/bootstrap）失败——村庄是附加体验。
// 返回值是「实际落库的村民数」（即便中途出错，前面已 Save 的人也算数）。
func (service *Service) SeedVillageBestEffort(ctx context.Context, sessionID string, factionID string, worldID string, seed int64) int {
	villagers, err := service.SeedVillage(ctx, sessionID, factionID, worldID, seed)
	if err != nil {
		log.Printf("seed village best-effort failed (session=%s faction=%s): %v; persisted %d", sessionID, factionID, err, len(villagers))
	}
	return len(villagers)
}

// mainVillageEnabled 读 QUNXIANG_MAIN_VILLAGE，**默认开**（未设/非法 → true，主战局兑现命运开盒
// 「她身边已有二十个有名有姓的人」承诺）。仅当显式置 false/0/no/off 才关——保留紧急回退开关，
// 用于线上出问题时一键回到「主局不织村」的旧行为（无需回滚代码）。
// 采用项目约定的「默认开但可显式关」反向 envBool 语义。与 onboarding 的 with_village 查询参数互不影响；
// 重复建局/重连不会重复造人由 seedVillageForSession 内的幂等守卫保证。
func mainVillageEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_MAIN_VILLAGE"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// seedVillageForSession 把「主战局是否在玩家身边织 20 人出生关系网」的策略集中于一处：建局/组队的两条主局路径
// （createSinglePlayerWithMapScript 非 draft 路径与 ApplyOpeningDraft）各调一行即可，避免漏播/口径漂移。
// 仿 seedAmbientForNewUnit 的集中化 idiom：单人局只为玩家阵营织本局关系网（worldID 传空=不入世界，安全）。
//
// 纪律与不变量：
//  1. flag-gated（默认开）：mainVillageEnabled 默认开，主局默认织村；仅 QUNXIANG_MAIN_VILLAGE=false/0/no/off
//     时整方法 no-op、零行为变化、零 DB 写——作紧急回退开关，回到「主局不织村」旧行为而无需回滚代码。
//  2. best-effort：复用 SeedVillageBestEffort 的吞错包装，任何失败只记日志，**绝不**中断或影响建局/组队。
//  3. 确定性：seed 由建局 RandomSeed 派生（state.RandomSeed+1，与 onboarding /api/units/bootstrap?with_village=1 同口径），
//     避免与玩家主单位撞种子；同一局重复调用是确定性一致的，但会新建行，故调用方须只在建局/组队各调一次。
//  4. 幂等：起始处用 sessionAlreadyHasVillage 守卫，已织过村则直接返回，绝不重复造人（确定性、零额外 LLM，仅一次 ListBySession）。
//     当前两个调用点均在建局/组队时各调一次（reconnect.go 不调用本方法）；守卫是防御性的，使本方法即便日后接进中局/重连路径也不会重复造人。
//
// 注意：调用点应放在「玩家单位刚落库、紧邻 seedAmbientForUnits」处，让村民与玩家在同一建局事务边界内成形。
func (service *Service) seedVillageForSession(ctx context.Context, state *State) {
	if service == nil || state == nil || !mainVillageEnabled() {
		return
	}
	// 幂等守卫：若本局已织过村（已有村民单位），直接返回——避免重复建局/未来接入中局路径时重复造人。
	// 必须排除玩家主角单位：大世界入口的主角带「出身」会把 Identity.Lineage 覆写为出身原型（如「边境猎户」），
	// 与村民的 Lineage 指纹无法区分；若不排除，带出身的主角会被误判为「本局已织村」致整张 20 人关系网被跳过
	// （H1：出身致 20 人村庄被整体跳过）。传 state.PlayerUnitIDs 让守卫精确剔除玩家单位后再做村民指纹判定。
	if service.sessionAlreadyHasVillage(ctx, state.ID, state.PlayerUnitIDs) {
		return
	}
	// worldID 传空：单人局当前无世界，只织本局关系网（不强行 world.Create）。
	_ = service.SeedVillageBestEffort(ctx, state.ID, state.PlayerFactionID, "", state.RandomSeed+1)
}

// isSeededVillagerRecord 判定一条单位记录是否为本局 SeedVillage 落库的「村民」。
// 用两个 seed 无关、SeedVillage 必然写入的指纹联合判定（任一命中即算村民），不依赖具体 seed/原型分布：
//   - Identity.Gender 为中文「男」/「女」：SeedVillage 把 villageseed.Member.Gender（"男"/"女"）直接写进
//     Identity.Gender，而 unit.BootstrapRecord 造的玩家主单位/普通单位、以及生育落库的孩子 Gender 恒为英文 "male"/"female"。
//     这是可靠主指纹（村民必中、非村民必不中）。
//   - Identity.Lineage 为出身原型（非空、非 "wanderer"、非 "child"）：SeedVillage 把 Lineage 置为出身原型（如「边境猎户」），
//     bootstrap 默认 Lineage 恒为 "wanderer"、生育孩子 Lineage 恒为 "child"（见 romance.go createChildUnit），故二者须显式排除，
//     否则「只生过孩子、从未织村」的局会被误判为已织村而永久跳过织村（潜伏 bug）。
//
// 重要：本指纹**无法区分大世界入口带「出身」的玩家主角**——applyMainWorldPersona 会把主角 Lineage 覆写为出身原型
// （如「边境猎户」），命中上面第二条 Lineage 指纹。故调用方（sessionAlreadyHasVillage）**必须先剔除玩家单位**
// 再做本判定，否则带出身的主角会被误判为村民、致整张 20 人关系网被跳过（H1）。本函数只管「像不像村民指纹」，
// 「是不是玩家」由调用方负责排除。
func isSeededVillagerRecord(rec *unit.Record) bool {
	if rec == nil {
		return false
	}
	switch rec.Identity.Gender {
	case "男", "女":
		return true
	}
	lineage := strings.TrimSpace(rec.Identity.Lineage)
	return lineage != "" && lineage != "wanderer" && lineage != "child"
}

// sessionAlreadyHasVillage 判定本局是否已织过 20 人出生关系网：本局是否已存在「村民」单位
// （isSeededVillagerRecord 指纹命中；玩家主单位先被 playerUnitIDs 剔除，不会误判）。
// playerUnitIDs 传本局玩家主角单位 ID 集合（state.PlayerUnitIDs）——大世界入口带「出身」的主角 Lineage
// 被覆写为出身原型，会命中村民 Lineage 指纹，故必须先排除玩家单位再判，否则带出身的主角会被误判为「本局已织村」
// 致整张 20 人关系网被跳过（H1：出身致 20 人村庄被整体跳过）。
// 确定性、零额外 LLM：仅一次 ListBySession + 内存比对。读 DB 失败时**保守返回 false**（宁可后续 best-effort
// 重试播种、也不因瞬时读错而永久跳过织村——SeedVillageBestEffort 本身吞错，最坏只是一次无害的多写尝试）。
func (service *Service) sessionAlreadyHasVillage(ctx context.Context, sessionID string, playerUnitIDs []string) bool {
	if service == nil || service.units == nil {
		return false
	}
	records, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		return false
	}
	playerSet := make(map[string]struct{}, len(playerUnitIDs))
	for _, id := range playerUnitIDs {
		playerSet[id] = struct{}{}
	}
	for i := range records {
		// 先剔除玩家主角单位：带出身的主角 Lineage 命中村民指纹，不排除会致误判（H1）。
		if _, isPlayer := playerSet[records[i].ID]; isPlayer {
			continue
		}
		if isSeededVillagerRecord(&records[i]) {
			return true
		}
	}
	return false
}
