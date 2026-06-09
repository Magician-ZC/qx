package featureflags

// 文件说明：featureflags 运行时开关层的单元测试。
// 覆盖：override 优先于 env、ClearOverride 回落 env、并发安全（race 下读写不崩）、KnownGameplayFlags 含主要游戏 flag。

import (
	"context"
	"os"
	"sync"
	"testing"
)

// resetState 清空进程级 override 表与 store，使每个用例从干净态起跑（包级全局须显式复位）。
func resetState(t *testing.T) {
	t.Helper()
	mu.Lock()
	overrides = map[string]string{}
	store = nil
	mu.Unlock()
}

// TestOverrideTakesPrecedenceOverEnv 验证 override 设置后优先于 os.Getenv。
func TestOverrideTakesPrecedenceOverEnv(t *testing.T) {
	resetState(t)
	const name = "QUNXIANG_DUNGEON"
	t.Setenv(name, "false") // env 底值=关

	// 未设 override 时严格等价 os.Getenv。
	if got := EnvOrOverride(name); got != "false" {
		t.Fatalf("未设 override 时应回落 env，期望 false 得 %q", got)
	}

	// 设 override=true 后优先返回 override。
	if err := SetOverride(name, "true"); err != nil {
		t.Fatalf("SetOverride 失败: %v", err)
	}
	if got := EnvOrOverride(name); got != "true" {
		t.Fatalf("override 应优先于 env，期望 true 得 %q", got)
	}
}

// TestClearOverrideFallsBackToEnv 验证 ClearOverride 后回落 os.Getenv。
func TestClearOverrideFallsBackToEnv(t *testing.T) {
	resetState(t)
	const name = "QUNXIANG_AUTO_PVE"
	t.Setenv(name, "on")

	if err := SetOverride(name, "off"); err != nil {
		t.Fatalf("SetOverride 失败: %v", err)
	}
	if got := EnvOrOverride(name); got != "off" {
		t.Fatalf("override 应生效，期望 off 得 %q", got)
	}

	existed, err := ClearOverride(name)
	if err != nil {
		t.Fatalf("ClearOverride 失败: %v", err)
	}
	if !existed {
		t.Fatalf("ClearOverride 应报告 override 本来存在")
	}
	if got := EnvOrOverride(name); got != "on" {
		t.Fatalf("ClearOverride 后应回落 env，期望 on 得 %q", got)
	}

	// 再次 Clear 不存在的 override：existed=false、不报错。
	existed, err = ClearOverride(name)
	if err != nil || existed {
		t.Fatalf("二次 ClearOverride 应 existed=false err=nil，得 existed=%v err=%v", existed, err)
	}
}

// TestEnvOrOverrideCaseInsensitiveName 验证 name 大小写/空白归一（override 用大写键存取）。
func TestEnvOrOverrideCaseInsensitiveName(t *testing.T) {
	resetState(t)
	if err := SetOverride("  qunxiang_serendipity ", "true"); err != nil {
		t.Fatalf("SetOverride 失败: %v", err)
	}
	if got := EnvOrOverride("QUNXIANG_SERENDIPITY"); got != "true" {
		t.Fatalf("name 归一后应命中，期望 true 得 %q", got)
	}
}

// TestUnsetEnvAndNoOverride 验证既无 override 又无 env 时返回空串（等价裸 os.Getenv 缺省）。
func TestUnsetEnvAndNoOverride(t *testing.T) {
	resetState(t)
	const name = "QUNXIANG_DUNGEON"
	os.Unsetenv(name)
	if got := EnvOrOverride(name); got != "" {
		t.Fatalf("无 override 无 env 应返回空串，得 %q", got)
	}
}

// TestKnownGameplayFlagsContainsMainFlags 验证白名单含主要游戏 flag，且不含被刻意排除的 DB/合规/启动期 flag。
func TestKnownGameplayFlagsContainsMainFlags(t *testing.T) {
	resetState(t)
	flags := KnownGameplayFlags()
	if len(flags) == 0 {
		t.Fatalf("KnownGameplayFlags 不应为空")
	}
	present := map[string]FlagSpec{}
	for _, f := range flags {
		present[f.Name] = f
	}

	mustHave := []string{
		"QUNXIANG_DUNGEON", "QUNXIANG_AUTO_PVE", "QUNXIANG_WORLD_BOSS_AUTO",
		"QUNXIANG_SERENDIPITY", "QUNXIANG_WORLDIZE_INBOUND", "QUNXIANG_AUTO_MATCH", "QUNXIANG_AUTO_SOCIAL",
		"QUNXIANG_ZEROSUM_CONTEST", "QUNXIANG_BLOOD_FEUD", "QUNXIANG_FREEZE_LIST", "QUNXIANG_CONSISTENCY_TIGHTEN",
		"QUNXIANG_COURAGE_CURVE", "QUNXIANG_AMBITION_SCORING", "QUNXIANG_MAIN_VILLAGE", "QUNXIANG_WORLD_BINDING",
		// 三阵营开放世界 F2/F3：阵营切换 + 阵营冲突遭遇（均默认关，GM 后台可运行时开关）。
		"QUNXIANG_FACTION_SWITCH", "QUNXIANG_FACTION_PVE",
	}
	for _, name := range mustHave {
		if _, ok := present[name]; !ok {
			t.Errorf("白名单应含游戏 flag %s", name)
		}
		if !IsKnownGameplayFlag(name) {
			t.Errorf("IsKnownGameplayFlag(%s) 应为 true", name)
		}
	}

	// 被刻意排除的非游戏 flag 绝不进白名单（防误改 DB/合规/启动期/商业开关）。
	mustNotHave := []string{
		"QUNXIANG_CONTENT_SAFETY", "QUNXIANG_BILLING_ENABLED", "QUNXIANG_OPS_TOKEN",
		"QUNXIANG_REGION_RUNNER_ENABLED", "QUNXIANG_REGION_SHARDING", "QUNXIANG_DB_DRIVER",
	}
	for _, name := range mustNotHave {
		if IsKnownGameplayFlag(name) {
			t.Errorf("非游戏 flag %s 不应进可运营白名单", name)
		}
	}

	// 默认开的 flag 默认态标对（与各调用点 switch 的默认分支一致）。
	defaultOn := []string{"QUNXIANG_AUTO_SOCIAL", "QUNXIANG_ZEROSUM_CONTEST", "QUNXIANG_BLOOD_FEUD", "QUNXIANG_MAIN_VILLAGE"}
	for _, name := range defaultOn {
		if !present[name].DefaultOn {
			t.Errorf("%s 应标 DefaultOn=true", name)
		}
	}

	// 多档字符串型 world_binding 应带 Values。
	if len(present["QUNXIANG_WORLD_BINDING"].Values) == 0 {
		t.Errorf("QUNXIANG_WORLD_BINDING 应是多档字符串型（带 Values）")
	}

	// 阵营切换 / 阵营冲突默认关（零行为，需 GM 显式开启）。
	defaultOff := []string{"QUNXIANG_FACTION_SWITCH", "QUNXIANG_FACTION_PVE"}
	for _, name := range defaultOff {
		if present[name].DefaultOn {
			t.Errorf("%s 应默认关（DefaultOn=false）", name)
		}
	}
}

// TestSnapshotEffectiveReflectsOverride 验证 SnapshotEffective 反映 override 态与生效值。
func TestSnapshotEffectiveReflectsOverride(t *testing.T) {
	resetState(t)
	t.Setenv("QUNXIANG_DUNGEON", "false")
	if err := SetOverride("QUNXIANG_DUNGEON", "true"); err != nil {
		t.Fatalf("SetOverride 失败: %v", err)
	}
	snap := SnapshotEffective()
	var found bool
	for _, ef := range snap {
		if ef.Name == "QUNXIANG_DUNGEON" {
			found = true
			if !ef.OverrideSet || ef.OverrideValue != "true" {
				t.Errorf("快照应标 override=true，得 set=%v value=%q", ef.OverrideSet, ef.OverrideValue)
			}
			if ef.EnvValue != "false" {
				t.Errorf("快照应保留 env 底值 false，得 %q", ef.EnvValue)
			}
			if ef.Effective != "true" {
				t.Errorf("生效值应为 override 的 true，得 %q", ef.Effective)
			}
		}
	}
	if !found {
		t.Fatalf("快照应含 QUNXIANG_DUNGEON")
	}
}

// fakeStore 是测试用内存 Store，验证 SetStore/SetOverride/ClearOverride 的持久化往返与 LoadFromStore 回灌。
type fakeStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[string]string{}} }

func (f *fakeStore) Upsert(_ context.Context, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[name] = value
	return nil
}
func (f *fakeStore) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, name)
	return nil
}
func (f *fakeStore) Load(_ context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.data {
		out[k] = v
	}
	return out, nil
}

// TestStorePersistenceRoundTrip 验证注入 Store 后 SetOverride/ClearOverride 同步落库，且 LoadFromStore 回灌。
func TestStorePersistenceRoundTrip(t *testing.T) {
	resetState(t)
	fs := newFakeStore()
	SetStore(fs)

	if err := SetOverride("QUNXIANG_DUNGEON", "true"); err != nil {
		t.Fatalf("SetOverride 失败: %v", err)
	}
	if fs.data["QUNXIANG_DUNGEON"] != "true" {
		t.Fatalf("SetOverride 应落库，得 %q", fs.data["QUNXIANG_DUNGEON"])
	}

	// 模拟重启：清空内存 override，再从 store 回灌。
	mu.Lock()
	overrides = map[string]string{}
	mu.Unlock()
	n, err := LoadFromStore(context.Background())
	if err != nil {
		t.Fatalf("LoadFromStore 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应回灌 1 条 override，得 %d", n)
	}
	if got := EnvOrOverride("QUNXIANG_DUNGEON"); got != "true" {
		t.Fatalf("回灌后 override 应生效，期望 true 得 %q", got)
	}

	// ClearOverride 同步删库。
	if _, err := ClearOverride("QUNXIANG_DUNGEON"); err != nil {
		t.Fatalf("ClearOverride 失败: %v", err)
	}
	if _, ok := fs.data["QUNXIANG_DUNGEON"]; ok {
		t.Fatalf("ClearOverride 应删库")
	}
}

// TestConcurrentAccessIsRaceFree 在 -race 下并发读写 override，验证不崩、不竞态。
func TestConcurrentAccessIsRaceFree(t *testing.T) {
	resetState(t)
	const name = "QUNXIANG_BLOOD_FEUD"
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = SetOverride(name, "true") }()
		go func() { defer wg.Done(); _ = EnvOrOverride(name) }()
		go func() { defer wg.Done(); _, _ = ClearOverride(name) }()
	}
	// 同时并发读快照/清单，确保读侧也加锁。
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = ListOverrides() }()
		go func() { defer wg.Done(); _ = SnapshotEffective() }()
	}
	wg.Wait()
}
