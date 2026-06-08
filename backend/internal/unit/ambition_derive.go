package unit

// 文件说明：六维野心向量的确定性派生——据种子与身份线索（出身/传记关键词）生成
// power/vengeance/wealth/lineage/mastery/freedom 六维 [0,1] 引力基线，供建人路径写入 Record.Ambition。
// 用 FNV 哈希保证可复现（同 seed+线索 → 同向量）；身份关键词命中给对应维度加偏置，让出身有倾向而非纯随机。
// 这是 unit.Ambition 在全代码库的写入源——BootstrapRecord 一处调用即覆盖主局捏人/村庄/新生儿等全部建人路径。

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// DeriveAmbition 据 seed 与身份线索确定性派生六维野心向量（各 clamp 到 [0,1]）。
func DeriveAmbition(seed int64, lineage string, biography string) Ambition {
	base := func(salt string) float64 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(fmt.Sprintf("%d|ambition|%s", seed, salt)))
		return float64(h.Sum64()%1000) / 1000.0 // [0,1)
	}
	amb := Ambition{
		Power:     base("power"),
		Vengeance: base("vengeance"),
		Wealth:    base("wealth"),
		Lineage:   base("lineage"),
		Mastery:   base("mastery"),
		Freedom:   base("freedom"),
	}
	// 身份关键词偏置（确定性、clamp[0,1]）：出身/传记里的语义线索把对应维度上抬，
	// 让「将门→power」「孤儿/灭门→vengeance」「商贾→wealth」「世家→lineage」「匠医→mastery」「游侠→freedom」有倾向。
	hay := strings.ToLower(strings.TrimSpace(lineage + " " + biography))
	if hay != "" {
		bump := func(v *float64, keywords ...string) {
			for _, kw := range keywords {
				if kw != "" && strings.Contains(hay, kw) {
					*v = clampAmbition(*v + 0.3)
					return
				}
			}
		}
		bump(&amb.Power, "将", "军", "王", "霸", "统帅")
		bump(&amb.Vengeance, "仇", "恨", "灭门", "复仇", "孤儿")
		bump(&amb.Wealth, "商", "贾", "财", "金", "富")
		bump(&amb.Lineage, "门", "族", "嗣", "血脉", "传家")
		bump(&amb.Mastery, "匠", "医", "学", "艺", "技")
		bump(&amb.Freedom, "侠", "游", "浪", "自由", "野")
	}
	return amb
}

// clampAmbition 把野心维度夹到 [0,1]。
func clampAmbition(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
