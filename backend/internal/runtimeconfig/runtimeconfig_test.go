package runtimeconfig

// 文件说明：runtimeconfig 核心层单测——验证「默认不变 / 类型范围校验 / override 热生效 / 回落 / 回灌丢弃非法」。

import (
	"context"
	"testing"
)

// resetForTest 清空包级 registry/overrides/store（测试隔离，仅本包测试可见）。
func resetForTest() {
	mu.Lock()
	registry = map[string]ParamSpec{}
	regOrder = nil
	overrides = map[string]string{}
	store = nil
	mu.Unlock()
}

func TestGetDefaultsWhenNoOverride(t *testing.T) {
	resetForTest()
	Register(ParamSpec{Name: "combat.momentum", Type: TypeFloat, Default: "0.85", Min: Ptr(0), Max: Ptr(1), Description: "势头惩罚"})
	Register(ParamSpec{Name: "fate.budget", Type: TypeInt, Default: "3", Min: Ptr(0), Max: Ptr(10)})
	Register(ParamSpec{Name: "world.binding", Type: TypeEnum, Default: "shared", Values: []string{"shared", "per_session", "off"}})
	Register(ParamSpec{Name: "safety.on", Type: TypeBool, Default: "false"})

	if got := GetFloat("combat.momentum"); got != 0.85 {
		t.Fatalf("GetFloat default = %v, want 0.85", got)
	}
	if got := GetInt("fate.budget"); got != 3 {
		t.Fatalf("GetInt default = %v, want 3", got)
	}
	if got := GetEnum("world.binding"); got != "shared" {
		t.Fatalf("GetEnum default = %q, want shared", got)
	}
	if GetBool("safety.on") {
		t.Fatalf("GetBool default = true, want false")
	}
	// 大小写不敏感
	if got := GetFloat("COMBAT.MOMENTUM"); got != 0.85 {
		t.Fatalf("GetFloat case-insensitive = %v, want 0.85", got)
	}
}

func TestSetOverrideHotReload(t *testing.T) {
	resetForTest()
	Register(ParamSpec{Name: "combat.momentum", Type: TypeFloat, Default: "0.85", Min: Ptr(0), Max: Ptr(1)})
	if err := SetOverride("combat.momentum", "0.5"); err != nil {
		t.Fatalf("SetOverride valid: %v", err)
	}
	if got := GetFloat("combat.momentum"); got != 0.5 {
		t.Fatalf("after override GetFloat = %v, want 0.5", got)
	}
	existed, err := ClearOverride("combat.momentum")
	if err != nil || !existed {
		t.Fatalf("ClearOverride existed=%v err=%v", existed, err)
	}
	if got := GetFloat("combat.momentum"); got != 0.85 {
		t.Fatalf("after clear GetFloat = %v, want default 0.85", got)
	}
}

func TestSetOverrideValidation(t *testing.T) {
	resetForTest()
	Register(ParamSpec{Name: "combat.momentum", Type: TypeFloat, Default: "0.85", Min: Ptr(0), Max: Ptr(1)})
	Register(ParamSpec{Name: "fate.budget", Type: TypeInt, Default: "3", Min: Ptr(0), Max: Ptr(10)})
	Register(ParamSpec{Name: "world.binding", Type: TypeEnum, Default: "shared", Values: []string{"shared", "off"}})

	cases := []struct {
		name, val string
		wantErr   bool
	}{
		{"combat.momentum", "0.5", false},
		{"combat.momentum", "2.0", true},   // 超上界
		{"combat.momentum", "-0.1", true},  // 超下界
		{"combat.momentum", "abc", true},   // 非浮点
		{"fate.budget", "5", false},        // 合法整数
		{"fate.budget", "5.5", true},       // 非整数
		{"fate.budget", "99", true},        // 超界
		{"world.binding", "off", false},    // 枚举内
		{"world.binding", "weird", true},   // 枚举外
		{"unknown.param", "1", true},       // 未注册
	}
	for _, c := range cases {
		err := SetOverride(c.name, c.val)
		if (err != nil) != c.wantErr {
			t.Errorf("SetOverride(%s=%s) err=%v wantErr=%v", c.name, c.val, err, c.wantErr)
		}
	}
	// 非法 override 不应改变生效值
	if got := GetFloat("combat.momentum"); got != 0.5 {
		t.Fatalf("after rejected overrides GetFloat = %v, want 0.5 (last valid)", got)
	}
}

func TestSnapshotEffective(t *testing.T) {
	resetForTest()
	Register(ParamSpec{Name: "a.x", Namespace: "a", Type: TypeFloat, Default: "1.0"})
	Register(ParamSpec{Name: "b.y", Namespace: "b", Type: TypeInt, Default: "2"})
	_ = SetOverride("a.x", "3.0")

	snap := SnapshotEffective()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	byName := map[string]EffectiveParam{}
	for _, p := range snap {
		byName[p.Name] = p
	}
	if ax := byName["a.x"]; !ax.OverrideSet || ax.Effective != "3.0" || ax.Default != "1.0" {
		t.Fatalf("a.x snapshot = %+v", ax)
	}
	if by := byName["b.y"]; by.OverrideSet || by.Effective != "2" {
		t.Fatalf("b.y snapshot = %+v", by)
	}
	ns := Namespaces()
	if len(ns) != 2 || ns[0] != "a" || ns[1] != "b" {
		t.Fatalf("namespaces = %v", ns)
	}
}

// memStore 是内存 Store 桩，验证落库回灌路径。
type memStore struct{ data map[string]string }

func (m *memStore) Upsert(_ context.Context, name, value string) error {
	m.data[name] = value
	return nil
}
func (m *memStore) Delete(_ context.Context, name string) error {
	delete(m.data, name)
	return nil
}
func (m *memStore) Load(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m.data {
		out[k] = v
	}
	return out, nil
}

func TestLoadFromStoreDropsStaleAndInvalid(t *testing.T) {
	resetForTest()
	// 预置「库里」三条：一条合法、一条已下线参数、一条值已越界（范围后来收紧）。
	ms := &memStore{data: map[string]string{
		"combat.momentum": "0.4",  // 合法
		"removed.param":   "x",    // 已下线（未注册）
		"fate.budget":     "999",  // 越界（注册 Max=10）
	}}
	Register(ParamSpec{Name: "combat.momentum", Type: TypeFloat, Default: "0.85", Min: Ptr(0), Max: Ptr(1)})
	Register(ParamSpec{Name: "fate.budget", Type: TypeInt, Default: "3", Min: Ptr(0), Max: Ptr(10)})
	SetStore(ms)

	n, err := LoadFromStore(context.Background())
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if n != 1 {
		t.Fatalf("loaded count = %d, want 1 (only valid+registered)", n)
	}
	if got := GetFloat("combat.momentum"); got != 0.4 {
		t.Fatalf("valid override not applied: %v", got)
	}
	if got := GetInt("fate.budget"); got != 3 {
		t.Fatalf("invalid override should be dropped, got %v want default 3", got)
	}
}
