package item

// 文件说明：静态物品目录定义，包含装备/消耗品/工具/材料属性及按 ID 查询能力。

type Slot string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	SlotWeapon    Slot = "weapon"
	SlotArmor     Slot = "armor"
	SlotShoes     Slot = "shoes"
	SlotAccessory Slot = "accessory"
)

// Category 类型定义用于统一该模块的数据表达。
type Category string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	CategoryEquipment  Category = "equipment"
	CategoryConsumable Category = "consumable"
	CategoryTool       Category = "tool"
	CategoryMaterial   Category = "material"
)

// Definition 结构体用于承载该模块的核心数据。
type Definition struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name"`
	Category     Category `json:"category"`
	Slot         Slot     `json:"slot,omitempty"`
	Price        int      `json:"price"`
	AttackBonus  int      `json:"attack_bonus"`
	DefenseBonus int      `json:"defense_bonus"`
	MoveBonus    int      `json:"move_bonus"`
	Weight       int      `json:"weight"`
	Stackable    bool     `json:"stackable"`
	MaxStack     int      `json:"max_stack"`
	Tags         []string `json:"tags"`
}

// Catalog 返回全量物品定义目录（静态配置）。
func Catalog() []Definition {
	return []Definition{
		{ID: "dagger", DisplayName: "匕首", Category: CategoryEquipment, Slot: SlotWeapon, Price: 35, AttackBonus: 8, Weight: 1, Tags: []string{"weapon", "blade", "light"}},
		{ID: "short_sword", DisplayName: "短剑", Category: CategoryEquipment, Slot: SlotWeapon, Price: 60, AttackBonus: 15, Weight: 1, Tags: []string{"weapon"}},
		{ID: "long_sword", DisplayName: "长剑", Category: CategoryEquipment, Slot: SlotWeapon, Price: 90, AttackBonus: 25, Weight: 2, Tags: []string{"weapon"}},
		{ID: "greatsword", DisplayName: "巨剑", Category: CategoryEquipment, Slot: SlotWeapon, Price: 125, AttackBonus: 33, Weight: 3, Tags: []string{"weapon", "heavy", "blade"}},
		{ID: "spear", DisplayName: "长矛", Category: CategoryEquipment, Slot: SlotWeapon, Price: 78, AttackBonus: 19, Weight: 2, Tags: []string{"weapon", "polearm"}},
		{ID: "bow", DisplayName: "弓箭", Category: CategoryEquipment, Slot: SlotWeapon, Price: 85, AttackBonus: 20, Weight: 1, Tags: []string{"weapon", "ranged"}},
		{ID: "crossbow", DisplayName: "弩", Category: CategoryEquipment, Slot: SlotWeapon, Price: 105, AttackBonus: 24, Weight: 2, Tags: []string{"weapon", "ranged", "crossbow"}},
		{ID: "battle_axe", DisplayName: "战斧", Category: CategoryEquipment, Slot: SlotWeapon, Price: 102, AttackBonus: 27, Weight: 3, Tags: []string{"weapon", "heavy", "axe"}},
		{ID: "warhammer", DisplayName: "战锤", Category: CategoryEquipment, Slot: SlotWeapon, Price: 110, AttackBonus: 30, Weight: 3, Tags: []string{"weapon", "heavy"}},
		{ID: "oak_staff", DisplayName: "橡木法杖", Category: CategoryEquipment, Slot: SlotWeapon, Price: 95, AttackBonus: 17, DefenseBonus: 4, Weight: 2, Tags: []string{"weapon", "focus", "arcane"}},
		{ID: "cloth_armor", DisplayName: "布甲", Category: CategoryEquipment, Slot: SlotArmor, Price: 40, DefenseBonus: 5, Weight: 1, Tags: []string{"armor"}},
		{ID: "padded_armor", DisplayName: "棉甲", Category: CategoryEquipment, Slot: SlotArmor, Price: 56, DefenseBonus: 8, Weight: 1, Tags: []string{"armor", "light"}},
		{ID: "leather_armor", DisplayName: "皮甲", Category: CategoryEquipment, Slot: SlotArmor, Price: 70, DefenseBonus: 12, Weight: 2, Tags: []string{"armor"}},
		{ID: "chain_mail", DisplayName: "锁甲", Category: CategoryEquipment, Slot: SlotArmor, Price: 98, DefenseBonus: 20, MoveBonus: -1, Weight: 3, Tags: []string{"armor", "medium"}},
		{ID: "brigandine", DisplayName: "札甲", Category: CategoryEquipment, Slot: SlotArmor, Price: 112, DefenseBonus: 24, MoveBonus: -1, Weight: 3, Tags: []string{"armor", "medium"}},
		{ID: "plate_armor", DisplayName: "板甲", Category: CategoryEquipment, Slot: SlotArmor, Price: 130, DefenseBonus: 35, MoveBonus: -1, Weight: 4, Tags: []string{"armor", "heavy"}},
		{ID: "mage_robe", DisplayName: "法袍", Category: CategoryEquipment, Slot: SlotArmor, Price: 88, DefenseBonus: 10, MoveBonus: 1, Weight: 1, Tags: []string{"armor", "arcane", "light"}},
		{ID: "cloth_shoes", DisplayName: "布鞋", Category: CategoryEquipment, Slot: SlotShoes, Price: 20, MoveBonus: 1, Weight: 0, Tags: []string{"shoes"}},
		{ID: "leather_boots", DisplayName: "皮靴", Category: CategoryEquipment, Slot: SlotShoes, Price: 45, MoveBonus: 2, Weight: 1, Tags: []string{"shoes"}},
		{ID: "war_boots", DisplayName: "战靴", Category: CategoryEquipment, Slot: SlotShoes, Price: 65, MoveBonus: 3, Weight: 1, Tags: []string{"shoes"}},
		{ID: "riding_spurs", DisplayName: "骑乘马刺", Category: CategoryEquipment, Slot: SlotShoes, Price: 92, MoveBonus: 4, Weight: 1, Tags: []string{"shoes", "mobility"}},
		{ID: "buckler", DisplayName: "圆盾", Category: CategoryEquipment, Slot: SlotAccessory, Price: 40, DefenseBonus: 5, Weight: 1, Tags: []string{"accessory", "shield"}},
		{ID: "kite_shield", DisplayName: "盾牌", Category: CategoryEquipment, Slot: SlotAccessory, Price: 55, DefenseBonus: 8, Weight: 2, Tags: []string{"accessory", "shield"}},
		{ID: "tower_shield", DisplayName: "塔盾", Category: CategoryEquipment, Slot: SlotAccessory, Price: 88, DefenseBonus: 14, MoveBonus: -1, Weight: 3, Tags: []string{"accessory", "shield", "heavy"}},
		{ID: "scout_charm", DisplayName: "侦察护符", Category: CategoryEquipment, Slot: SlotAccessory, Price: 75, MoveBonus: 1, Weight: 0, Tags: []string{"accessory"}},
		{ID: "arcane_focus", DisplayName: "奥术焦点", Category: CategoryEquipment, Slot: SlotAccessory, Price: 98, AttackBonus: 7, Weight: 0, Tags: []string{"accessory", "arcane", "focus"}},
		{ID: "healer_emblem", DisplayName: "疗愈徽章", Category: CategoryEquipment, Slot: SlotAccessory, Price: 86, DefenseBonus: 6, Weight: 0, Tags: []string{"accessory", "healing"}},
		{ID: "ration", DisplayName: "口粮", Category: CategoryConsumable, Price: 8, Stackable: true, MaxStack: 10, Tags: []string{"food"}},
		{ID: "herb_bundle", DisplayName: "药草包", Category: CategoryConsumable, Price: 12, Stackable: true, MaxStack: 20, Tags: []string{"medicine"}},
		{ID: "healing_potion", DisplayName: "治疗药剂", Category: CategoryConsumable, Price: 24, Stackable: true, MaxStack: 8, Tags: []string{"medicine", "healing"}},
		{ID: "antidote", DisplayName: "解毒药", Category: CategoryConsumable, Price: 22, Stackable: true, MaxStack: 8, Tags: []string{"medicine", "detox"}},
		{ID: "revive_stone", DisplayName: "复活石", Category: CategoryConsumable, Price: 120, Stackable: true, MaxStack: 2, Tags: []string{"revive", "rare"}},
		{ID: "rope", DisplayName: "绳索", Category: CategoryTool, Price: 15, Tags: []string{"tool"}},
		{ID: "pickaxe", DisplayName: "铁镐", Category: CategoryTool, Price: 22, Weight: 1, Tags: []string{"tool"}},
		{ID: "fishing_net", DisplayName: "渔网", Category: CategoryTool, Price: 18, Weight: 1, Tags: []string{"tool"}},
		{ID: "hatchet", DisplayName: "斧头", Category: CategoryTool, Price: 20, Weight: 1, Tags: []string{"tool"}},
		{ID: "torch", DisplayName: "火把", Category: CategoryTool, Price: 10, Stackable: true, MaxStack: 6, Tags: []string{"tool", "light"}},
		{ID: "carrier_pigeon", DisplayName: "信鸽", Category: CategoryTool, Price: 30, Tags: []string{"tool", "message"}},
		{ID: "iron_ore", DisplayName: "铁矿", Category: CategoryMaterial, Price: 18, Stackable: true, MaxStack: 10, Weight: 1, Tags: []string{"material"}},
		{ID: "wood", DisplayName: "木材", Category: CategoryMaterial, Price: 10, Stackable: true, MaxStack: 12, Weight: 1, Tags: []string{"material", "wood"}},
		{ID: "stone", DisplayName: "石料", Category: CategoryMaterial, Price: 9, Stackable: true, MaxStack: 12, Weight: 1, Tags: []string{"material", "stone"}},
		{ID: "leather", DisplayName: "皮革", Category: CategoryMaterial, Price: 14, Stackable: true, MaxStack: 10, Weight: 1, Tags: []string{"material", "leather"}},
		{ID: "cloth_roll", DisplayName: "布匹", Category: CategoryMaterial, Price: 11, Stackable: true, MaxStack: 12, Weight: 1, Tags: []string{"material", "cloth"}},
		{ID: "gemstone", DisplayName: "宝石", Category: CategoryMaterial, Price: 44, Stackable: true, MaxStack: 6, Weight: 1, Tags: []string{"material", "gem"}},
	}
}

// Lookup 按物品 ID 查询物品定义。
func Lookup(itemID string) (Definition, bool) {
	for _, definition := range Catalog() {
		if definition.ID == itemID {
			return definition, true
		}
	}
	return Definition{}, false
}
