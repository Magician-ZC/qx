package session

// 文件说明：M1 data-driven 翻译矩阵的「DB 表」落地（设计 事件耦合与跨玩家关联 §1.2 + §5 风险「翻译模板覆盖不全」）。
// 把原 translation.go 里硬编码的 Go map 升级为运营态可补的 translation_templates DB 表：
//
//   - builtinTranslationTemplates：内置全 (reason_code × anchor_kind) 覆盖矩阵（每个关键 reason_code × 6 锚类，
//     外加 anchor_kind='' 的「该 reason_code 通用兜底」行）。force_pending=1 的组（如密友倒地 COMBAT_DOWN×relation）
//     标记为「必须升级待决策」，供消费方据此把路由从高光卡硬抬到待决策。
//   - SeedTranslationTemplates：幂等 upsert 内置矩阵到表（启动时调，双驱动 ON CONFLICT / ON DUPLICATE KEY）。
//   - loadTranslationTemplate：按 (reason_code, anchor_kind) 三级回退查表——命中精确组优先 → 回退 anchor_kind='' 通用 →
//     再回退 events.DefaultReasonText（并计一条遥测，便于排期补缺）。带进程级内存缓存，避免每次命运路由都查库。
//
// 设计原则：确定性、best-effort（查库失败优雅回退内置矩阵/DefaultReasonText，绝不阻断命运路由）、
// 占位符替换安全（{friend}/{region}/{target}/{event} 缺省被裁剪而非残留）、反 P2W（评分/门槛绝不含 wallet/billing）。

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/storage/dbdialect"
)

// translationTemplate 是翻译矩阵的一行：把一条 (reason_code × anchor_kind) 世界事实翻成对她的命运 beat 模板。
//   - ReasonCode：底层事件 reason-code（如 COMBAT_DOWN）；AnchorKind：命中里最重的锚类（空=该 reason_code 的通用兜底）。
//   - Narrative：含占位 {friend}/{region}/{target}/{event} 的命运 beat 模板，由 renderFateTemplate 安全渲染。
//   - ForcePending：该组是否强制升级待决策（密友倒地、密友被背叛、她所在地被劫等「非看不可」的命运节点）。
//   - Priority：同 reason_code 下多锚命中时的择优序（越大越优先；当前消费方按精确锚命中取，预留运营态调权）。
type translationTemplate struct {
	ReasonCode   events.ReasonCode
	AnchorKind   relevance.AnchorKind
	Narrative    string
	ForcePending bool
	Priority     int
}

// builtinTranslationTemplates 是内置全覆盖矩阵——对关键 reason_code 建 6 锚类精确模板 + 1 条 anchor_kind=” 通用兜底。
// SeedTranslationTemplates 幂等写进表；表为权威源，但查库失败时这张内置矩阵也是 loadTranslationTemplate 的内存兜底。
//
// force_pending 类（设计 §1.2 点名）：密友倒地 COMBAT_DOWN×relation、密友被背叛 RELATION_BETRAYAL×relation、
// 角色之死 CHARACTER_DIED×relation、红线被触类（任何 reason × redline）——这些是「她在乎的人/底线」出了覆水难收的事，
// 必须停在玩家手里，不能被自治悄悄吞掉。
var builtinTranslationTemplates = func() []translationTemplate {
	anchors := []relevance.AnchorKind{
		relevance.Relation, relevance.Redline, relevance.Goal,
		relevance.DebtGrudgeLove, relevance.Geo, relevance.Legacy,
	}
	out := make([]translationTemplate, 0, 64)

	// add 追加一组：(reason_code × 6 锚类) 精确模板 + 1 条通用兜底。forcePendingAnchors 列出该 reason_code 下需强制升级的锚类。
	add := func(code events.ReasonCode, generic string, perAnchor map[relevance.AnchorKind]string, forcePendingAnchors ...relevance.AnchorKind) {
		fp := map[relevance.AnchorKind]bool{}
		for _, a := range forcePendingAnchors {
			fp[a] = true
		}
		for _, a := range anchors {
			tmpl, ok := perAnchor[a]
			if !ok {
				// 该锚未单列时回落 generic 文案，但仍登记为精确组（保证全 6 锚覆盖、不漏配）。
				tmpl = generic
			}
			out = append(out, translationTemplate{ReasonCode: code, AnchorKind: a, Narrative: tmpl, ForcePending: fp[a]})
		}
		// anchor_kind='' 通用兜底行（命中锚但该 reason_code×该锚未精确命中时用；redline 触类的 force_pending 由 redline 行携带）。
		out = append(out, translationTemplate{ReasonCode: code, AnchorKind: "", Narrative: generic})
	}

	// ===== COMBAT_DOWN（倒地濒死）：密友倒地是 §1.2 点名的 force_pending 标杆 =====
	add(events.ReasonCombatDown,
		"{friend}在战场上倒下了，生死未卜。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}倒下了，生死未卜——她在乎的人，正躺在那片战场上。",
			relevance.Redline:        "{friend}重伤倒地。她曾立誓绝不让这样的事发生，红线就在眼前血淋淋地裂开。",
			relevance.Goal:           "{friend}在关键一战里倒下，她苦心要做成的事，悬在了那口血气上。",
			relevance.DebtGrudgeLove: "{friend}倒在了血泊里——她与{friend}之间那桩未了的债仇情，眼看就要永远没了下文。",
			relevance.Geo:            "就在{region}，{friend}倒下了——她脚下这片地方，又添了一道她不愿看见的伤。",
			relevance.Legacy:         "{friend}倒下了，连同她血脉里那一脉的牵系，也被这一倒狠狠揪起。",
		},
		relevance.Relation, relevance.Redline, relevance.DebtGrudgeLove, // 密友/红线/旧情倒地→必停玩家手里
	)

	// ===== RELATION_BETRAYAL（遭遇背叛/密友被背叛）：§1.2 点名的 worry 压力位来源 =====
	add(events.ReasonRelationBetray,
		"{friend}那边出事了，有人背了信。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}那边出事了——她在乎的人被身边人背了信，刀就插在背上。",
			relevance.Redline:        "背信弃义，正是她立下死也不碰的那条红线，如今{friend}撞了上来。",
			relevance.Goal:           "一桩背叛把局搅乱：{friend}被人反咬，她想成的事跟着摇晃。",
			relevance.DebtGrudgeLove: "{friend}遭了背叛——这一刀，把她与{friend}之间的债仇情又添了一笔新账。",
			relevance.Geo:            "在{region}，{friend}被身边人反了水，人心倒向了别处。",
			relevance.Legacy:         "{friend}遭背叛，连同她血脉所系的那份信义，也被这一背扯裂。",
		},
		relevance.Relation, relevance.Redline, // 密友被背叛/触红线→必停玩家手里
	)

	// ===== ECONOMY_LOOT（战利品/她所在 region 被劫）：§1.2 点名的 goal 锚可达性 =====
	add(events.ReasonEconomyLoot,
		"一笔横财易了主——有人空了别人的家底。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}的家底被人劫了空——她在乎的人，一夜回到了赤手空拳。",
			relevance.Redline:        "巧取豪夺，触到了她绝不容许的那条线：{event}",
			relevance.Goal:           "{region}的商路被断，她攒下的本钱眼看要打水漂，想做成的事一下子远了。",
			relevance.DebtGrudgeLove: "那笔被劫的财货里，缠着她与{friend}之间一桩未清的债——如今更难了断。",
			relevance.Geo:            "{region}被人洗劫了一遍——她脚下这片地方的元气，伤在了这一劫上。",
			relevance.Legacy:         "连传家的东西都被劫掠者卷走了，她血脉里的根，被人生生拔了一截。",
		},
		relevance.Geo, // 她所在地被劫→必停玩家手里（直接威胁她的根据地/goal 可达性）
	)

	// ===== RELATION_RESCUED（被队友救援）：暖色命运 beat（不进 force_pending）=====
	add(events.ReasonRelationRescue,
		"危急关头，有人伸手把人从鬼门关拉了回来。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}被人从鬼门关拉了回来——她在乎的人，捡回了一条命。",
			relevance.Redline:        "有人替{friend}挡了那一刀，没让她最怕的事成真。",
			relevance.Goal:           "{friend}得救了，她苦撑的局，总算没在这一步上崩掉。",
			relevance.DebtGrudgeLove: "{friend}被救下一命——她与{friend}之间的债仇情，又欠下了一笔救命的人情。",
			relevance.Geo:            "在{region}，{friend}被同伴救了下来，这片地方还留着一线暖意。",
			relevance.Legacy:         "{friend}得救，她血脉里那一脉，也跟着续上了一口气。",
		},
	)

	// ===== CHARACTER_DIED（角色之死）：最重的不可逆命运，密友之死必停玩家手里 =====
	add(events.ReasonCharacterDied,
		"{friend}走了，再也回不来了。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}走了——她在乎了那么久的人，从此天人永隔。",
			relevance.Redline:        "{friend}的死撞碎了她立下的誓，那条她拼死也要守住的红线，断在了今天。",
			relevance.Goal:           "{friend}没能等到那一天，她想做成的事，从此少了最要紧的一个人。",
			relevance.DebtGrudgeLove: "{friend}带着她与{friend}之间那笔债仇情走了，再没有了断的机会。",
			relevance.Geo:            "{region}埋下了{friend}——她脚下这片地方，从此多了一座她不敢回望的坟。",
			relevance.Legacy:         "{friend}走了，把她血脉里那一脉的香火，沉沉压到了她一个人肩上。",
		},
		relevance.Relation, relevance.Redline, relevance.DebtGrudgeLove, relevance.Legacy, // 密友之死/血脉断→必停玩家手里
	)

	// ===== EMOTION_TRAUMA（创伤事件）：情绪重击，关系/红线触类需玩家知晓 =====
	add(events.ReasonEmotionTrauma,
		"一桩惨事压了下来，心里落了道难愈的伤。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}经历了一场惨事，她在乎的人，眼里熄了光。",
			relevance.Redline:        "那道她最怕被踩的红线被狠狠碾过，留下一道难愈的伤：{event}",
			relevance.Goal:           "这桩惨事压垮了心气，她想做成的事，先被压在了这道伤底下。",
			relevance.DebtGrudgeLove: "这道伤连着她与{friend}的旧债旧情，越想越疼。",
			relevance.Geo:            "{region}发生的惨事，在她脚下这片地方留了道抹不掉的阴影。",
			relevance.Legacy:         "这桩惨事伤到了她血脉里最软的那处。",
		},
	)

	// ===== FACTION_COLLAPSE（势力崩塌）：大变故，地理/目标视角的覆水难收 =====
	add(events.ReasonFactionCollapse,
		"一方势力土崩瓦解，天，要变了。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}所依的那方势力塌了——她在乎的人，一夜没了靠山。",
			relevance.Redline:        "树倒猢狲散之际，多少人正越过她绝不容许的那条线：{event}",
			relevance.Goal:           "{region}赖以立足的势力崩了，她苦心经营的局，根基跟着动摇。",
			relevance.DebtGrudgeLove: "势力一塌，她与{friend}之间那笔债仇情，被这场大变故劈成了敌我两端。",
			relevance.Geo:            "{region}的天要变了：一方势力土崩瓦解，人心四散。",
			relevance.Legacy:         "靠山崩塌，连她血脉所系的那一脉，也被这场塌方扯进了风里。",
		},
		relevance.Redline, // 大崩塌中的触线→停玩家手里
	)

	// ===== REDLINE_TRIP（显式触碰红线）：任何触线，红线视角必停玩家手里 =====
	add(events.ReasonRedlineTrip,
		"有人越过了她（或你）立下的那条线。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}越过了那条不该越的线——她在乎的人，亲手碰了禁忌。",
			relevance.Redline:        "她亲手划下的红线，被人当面踩了过去：{event}",
			relevance.Goal:           "为了那点目标，有人越了她立下的线，路走歪了。",
			relevance.DebtGrudgeLove: "这一越线，连着她与{friend}之间未了的债仇情，又添了新怨。",
			relevance.Geo:            "就在{region}，有人越过了那条不该越的线。",
			relevance.Legacy:         "越线之举伤及她血脉所系的根本：{event}",
		},
		relevance.Redline, // 红线被当面踩→必停玩家手里
	)

	// ===== COMMAND_FORCED_ORDER（被强令越线）：被你/他人强令做高风险事，红线触类需知晓 =====
	add(events.ReasonCommandForced,
		"她被强令做了一件高风险、近乎越线的事。",
		map[relevance.AnchorKind]string{
			relevance.Relation:       "{friend}被强令去做一件凶险的事——她在乎的人，被人推上了刀尖。",
			relevance.Redline:        "一道严令把她逼到了红线边上：{event}",
			relevance.Goal:           "这道强令打乱了她的盘算，想做成的事，被硬生生扭了向。",
			relevance.DebtGrudgeLove: "被强令做的这件事，连着她与{friend}之间那笔旧账，越扯越紧。",
			relevance.Geo:            "在{region}，她被强令去做一件凶险的事。",
			relevance.Legacy:         "这道严令逼她拿血脉所系的东西去冒险：{event}",
		},
	)

	return out
}()

// translationCacheKey 是翻译矩阵的内存缓存键：(reason_code × anchor_kind)。
type translationCacheKey struct {
	reasonCode string
	anchorKind string
}

// loadedTemplate 是 loadTranslationTemplate 的返回（缓存值）：narrative 模板 + 是否强制升级待决策 + 是否命中表（非默认兜底）。
type loadedTemplate struct {
	Narrative    string
	ForcePending bool
	Matched      bool // true=命中精确组或通用兜底行；false=回退到 DefaultReasonText（缺模板）。
}

// 翻译矩阵的进程级内存缓存（避免每次命运路由都查库；首次未命中也缓存负结果，省掉重复 fallback 查询）。
var (
	translationCacheMu sync.RWMutex
	translationCache   = map[translationCacheKey]loadedTemplate{}
)

// invalidateTranslationCache 清空翻译模板内存缓存（SeedTranslationTemplates 写表后调，让新矩阵即时生效；测试也用）。
func invalidateTranslationCache() {
	translationCacheMu.Lock()
	translationCache = map[translationCacheKey]loadedTemplate{}
	translationCacheMu.Unlock()
}

// ===== 缺模板回退遥测（§5 风险「翻译模板覆盖不全」：缺一条就计数，便于排期补全）=====

// 进程级翻译遥测计数（跨会话/请求累计）：total=总查询次数，missing=回退到 DefaultReasonText 的次数。
var (
	translationLookupTotal   atomic.Int64
	translationLookupMissing atomic.Int64
)

// TranslationStats 返回进程级翻译查表累计：总次数与「缺模板回退 DefaultReasonText」次数（surfaced 给 /healthz 排期补缺）。
func TranslationStats() (total int64, missing int64) {
	return translationLookupTotal.Load(), translationLookupMissing.Load()
}

// SeedTranslationTemplates 把内置翻译矩阵幂等 upsert 进 translation_templates 表（启动时调，双驱动安全）。
// 已存在的 (reason_code, anchor_kind) 行被更新为内置文案（运营态如手改了表、重启会被内置矩阵覆盖——内置矩阵是代码权威基线，
// 运营增补建议用新 reason_code/anchor 行或独立运维通道，与本基线 upsert 不冲突）。写完清缓存让新矩阵即时生效。best-effort 失败返回 err 由调用方决定。
func SeedTranslationTemplates(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	upsert := `
		INSERT INTO translation_templates (id, reason_code, anchor_kind, narrative_template, force_pending, priority, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(reason_code, anchor_kind) DO UPDATE SET
			narrative_template = excluded.narrative_template,
			force_pending = excluded.force_pending,
			priority = excluded.priority,
			updated_at = excluded.updated_at`
	if dbdialect.IsMySQL(db) {
		upsert = `
		INSERT INTO translation_templates (id, reason_code, anchor_kind, narrative_template, force_pending, priority, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			narrative_template = VALUES(narrative_template),
			force_pending = VALUES(force_pending),
			priority = VALUES(priority),
			updated_at = VALUES(updated_at)`
	}

	for _, t := range builtinTranslationTemplates {
		forcePending := 0
		if t.ForcePending {
			forcePending = 1
		}
		// id 用稳定派生键（reason_code|anchor_kind），与 UNIQUE(reason_code, anchor_kind) 同口径——确定性、可复算、不依赖随机。
		id := "tt_" + string(t.ReasonCode) + "|" + string(t.AnchorKind)
		if _, err := tx.ExecContext(ctx, upsert,
			id, string(t.ReasonCode), string(t.AnchorKind), t.Narrative, forcePending, t.Priority, now,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	invalidateTranslationCache()
	return nil
}

// loadTranslationTemplate 按 (reason_code, anchor_kind) 三级回退查翻译模板（带进程级内存缓存）：
//  1. 精确组 (reason_code, anchor_kind)：最贴合；
//  2. 通用兜底 (reason_code, anchor_kind=”)：命中锚但该精确组缺表时用；
//  3. 都没有 → events.DefaultReasonText（缺模板保守 beat）+ 计一条遥测（missing），便于排期补全。
//
// best-effort：db 为 nil 或查库失败时优雅回退内置矩阵 / DefaultReasonText，绝不阻断命运路由。确定性：同输入恒同输出。
func loadTranslationTemplate(ctx context.Context, db *sql.DB, reasonCode events.ReasonCode, anchorKind relevance.AnchorKind) loadedTemplate {
	translationLookupTotal.Add(1)
	key := translationCacheKey{reasonCode: string(reasonCode), anchorKind: string(anchorKind)}

	// 命中缓存直接返回（负结果也缓存，省掉重复 fallback 查询）。
	translationCacheMu.RLock()
	if cached, ok := translationCache[key]; ok {
		translationCacheMu.RUnlock()
		if !cached.Matched {
			translationLookupMissing.Add(1)
		}
		return cached
	}
	translationCacheMu.RUnlock()

	result := resolveTranslationTemplate(ctx, db, reasonCode, anchorKind)
	if !result.Matched {
		translationLookupMissing.Add(1)
		// 缺模板遥测：缺一条就 warn 一次（首次未命中即记，缓存后不再重复 warn），便于运营按 reason_code×anchor 排期补全。
		slog.Warn("translation template missing, fell back to DefaultReasonText",
			"reason_code", string(reasonCode), "anchor_kind", string(anchorKind))
	}

	translationCacheMu.Lock()
	translationCache[key] = result
	translationCacheMu.Unlock()
	return result
}

// resolveTranslationTemplate 是 loadTranslationTemplate 的无缓存内核：先查库（精确组→通用兜底），库不可用/未命中再退内置矩阵，
// 最后退 events.DefaultReasonText。纯查找、无副作用（遥测/缓存由 loadTranslationTemplate 负责）、确定性。
func resolveTranslationTemplate(ctx context.Context, db *sql.DB, reasonCode events.ReasonCode, anchorKind relevance.AnchorKind) loadedTemplate {
	// 1+2) 查库：精确组优先，回退 anchor_kind='' 通用兜底。
	if db != nil {
		if t, ok := queryTranslationTemplate(ctx, db, reasonCode, anchorKind); ok {
			return t
		}
		if anchorKind != "" {
			if t, ok := queryTranslationTemplate(ctx, db, reasonCode, ""); ok {
				return t
			}
		}
	}
	// 1+2 内置兜底：库不可用/未 seed 时，用内置矩阵做同样的精确→通用回退（保证 seed 前/无 DB 测试也有 beat）。
	if t, ok := lookupBuiltinTemplate(reasonCode, anchorKind); ok {
		return t
	}
	if anchorKind != "" {
		if t, ok := lookupBuiltinTemplate(reasonCode, ""); ok {
			return t
		}
	}
	// 3) 缺模板：回退 DefaultReasonText（保守 beat），Matched=false 触发遥测。
	if def, ok := events.Lookup(reasonCode); ok && def.DefaultReasonText != "" {
		return loadedTemplate{Narrative: def.DefaultReasonText, Matched: false}
	}
	return loadedTemplate{Matched: false}
}

// queryTranslationTemplate 按精确 (reason_code, anchor_kind) 查一行翻译模板。未命中返回 ok=false（含查库出错——best-effort）。
func queryTranslationTemplate(ctx context.Context, db *sql.DB, reasonCode events.ReasonCode, anchorKind relevance.AnchorKind) (loadedTemplate, bool) {
	var narrative string
	var forcePending int
	err := db.QueryRowContext(ctx,
		`SELECT narrative_template, force_pending FROM translation_templates WHERE reason_code = ? AND anchor_kind = ?`,
		string(reasonCode), string(anchorKind),
	).Scan(&narrative, &forcePending)
	if err != nil {
		return loadedTemplate{}, false // best-effort：未命中或查库出错都回退下一级
	}
	return loadedTemplate{Narrative: narrative, ForcePending: forcePending != 0, Matched: true}, true
}

// lookupBuiltinTemplate 在内置矩阵里按精确 (reason_code, anchor_kind) 查一行（DB 兜底/无 DB 测试用）。确定性、纯函数。
func lookupBuiltinTemplate(reasonCode events.ReasonCode, anchorKind relevance.AnchorKind) (loadedTemplate, bool) {
	for _, t := range builtinTranslationTemplates {
		if t.ReasonCode == reasonCode && t.AnchorKind == anchorKind {
			return loadedTemplate{Narrative: t.Narrative, ForcePending: t.ForcePending, Matched: true}, true
		}
	}
	return loadedTemplate{}, false
}
