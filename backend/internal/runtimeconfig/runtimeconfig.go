// Package runtimeconfig 是「类型化运行时配置层」：把散落在各处的硬编码玩法常量（数值/字符串/枚举）与
// 启动期 LLM 设置收口到一个线程安全的覆盖层，让 GM 后台在**不重启进程**的前提下实时调参。
//
// 设计取向（与 internal/featureflags 的范式完全同构、行为默认不变）：
//   - featureflags 只能存「字符串布尔开关」；本层升级为**带类型 + 范围 + namespace + 默认值**的参数：
//     每个参数先 Register 一个 ParamSpec（名字/命名空间/类型/默认/min-max/枚举值/说明/是否热生效），
//     读取走 GetFloat/GetInt/GetBool/GetString/GetEnum —— override（GM 设过的）> 注册默认值。
//   - 关键不变量：GM 未动手前，GetXxx(name) 严格返回 spec.Default 解析值 —— 即「把 const X = 0.85 换成
//     runtimeconfig.GetFloat(name) 且 spec.Default="0.85"」后，行为逐位一致。这是「逐个迁移、默认不变」的保证。
//   - SetOverride 在写入时做**类型 + 范围/枚举校验**：非法值直接拒绝（featureflags 不校验，本层校验，
//     因为数值越界会破坏玩法平衡 / 触发 panic）。校验不过不落库、不进内存。
//
// 持久化：override 落一张 runtime_config_overrides 表（双驱动），进程启动时 LoadFromStore 回灌，
// 使 GM 设过的参数**重启后存活**。本包刻意不 import storage（避免下游依赖膨胀），只声明最小 Store
// 接口由调用方（session/admin_world.go）注入实现；未注入时纯内存。
//
// 线程安全：registry 在启动期 Register 完即只读；overrides 由 sync.RWMutex 守护。GetXxx 在 RLock 下读，
// 开销可忽略（即便热循环每拍读一次，RLock 远低于一次 LLM/DB 往返）。
package runtimeconfig

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ParamType 是参数的类型档：决定 GetXxx 的解析与 SetOverride 的校验。
type ParamType string

const (
	TypeBool   ParamType = "bool"   // true/1/yes/on 系列；GetBool
	TypeInt    ParamType = "int"    // 整数；GetInt；可带 Min/Max
	TypeFloat  ParamType = "float"  // 浮点；GetFloat；可带 Min/Max
	TypeString ParamType = "string" // 自由字符串；GetString
	TypeEnum   ParamType = "enum"   // 限定取值集合（Values）；GetEnum/GetString
)

// ParamSpec 描述一个可运营参数：名字、命名空间（分组）、类型、默认值（字符串原始值）、范围/枚举、说明。
// Default 是**字符串原始值**（与各类型的解析约定一致），是 GM 未设 override 时的生效底值。
type ParamSpec struct {
	Name        string    // 全局唯一参数名，约定用点分命名空间前缀（如 "fate.serendipity_daily_budget"）
	Namespace   string    // 分组（GM 后台折叠分组用，如 "fate"/"combat"/"memory"/"llm"）
	Type        ParamType // 类型档
	Default     string    // 默认原始值（未设 override 时的生效底值，必须能被本类型解析）
	Min         *float64  // 数值型下界（含），nil=不限
	Max         *float64  // 数值型上界（含），nil=不限
	Values      []string  // 枚举型可选值集合（Type=enum 时非空）
	Description string    // 一句中文说明（GM 后台展示）
	HotReload   bool      // 是否热生效（true=改后立即被下一次 GetXxx 读到；false=需重启或特殊刷新，仅作展示提示）
}

var (
	mu        sync.RWMutex
	registry  = map[string]ParamSpec{} // name → spec（启动期 Register 完即只读）
	regOrder  []string                 // 注册顺序（稳定快照顺序，按 namespace 再分组在快照层处理）
	overrides = map[string]string{}    // name → 覆盖原始值（GM 设过的）
	store     Store                    // 可选持久化后端；nil=纯内存
)

// Store 是 override 持久化的最小后端接口（由 session/admin_world.go 用 *sql.DB 实现并注入）。
// 与 featureflags.Store 同构，刻意分开声明，避免跨包耦合。
type Store interface {
	Upsert(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
	Load(ctx context.Context) (map[string]string, error)
}

// Register 登记一个或多个参数规格（启动期 init 调用）。重复名 panic（编程错误，启动即暴露）。
// Default 必须能被声明类型解析、且落在 Min/Max 与 Values 内，否则 panic（防止注册一个永远非法的默认值）。
func Register(specs ...ParamSpec) {
	mu.Lock()
	defer mu.Unlock()
	for _, spec := range specs {
		key := normalize(spec.Name)
		if key == "" {
			panic("runtimeconfig: empty param name")
		}
		if _, dup := registry[key]; dup {
			panic("runtimeconfig: duplicate param registration: " + spec.Name)
		}
		spec.Name = key
		if spec.Namespace == "" {
			spec.Namespace = namespaceOf(key)
		}
		if err := validateValue(spec, spec.Default); err != nil {
			panic(fmt.Sprintf("runtimeconfig: invalid default for %s: %v", spec.Name, err))
		}
		registry[key] = spec
		regOrder = append(regOrder, key)
	}
}

// SetStore 注入持久化后端（启动期由集成方调用一次）。传 nil 退化为纯内存。
func SetStore(s Store) {
	mu.Lock()
	store = s
	mu.Unlock()
}

// LoadFromStore 在启动期把已持久的 override 回灌进内存（best-effort）。
// 只回灌**仍注册且校验通过**的参数：spec 已下线或值已非法（如改了范围）的历史 override 被丢弃，绝不污染内存。
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
	defer mu.Unlock()
	n := 0
	for name, value := range loaded {
		key := normalize(name)
		spec, ok := registry[key]
		if !ok {
			continue // 已下线的参数：丢弃历史 override
		}
		if err := validateValue(spec, value); err != nil {
			continue // 历史值已非法（范围/枚举改过）：丢弃，回落默认
		}
		overrides[key] = value
		n++
	}
	return n, nil
}

// effectiveRaw 返回某参数的生效原始字符串：override > spec.Default。未注册返回 ("", false)。
func effectiveRaw(key string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	spec, ok := registry[key]
	if !ok {
		return "", false
	}
	if v, set := overrides[key]; set {
		return v, true
	}
	return spec.Default, true
}

// GetString 返回字符串参数的生效值（override>默认）。未注册返回空串（编程错误，启动期 Register 应已覆盖）。
func GetString(name string) string {
	v, _ := effectiveRaw(normalize(name))
	return v
}

// GetEnum 等价 GetString（枚举型存储即字符串），单列以表意。
func GetEnum(name string) string { return GetString(name) }

// GetBool 返回布尔参数的生效值（true/1/yes/on 不分大小写为真）。解析失败回落默认值的解析。
func GetBool(name string) bool {
	key := normalize(name)
	raw, ok := effectiveRaw(key)
	if !ok {
		return false
	}
	return parseBool(raw)
}

// GetInt 返回整数参数的生效值。解析失败回落 spec.Default 解析；再失败回 0。
func GetInt(name string) int {
	key := normalize(name)
	raw, ok := effectiveRaw(key)
	if !ok {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return n
	}
	mu.RLock()
	spec, has := registry[key]
	mu.RUnlock()
	if has {
		if n, err := strconv.Atoi(strings.TrimSpace(spec.Default)); err == nil {
			return n
		}
	}
	return 0
}

// GetFloat 返回浮点参数的生效值。解析失败回落 spec.Default 解析；再失败回 0。
func GetFloat(name string) float64 {
	key := normalize(name)
	raw, ok := effectiveRaw(key)
	if !ok {
		return 0
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
		return f
	}
	mu.RLock()
	spec, has := registry[key]
	mu.RUnlock()
	if has {
		if f, err := strconv.ParseFloat(strings.TrimSpace(spec.Default), 64); err == nil {
			return f
		}
	}
	return 0
}

// SetOverride 设置一条运行时 override（GM 后台调用）。未注册的 name 拒绝（防误设拼错名）。
// 值经类型 + 范围/枚举校验，非法直接返回错误（不落库、不进内存）。
// 持久化 best-effort：store 注入时同步落库（返回其错误供端点透传），未注入时纯内存。
func SetOverride(name, value string) error {
	key := normalize(name)
	mu.RLock()
	spec, ok := registry[key]
	s := store
	mu.RUnlock()
	if !ok {
		return fmt.Errorf("runtimeconfig: unknown param %q", name)
	}
	if err := validateValue(spec, value); err != nil {
		return fmt.Errorf("runtimeconfig: invalid value for %s: %w", name, err)
	}
	mu.Lock()
	overrides[key] = value
	mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Upsert(context.Background(), key, value)
}

// ClearOverride 清除一条运行时 override，使 GetXxx 回落 spec.Default。返回是否本来存在 + 落库错误。
func ClearOverride(name string) (existed bool, err error) {
	key := normalize(name)
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

// EffectiveParam 是某参数的「当前生效态」视图（GET /api/admin/config 用）。
// 刻意不加 json tag：与 featureflags.EffectiveFlag 同口径，序列化为 Go 大写键名
// （ParamSpec 嵌入字段也无 tag → 大写），前端 AdminConfigItem 按大写键消费（与 AdminFlag 一致）。
type EffectiveParam struct {
	ParamSpec
	OverrideSet   bool   // 是否设了运行时 override
	OverrideValue string // override 原始值（OverrideSet=false 时为空）
	Effective     string // 实际生效原始值（override>默认）
}

// SnapshotEffective 返回全部已注册参数的当前生效态（注册顺序）。供 GET /api/admin/config 序列化。
func SnapshotEffective() []EffectiveParam {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]EffectiveParam, 0, len(regOrder))
	for _, key := range regOrder {
		spec := registry[key]
		v, set := overrides[key]
		eff := spec.Default
		if set {
			eff = v
		}
		out = append(out, EffectiveParam{
			ParamSpec:     spec,
			OverrideSet:   set,
			OverrideValue: v,
			Effective:     eff,
		})
	}
	return out
}

// EffectiveByName 返回单个参数的当前生效态（POST/DELETE /api/admin/config 回最新态用）。未注册返回 (零值, false)。
func EffectiveByName(name string) (EffectiveParam, bool) {
	key := normalize(name)
	mu.RLock()
	defer mu.RUnlock()
	spec, ok := registry[key]
	if !ok {
		return EffectiveParam{}, false
	}
	v, set := overrides[key]
	eff := spec.Default
	if set {
		eff = v
	}
	return EffectiveParam{ParamSpec: spec, OverrideSet: set, OverrideValue: v, Effective: eff}, true
}

// Namespaces 返回全部命名空间（升序去重），供 GM 后台分组渲染。
func Namespaces() []string {
	mu.RLock()
	defer mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, key := range regOrder {
		ns := registry[key].Namespace
		if !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out
}

// IsRegistered 判断某 name 是否已注册（admin 端点拒绝设置未注册参数）。
func IsRegistered(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := registry[normalize(name)]
	return ok
}

// validateValue 按 spec 校验一个原始字符串值：类型可解析 + 落在 Min/Max（数值）/ Values（枚举）内。
func validateValue(spec ParamSpec, raw string) error {
	raw = strings.TrimSpace(raw)
	switch spec.Type {
	case TypeBool:
		switch strings.ToLower(raw) {
		case "true", "1", "yes", "on", "false", "0", "no", "off", "":
			return nil
		default:
			return fmt.Errorf("not a bool: %q", raw)
		}
	case TypeInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("not an int: %q", raw)
		}
		return checkRange(spec, float64(n))
	case TypeFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("not a float: %q", raw)
		}
		return checkRange(spec, f)
	case TypeEnum:
		for _, v := range spec.Values {
			if v == raw {
				return nil
			}
		}
		return fmt.Errorf("not in enum %v: %q", spec.Values, raw)
	case TypeString:
		return nil
	default:
		return fmt.Errorf("unknown param type: %q", spec.Type)
	}
}

// checkRange 校验数值落在 [Min,Max]（边界含），nil 界不限。
func checkRange(spec ParamSpec, v float64) error {
	if spec.Min != nil && v < *spec.Min {
		return fmt.Errorf("%v < min %v", v, *spec.Min)
	}
	if spec.Max != nil && v > *spec.Max {
		return fmt.Errorf("%v > max %v", v, *spec.Max)
	}
	return nil
}

// parseBool 复刻项目布尔解析约定（与 featureflags 调用点 switch 一致）。
func parseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// normalize 归一参数名：去首尾空白 + 转小写（参数名约定点分小写，容错大小写输入）。
func normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// namespaceOf 从点分参数名取第一段作命名空间（Register 未显式给 Namespace 时的缺省）。
func namespaceOf(key string) string {
	if i := strings.IndexByte(key, '.'); i > 0 {
		return key[:i]
	}
	return "general"
}

// Ptr 是 float64 取址辅助（注册带范围参数时写 Min: runtimeconfig.Ptr(0)）。
func Ptr(f float64) *float64 { return &f }
