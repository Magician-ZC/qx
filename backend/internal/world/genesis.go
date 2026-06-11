package world

// 文件说明：世界名表 + 确定性世界名解析（创世序章模块1）——给每个世界一个有韵味的中文名，
// 用 FNV-64a(worldID) 确定性从名表里选名，使同一 worldID 在任何进程/任何时刻都得到同一个名字
// （与 world_boss 选名/combat_roll 同口径的确定性随机，禁用全局 rand）。
//
// 世界名只作叙事露出（创世序章卷首「<世界名>——史前无名……」），不进任何账本/不参与结算，
// 纯派生物：worldID 在则名稳定，worldID 空回落「无名之境」。

import "hash/fnv"

// worldNameTable 是给世界起名的中文名表（有韵味、苍茫古意，呼应「史前无名、三道并立」的创世基调）。
// 长度取质数 13，使 FNV-64a 取模分布更均匀；选名仅作叙事，名字重复在不同世界间无妨。
var worldNameTable = []string{
	"九野",
	"寰墟",
	"苍寰",
	"玄垠",
	"赤霄",
	"忘川",
	"太衍",
	"裂寰",
	"孤鸣",
	"重渊",
	"幽朔",
	"曦冥",
	"沧鸿",
}

// WorldDisplayName 用 FNV-64a(worldID) 确定性从 worldNameTable 里选一个世界名。
// worldID 为空（旧单图档/未接多世界）回落「无名之境」；非空则同 worldID 恒得同名（可复现、跨进程稳定）。
func WorldDisplayName(worldID string) string {
	if worldID == "" {
		return "无名之境"
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("world-name:" + worldID))
	idx := int(h.Sum64() % uint64(len(worldNameTable)))
	return worldNameTable[idx]
}
