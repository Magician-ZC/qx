// Package featureflags 是「运行时 flag 开关层」：把散落在各游戏逻辑里的 os.Getenv("QUNXIANG_X")
// 收口到一个线程安全的覆盖层，让 GM 后台可在**不重启进程**的前提下灰度开关游戏性特性。
//
// 设计取向（与项目既有 flag idiom 完全兼容、行为默认不变）：
//   - EnvOrOverride(name) 取值优先级：运行时 override（GM 设过的）> os.Getenv(name)（进程环境）。
//     未设过 override 时严格等价于裸 os.Getenv —— 这是「逐个改、行为默认不变」的核心保证：
//     调用点把 os.Getenv("QUNXIANG_X") 换成 featureflags.EnvOrOverride("QUNXIANG_X") 后，
//     在 GM 没动手前，每一处解析（true/1/yes/on …）的结果逐位一致。
//   - override 是**字符串覆盖**而非布尔：因为各 flag 的解析语义不同（有的默认开、有的取多档字符串
//     如 QUNXIANG_WORLD_BINDING 的 shared/per_session/off），本层只负责「提供哪个原始字符串」，
//     具体真值解析仍由各调用点自己的 switch 决定，零语义侵入。ClearOverride 后回落 os.Getenv。
//
// 持久化：override 落一张 feature_flag_overrides 表（双驱动），进程启动时 LoadFromStore 回灌，
// 使 GM 设过的开关**重启后存活**。本包刻意不 import storage（避免下游包依赖膨胀/潜在环），
// 只声明最小 Store 接口由调用方（admin_world.go）注入实现；未注入时纯内存（重启即失，有注释残留）。
//
// 线程安全：override map 由 sync.RWMutex 守护；EnvOrOverride 在热路径外（flag 解析频率远低于决策热路径），
// RLock 开销可忽略。
package featureflags

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
)

// getenv 是 os.Getenv 的薄包装，集中环境变量读取点（便于阅读/未来注入测试桩）。
func getenv(name string) string { return os.Getenv(name) }

// overrides 是进程级运行时覆盖表（name → 原始字符串值）。由 mu 守护。
var (
	mu        sync.RWMutex
	overrides = map[string]string{}
	store     Store // 可选持久化后端；nil=纯内存
)

// Store 是 override 持久化的最小后端接口（由 admin_world.go 用 *sql.DB 实现并注入）。
// 刻意只声明本包真正需要的三件事，避免 featureflags → storage 的包依赖。
type Store interface {
	// Upsert 持久化一条 override（重启后由 Load 回灌）。
	Upsert(ctx context.Context, name, value string) error
	// Delete 删除一条 override 的持久记录（ClearOverride 时调用）。
	Delete(ctx context.Context, name string) error
	// Load 读全部已持久的 override（启动回灌）。
	Load(ctx context.Context) (map[string]string, error)
}

// SetStore 注入持久化后端（启动期由集成方调用一次）。传 nil 退化为纯内存。
func SetStore(s Store) {
	mu.Lock()
	store = s
	mu.Unlock()
}

// LoadFromStore 在启动期把已持久的 override 回灌进内存（best-effort：store 未注入或读失败时不报错、保持空表）。
// 返回回灌条数与错误（错误供启动期日志，不应阻断服务启动）。
func LoadFromStore(ctx context.Context) (int, error) {
	mu.RLock()
	s := store
	mu.RUnlock()
	if s == nil {
		return 0, nil
	}
	loaded, err := s.Load(ctx)
	if err != nil {
		return 0, err
	}
	mu.Lock()
	for name, value := range loaded {
		overrides[normalize(name)] = value
	}
	n := len(loaded)
	mu.Unlock()
	return n, nil
}

// EnvOrOverride 返回某 flag 的生效原始字符串：有运行时 override 返 override，否则回落 os.Getenv(name)。
// 这是全部调用点应替换 os.Getenv 的唯一入口。name 大小写/首尾空白归一后查 override 表。
func EnvOrOverride(name string) string {
	key := normalize(name)
	mu.RLock()
	v, ok := overrides[key]
	mu.RUnlock()
	if ok {
		return v
	}
	return getenv(name)
}

// SetOverride 设置一条运行时 override（GM 后台调用）。空 name 忽略。
// 持久化 best-effort：store 注入时同步落库（返回其错误供端点透传），未注入时纯内存。
func SetOverride(name, value string) error {
	key := normalize(name)
	if key == "" {
		return nil
	}
	mu.Lock()
	overrides[key] = value
	s := store
	mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Upsert(context.Background(), key, value)
}

// ClearOverride 清除一条运行时 override，使 EnvOrOverride 回落 os.Getenv。空 name 忽略。
// 持久化 best-effort：store 注入时同步删库。返回是否本来存在该 override + 落库错误。
func ClearOverride(name string) (existed bool, err error) {
	key := normalize(name)
	if key == "" {
		return false, nil
	}
	mu.Lock()
	_, existed = overrides[key]
	delete(overrides, key)
	s := store
	mu.Unlock()
	if s == nil {
		return existed, nil
	}
	return existed, s.Delete(context.Background(), key)
}

// ListOverrides 返回当前全部运行时 override 的快照副本（name → value），按 name 升序无关（map 副本）。
func ListOverrides() map[string]string {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]string, len(overrides))
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// FlagSpec 描述一个可运营的游戏 flag：名字、默认态语义、一句中文说明。
// DefaultOn 表示「未设环境变量时的默认行为是否为开」（对齐各调用点 switch 的默认分支）。
// Values 非空时表示该 flag 取多档字符串值（如 world_binding），而非简单布尔。
type FlagSpec struct {
	Name        string   // 环境变量名（如 QUNXIANG_DUNGEON）
	Description string   // 一句中文说明（GM 后台展示）
	DefaultOn   bool     // 默认是否开（布尔型 flag 才有意义）
	Values      []string // 多档字符串型 flag 的可选值（空=布尔型，true/1/yes/on 系列）
}

// knownGameplayFlags 是「可运营的游戏性 flag」白名单（清单 + 默认值 + 说明）。
// 严格只含**游戏性 per-call flag**（GM 可在运行时安全开关、影响世界演化的玩法特性），
// **刻意不含**：region-runner 启动期 flag（QUNXIANG_REGION_RUNNER_ENABLED/REGION_SHARDING，
// 建局期一次性快照、运行时切换无意义）、LLM/DB/合规/商业化 flag（QUNXIANG_BILLING_ENABLED/
// CONTENT_SAFETY/OPS_TOKEN 等，由各自专门通道治理，不进游戏开关层）。
// 顺序即 KnownGameplayFlags 的稳定返回顺序（已按主题分组）。
var knownGameplayFlags = []FlagSpec{
	// —— PvE / 威胁 / 世界 Boss —— //
	{Name: "QUNXIANG_DUNGEON", Description: "副本（多层逐层 PvE 推进）；默认关，开后玩家可入副本逐层消耗战分赃。", DefaultOn: false},
	{Name: "QUNXIANG_AUTO_PVE", Description: "野外威胁自动开打（撞见威胁的合格单位按比例真实交战，否则仅投高光卡）；默认关。", DefaultOn: false},
	{Name: "QUNXIANG_WORLD_BOSS_AUTO", Description: "世界 Boss 自动刷新（按世界确定性投放可多人协作消耗的世界 Boss）；默认关。", DefaultOn: false},

	// —— 命运 / 跨玩家关联 / 世界化 —— //
	{Name: "QUNXIANG_SERENDIPITY", Description: "破圈预算（每日≤1 件零锚来源事件升档进高光卡作新锚种子）；默认关。", DefaultOn: false},
	{Name: "QUNXIANG_WORLDIZE_INBOUND", Description: "入向世界化扇出（玩家做出会激起涟漪的事时反查谁的锚被点亮）；默认关。", DefaultOn: false},
	{Name: "QUNXIANG_AUTO_MATCH", Description: "野外同行自动撮合（确定性四因子撮合并绑社会客体）；默认关。", DefaultOn: false},
	{Name: "QUNXIANG_AUTO_SOCIAL", Description: "社交自治（玩家不在场时本局单位间自动结识/结盟/反目/复仇）；**默认开**。", DefaultOn: true},

	// —— 零和 / 一致性 / 冻结 / 血仇 —— //
	{Name: "QUNXIANG_ZEROSUM_CONTEST", Description: "排他标的确定性零和裁决（反 P2W 机制基石，胜率∝Score、与频率无关）；**默认开**。", DefaultOn: true},
	{Name: "QUNXIANG_BLOOD_FEUD", Description: "血仇沿关系图传播（在乎死者的人继承敌意，按跳数失真衰减）；**默认开**。", DefaultOn: true},
	{Name: "QUNXIANG_FREEZE_LIST", Description: "冻结清单（传家宝/红线动作拦截，让角色「向命运低头」而非自动卖/叛）；默认关。", DefaultOn: false},
	{Name: "QUNXIANG_CONSISTENCY_TIGHTEN", Description: "一致性收紧闭环（OOC 率高时压低突然戏剧性动作的惊喜上限）；默认关。", DefaultOn: false},

	// —— 自治曲线 / 野心 / 村庄 —— //
	{Name: "QUNXIANG_COURAGE_CURVE", Description: "自治胆量曲线（离线越久越自主但越保守，进抗命概率与广度门）；默认关。", DefaultOn: false},
	{Name: "QUNXIANG_AMBITION_SCORING", Description: "野心打分（带野心语义的候选动作按角色野心引力加权）；默认关。", DefaultOn: false},
	{Name: "QUNXIANG_MAIN_VILLAGE", Description: "主战局出生织 20 人关系网（命运开盒「身边已有二十个有名有姓的人」）；**默认开**。", DefaultOn: true},

	// —— 多档字符串型 —— //
	{
		Name:        "QUNXIANG_WORLD_BINDING",
		Description: "世界绑定策略：shared（默认，共享主世界，跨玩家关联前置）/ per_session（每局专属世界，隔离）/ off（不接入世界，等价旧行为）。",
		Values:      []string{"shared", "per_session", "off"},
	},
}

// KnownGameplayFlags 返回可运营游戏 flag 清单的副本（含默认值与说明），供 GM 后台渲染开关面板。
// 副本返回，调用方修改不影响内部白名单。
func KnownGameplayFlags() []FlagSpec {
	out := make([]FlagSpec, len(knownGameplayFlags))
	copy(out, knownGameplayFlags)
	return out
}

// EffectiveFlag 是某 flag 的「当前生效态」视图（GET /api/admin/flags 用）。
type EffectiveFlag struct {
	FlagSpec
	OverrideSet   bool   // 是否设了运行时 override
	OverrideValue string // override 原始值（OverrideSet=false 时为空）
	EnvValue      string // os.Getenv 原始值（用于展示 GM 没动手时的底值）
	Effective     string // EnvOrOverride 实际生效值
}

// SnapshotEffective 返回全部已知游戏 flag 的当前生效态（白名单顺序）。供 GET /api/admin/flags 直接序列化。
func SnapshotEffective() []EffectiveFlag {
	specs := KnownGameplayFlags()
	ov := ListOverrides()
	out := make([]EffectiveFlag, 0, len(specs))
	for _, spec := range specs {
		key := normalize(spec.Name)
		v, set := ov[key]
		ef := EffectiveFlag{
			FlagSpec:      spec,
			OverrideSet:   set,
			OverrideValue: v,
			EnvValue:      getenv(spec.Name),
			Effective:     EnvOrOverride(spec.Name),
		}
		out = append(out, ef)
	}
	return out
}

// IsKnownGameplayFlag 判断某 name 是否在可运营白名单内（GM 后台拒绝设置白名单外的 flag，防误改 DB/合规/商业开关）。
func IsKnownGameplayFlag(name string) bool {
	key := normalize(name)
	for _, spec := range knownGameplayFlags {
		if normalize(spec.Name) == key {
			return true
		}
	}
	return false
}

// KnownFlagNames 返回白名单内全部 flag 名（升序），便于测试/校验。
func KnownFlagNames() []string {
	out := make([]string, 0, len(knownGameplayFlags))
	for _, spec := range knownGameplayFlags {
		out = append(out, spec.Name)
	}
	sort.Strings(out)
	return out
}

// normalize 归一 flag 名：去首尾空白 + 转大写（环境变量名约定大写，容错大小写输入）。
func normalize(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}
