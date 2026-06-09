package session

// 文件说明：M1 data-driven 翻译矩阵 DB 表（translation_seed.go）的集成+纯函数测试——覆盖：
// 幂等 seed、精确组命中、anchor_kind='' 通用回退、缺模板回退 DefaultReasonText+遥测计数、force_pending 标记、
// 占位 {friend}/{region}/{event} 安全替换、内存缓存。前缀统一 TranslationSeed* / TranslationDB* 避免与既有命运/翻译测试撞名。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

// newTranslationTestDB 起一个临时 SQLite（含 translation_templates 表，由 sqlite store 建），并清空进程级翻译缓存/遥测，
// 保证每个用例从干净状态开始（缓存/遥测是进程级全局，串行测试间须隔离）。
func newTranslationTestDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "translation.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	invalidateTranslationCache()
	translationLookupTotal.Store(0)
	translationLookupMissing.Store(0)
	return context.Background(), db
}

// ---- 幂等 seed ----

func TestSeedTranslationTemplates_Idempotent(t *testing.T) {
	ctx, db := newTranslationTestDB(t)
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("首次 seed: %v", err)
	}
	var n1 int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM translation_templates`).Scan(&n1); err != nil {
		t.Fatalf("count1: %v", err)
	}
	if n1 != len(builtinTranslationTemplates) {
		t.Fatalf("seed 行数应等于内置矩阵 %d，得到 %d", len(builtinTranslationTemplates), n1)
	}
	// 再 seed 一次：行数不应翻倍（UNIQUE(reason_code, anchor_kind) + upsert 幂等）。
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("二次 seed: %v", err)
	}
	var n2 int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM translation_templates`).Scan(&n2); err != nil {
		t.Fatalf("count2: %v", err)
	}
	if n2 != n1 {
		t.Fatalf("重复 seed 应幂等，行数 %d→%d", n1, n2)
	}
}

// ---- 精确组命中 ----

func TestLoadTranslationTemplate_ExactHit(t *testing.T) {
	ctx, db := newTranslationTestDB(t)
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := loadTranslationTemplate(ctx, db, events.ReasonCombatDown, relevance.Relation)
	if !got.Matched {
		t.Fatalf("COMBAT_DOWN×relation 应命中精确组")
	}
	// 精确组文案应来自内置矩阵（含「生死未卜」字样），而非 DefaultReasonText。
	if !strings.Contains(got.Narrative, "生死未卜") {
		t.Fatalf("应命中精确 relation 模板，得到 %q", got.Narrative)
	}
}

// ---- anchor_kind='' 通用回退 ----

func TestLoadTranslationTemplate_GenericFallback(t *testing.T) {
	ctx, db := newTranslationTestDB(t)
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 删掉某 reason_code 的某锚精确行，强制走 anchor_kind='' 通用兜底。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM translation_templates WHERE reason_code = ? AND anchor_kind = ?`,
		string(events.ReasonRelationRescue), string(relevance.Goal),
	); err != nil {
		t.Fatalf("删精确行: %v", err)
	}
	invalidateTranslationCache() // 删表后须清缓存，否则命中旧缓存
	got := loadTranslationTemplate(ctx, db, events.ReasonRelationRescue, relevance.Goal)
	if !got.Matched {
		t.Fatalf("精确行被删后应回退 anchor_kind='' 通用兜底（仍 Matched）")
	}
	// 通用兜底文案=该 reason_code 的 generic（救援 generic 含「鬼门关」）。
	if !strings.Contains(got.Narrative, "鬼门关") {
		t.Fatalf("应回退到通用兜底文案，得到 %q", got.Narrative)
	}
}

// ---- 缺模板回退 DefaultReasonText + 遥测计数 ----

func TestLoadTranslationTemplate_MissingFallsBackToDefaultAndCounts(t *testing.T) {
	ctx, db := newTranslationTestDB(t)
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 选一个内置矩阵未覆盖的 reason_code（有 DefaultReasonText 但无翻译模板）。
	missingCode := events.ReasonSurvivalHunger
	if _, ok := lookupBuiltinTemplate(missingCode, ""); ok {
		t.Fatalf("前提失败：%s 不应在内置矩阵里", missingCode)
	}
	total0, missing0 := TranslationStats()
	got := loadTranslationTemplate(ctx, db, missingCode, relevance.Relation)
	if got.Matched {
		t.Fatalf("未覆盖的 reason_code 应未命中模板（Matched=false）")
	}
	// 回退文案=该 reason_code 的 DefaultReasonText（仍是一句可用保守 beat）。
	def, _ := events.Lookup(missingCode)
	if got.Narrative != def.DefaultReasonText {
		t.Fatalf("缺模板应回退 DefaultReasonText %q，得到 %q", def.DefaultReasonText, got.Narrative)
	}
	total1, missing1 := TranslationStats()
	if total1 != total0+1 {
		t.Fatalf("查表总数应 +1，%d→%d", total0, total1)
	}
	if missing1 != missing0+1 {
		t.Fatalf("缺模板应计一条 missing 遥测，%d→%d", missing0, missing1)
	}
}

// ---- force_pending 标记 ----

func TestLoadTranslationTemplate_ForcePendingFlag(t *testing.T) {
	ctx, db := newTranslationTestDB(t)
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 密友倒地 COMBAT_DOWN×relation 是 §1.2 点名的 force_pending 标杆。
	fp := loadTranslationTemplate(ctx, db, events.ReasonCombatDown, relevance.Relation)
	if !fp.Matched || !fp.ForcePending {
		t.Fatalf("COMBAT_DOWN×relation 应标 force_pending，得到 matched=%v fp=%v", fp.Matched, fp.ForcePending)
	}
	// 同 reason_code 下非 force_pending 的锚（COMBAT_DOWN×goal）不应标。
	invalidateTranslationCache()
	notFP := loadTranslationTemplate(ctx, db, events.ReasonCombatDown, relevance.Goal)
	if !notFP.Matched || notFP.ForcePending {
		t.Fatalf("COMBAT_DOWN×goal 不应标 force_pending，得到 fp=%v", notFP.ForcePending)
	}
	// 暖色事件（被救援）任何锚都不应 force_pending。
	invalidateTranslationCache()
	rescue := loadTranslationTemplate(ctx, db, events.ReasonRelationRescue, relevance.Relation)
	if rescue.ForcePending {
		t.Fatalf("RELATION_RESCUED×relation 不应 force_pending")
	}
}

// 每个 force_pending 组都必须有专属（精确）模板，且文案非 DefaultReasonText（§1.2「force_pending 类必须有专属模板」）。
func TestBuiltinTranslationTemplates_ForcePendingHaveDedicatedTemplates(t *testing.T) {
	for _, tmpl := range builtinTranslationTemplates {
		if !tmpl.ForcePending {
			continue
		}
		if tmpl.AnchorKind == "" {
			t.Fatalf("force_pending 行不应是通用兜底（anchor_kind=''）：%s", tmpl.ReasonCode)
		}
		if strings.TrimSpace(tmpl.Narrative) == "" {
			t.Fatalf("force_pending 组缺专属模板：%s × %s", tmpl.ReasonCode, tmpl.AnchorKind)
		}
		if def, ok := events.Lookup(tmpl.ReasonCode); ok && tmpl.Narrative == def.DefaultReasonText {
			t.Fatalf("force_pending 组不应只用 DefaultReasonText 兜底：%s × %s", tmpl.ReasonCode, tmpl.AnchorKind)
		}
	}
}

// 内置矩阵对每个登记的 reason_code 都满 6 锚类 + 1 通用兜底（防漏配，§5 风险「翻译模板覆盖不全」）。
func TestBuiltinTranslationTemplates_FullAnchorCoverage(t *testing.T) {
	anchors := []relevance.AnchorKind{
		relevance.Relation, relevance.Redline, relevance.Goal,
		relevance.DebtGrudgeLove, relevance.Geo, relevance.Legacy,
	}
	codes := map[events.ReasonCode]bool{}
	for _, tmpl := range builtinTranslationTemplates {
		codes[tmpl.ReasonCode] = true
	}
	for code := range codes {
		for _, a := range anchors {
			if _, ok := lookupBuiltinTemplate(code, a); !ok {
				t.Fatalf("内置矩阵缺精确模板：%s × %s", code, a)
			}
		}
		if _, ok := lookupBuiltinTemplate(code, ""); !ok {
			t.Fatalf("内置矩阵缺通用兜底：%s × ''", code)
		}
		// 缺模板回退里 reason_code 都应在 events 目录登记（reason-codes 全登记不变量）。
		if _, ok := events.Lookup(code); !ok {
			t.Fatalf("矩阵 reason_code 未在 events 目录登记：%s", code)
		}
	}
}

// ---- 占位 {friend}/{region}/{event} 安全替换 ----

func TestTranslateFateBeatFromDB_PlaceholderRendering(t *testing.T) {
	ctx, db := newTranslationTestDB(t)
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// COMBAT_DOWN×geo 模板含 {region} 与 {friend}。
	ev := FateEvent{
		ReasonCode:     events.ReasonCombatDown,
		ActorID:        "u_laowu",
		SourceRegionID: "北岭",
		Summary:        "老吴倒下了",
	}
	beat, _ := translateFateBeatFromDB(ctx, db, ev, relevance.Geo)
	if beat == "" {
		t.Fatalf("应渲染出非空 beat")
	}
	// {friend} 应渲染为 ActorID，{region} 应渲染为来源地；不应残留任何裸占位。
	if !strings.Contains(beat, "u_laowu") {
		t.Fatalf("{friend} 应渲染为 ActorID，得到 %q", beat)
	}
	if !strings.Contains(beat, "北岭") {
		t.Fatalf("{region} 应渲染为来源地，得到 %q", beat)
	}
	for _, ph := range []string{"{friend}", "{region}", "{event}", "{target}"} {
		if strings.Contains(beat, ph) {
			t.Fatalf("渲染后残留占位 %s：%q", ph, beat)
		}
	}
}

// {region} 缺省（无 SourceRegionID）应回落「她所在的地方」，不残留裸占位。
func TestRenderFateTemplate_RegionAndFriendDefaults(t *testing.T) {
	got := renderFateTemplate("在{region}，{friend}倒下了", FateEvent{})
	if strings.Contains(got, "{region}") || strings.Contains(got, "{friend}") {
		t.Fatalf("缺省占位应被替换，得到 %q", got)
	}
	if !strings.Contains(got, "她所在的地方") || !strings.Contains(got, "那个人") {
		t.Fatalf("{region}/{friend} 缺省应回落缺省词，得到 %q", got)
	}
}

// ---- 内存缓存：第二次查表不再触底（命中缓存）----

func TestLoadTranslationTemplate_Cached(t *testing.T) {
	ctx, db := newTranslationTestDB(t)
	if err := SeedTranslationTemplates(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first := loadTranslationTemplate(ctx, db, events.ReasonCharacterDied, relevance.Relation)
	// 删掉该行：若仍命中缓存，第二次返回应与第一次逐字节一致（证明走了缓存，未重新查库）。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM translation_templates WHERE reason_code = ? AND anchor_kind = ?`,
		string(events.ReasonCharacterDied), string(relevance.Relation),
	); err != nil {
		t.Fatalf("删行: %v", err)
	}
	second := loadTranslationTemplate(ctx, db, events.ReasonCharacterDied, relevance.Relation)
	if second.Narrative != first.Narrative || second.Matched != first.Matched {
		t.Fatalf("第二次应命中缓存返回相同结果，first=%q second=%q", first.Narrative, second.Narrative)
	}
}

// ---- 无 DB 也能工作（best-effort：db=nil 退内置矩阵）----

func TestLoadTranslationTemplate_NilDBFallsBackToBuiltin(t *testing.T) {
	invalidateTranslationCache()
	got := loadTranslationTemplate(context.Background(), nil, events.ReasonRelationBetray, relevance.Relation)
	if !got.Matched {
		t.Fatalf("db=nil 应回退内置矩阵命中")
	}
	if !got.ForcePending {
		t.Fatalf("RELATION_BETRAYAL×relation 内置应标 force_pending")
	}
}
