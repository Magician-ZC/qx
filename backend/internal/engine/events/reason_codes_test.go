package events

// 文件说明：reason-code 目录完整性测试（events 包首套测试）。
// 守护：无重复 code、importance 区间合法、新登记的大世界生命周期码可被 Lookup（Mutator 经 Lookup 校验，未登记即拒绝）。

import "testing"

func TestCatalogIntegrity(t *testing.T) {
	seen := map[ReasonCode]bool{}
	for _, def := range Catalog() {
		if def.Code == "" {
			t.Fatalf("存在空 code 的目录项: %+v", def)
		}
		if seen[def.Code] {
			t.Fatalf("重复 reason code: %s", def.Code)
		}
		seen[def.Code] = true
		if def.Category == "" {
			t.Fatalf("%s 缺 category", def.Code)
		}
		if def.DisplayName == "" || def.DefaultReasonText == "" {
			t.Fatalf("%s 缺 display_name / default_reason_text", def.Code)
		}
		if def.ImportanceMin < 0 || def.ImportanceMax > 10 || def.ImportanceMin > def.ImportanceMax {
			t.Fatalf("%s importance 区间非法: [%d,%d]", def.Code, def.ImportanceMin, def.ImportanceMax)
		}
		if def.StatDomains == nil {
			t.Fatalf("%s StatDomains 应为非 nil 切片（可空但不可 nil）", def.Code)
		}
	}
}

func TestLifecycleReasonCodesRegistered(t *testing.T) {
	// 大世界生命周期码必须可 Lookup——否则经 Mutator 写保护字段时会被「unknown reason code」拒绝。
	want := []ReasonCode{
		ReasonCharacterBorn, ReasonCharacterDied, ReasonVengeanceFulfilled,
		ReasonFactionCollapse, ReasonPersonalityDrift, ReasonLoyaltyGain, ReasonLoyaltyStrain,
	}
	for _, code := range want {
		def, ok := Lookup(code)
		if !ok {
			t.Fatalf("生命周期 reason code 未登记: %s", code)
		}
		if def.Code != code {
			t.Fatalf("Lookup(%s) 返回了 %s", code, def.Code)
		}
	}
	// 改保护字段的两个 loyalty 码须标注 loyalty 域；陨落须标注 lives_remaining。
	if def, _ := Lookup(ReasonLoyaltyGain); len(def.StatDomains) == 0 || def.StatDomains[0] != "loyalty" {
		t.Fatalf("LOYALTY_GAIN 应标注 loyalty 域，得到 %v", def.StatDomains)
	}
	if def, _ := Lookup(ReasonCharacterDied); len(def.StatDomains) == 0 || def.StatDomains[0] != "lives_remaining" {
		t.Fatalf("CHARACTER_DIED 应标注 lives_remaining 域，得到 %v", def.StatDomains)
	}
}
