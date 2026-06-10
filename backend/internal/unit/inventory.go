package unit

// 文件说明：单位背包与装备系统实现，处理增删换装、堆叠规则、负重惩罚与派生属性重算。

import (
	"fmt"

	"qunxiang/backend/internal/item"
)

const BackpackCapacity = 6

// RecalculateDerivedStats 按装备和负重重新计算单位派生战斗属性。
func RecalculateDerivedStats(record *Record) {
	baseAttack := record.Stats.Derived.Attack
	if baseAttack <= 0 {
		baseAttack = 10
	}
	baseDefense := record.Stats.Derived.Defense
	if baseDefense <= 0 {
		baseDefense = 5
	}
	baseMove := movementFromStats(record.Stats)

	record.Status.Attack = baseAttack
	record.Status.Defense = baseDefense
	record.Status.Move = baseMove

	for _, stack := range record.Inventory.Equipment {
		definition, ok := item.Lookup(stack.ItemID)
		if !ok {
			continue
		}
		record.Status.Attack += definition.AttackBonus
		record.Status.Defense += definition.DefenseBonus
		record.Status.Move += definition.MoveBonus
		attackBonus, defenseBonus, moveBonus := upgradeBonuses(stack, definition)
		record.Status.Attack += attackBonus
		record.Status.Defense += defenseBonus
		record.Status.Move += moveBonus
	}

	if len(record.Inventory.Backpack) >= BackpackCapacity {
		record.Status.Move--
	}

	if hasHeavyLoad(record.Inventory) {
		record.Status.Move--
	}

	if record.Status.Move < 0 {
		record.Status.Move = 0
	}
}

// upgradeBonuses 根据装备强化等级提供额外攻防/移动加成。
func upgradeBonuses(stack ItemStack, definition item.Definition) (int, int, int) {
	if stack.Level <= 0 || definition.Slot == "" {
		return 0, 0, 0
	}
	switch definition.Slot {
	case item.SlotWeapon:
		return stack.Level * 4, stack.Level, 0
	case item.SlotArmor:
		return 0, stack.Level * 3, 0
	case item.SlotShoes:
		return 0, stack.Level, stack.Level / 2
	case item.SlotAccessory:
		return stack.Level, stack.Level * 2, 0
	default:
		return 0, 0, 0
	}
}

// movementFromStats 根据开局主属性/派生闪避推导基础移动力，缺省时保持旧版 4 点移动。
func movementFromStats(stats Stats) int {
	move := 4
	if stats.Primary.Dexterity >= 14 || stats.Derived.Evasion >= 10 {
		move = 5
	} else if stats.Primary.Dexterity <= 7 && stats.Primary.Constitution <= 8 {
		move = 3
	}
	return move
}

// AddItem 添加物品；装备类会自动进入装备栏并触发属性重算。
func AddItem(record *Record, itemID string, quantity int) error {
	return AddNamedItem(record, itemID, quantity, "")
}

// AddNamedItem 添加物品；装备类可带自定义名称并会自动进入装备栏。
func AddNamedItem(record *Record, itemID string, quantity int, customName string) error {
	if quantity <= 0 {
		return fmt.Errorf("quantity must be positive")
	}

	definition, ok := item.Lookup(itemID)
	if !ok {
		return fmt.Errorf("unknown item %s", itemID)
	}

	if definition.Slot != "" {
		if err := equipStack(record, ItemStack{ItemID: itemID, Quantity: 1, CustomName: customName}); err != nil {
			return err
		}
		RecalculateDerivedStats(record)
		return nil
	}

	if err := addBackpackStack(&record.Inventory, itemID, quantity); err != nil {
		return err
	}
	RecalculateDerivedStats(record)
	return nil
}

// EquipBackpackItem 将背包中的装备穿戴到对应装备栏，原装备会回到背包。
func EquipBackpackItem(record *Record, itemID string) error {
	definition, ok := item.Lookup(itemID)
	if !ok {
		return fmt.Errorf("unknown item %s", itemID)
	}
	if definition.Slot == "" {
		return fmt.Errorf("item %s is not equipment", itemID)
	}
	for index, stack := range record.Inventory.Backpack {
		if stack.ItemID != itemID {
			continue
		}
		record.Inventory.Backpack = append(record.Inventory.Backpack[:index], record.Inventory.Backpack[index+1:]...)
		if err := equipStack(record, stack); err != nil {
			record.Inventory.Backpack = append(record.Inventory.Backpack, stack)
			return err
		}
		RecalculateDerivedStats(record)
		return nil
	}
	return fmt.Errorf("equipment %s not found in backpack", itemID)
}

// UnequipBackpackItem 将指定槽位的装备卸下放回背包（EquipBackpackItem 的逆操作），并重算派生攻防。
// 关键顺序：先回包成功才清槽位——背包满时装备保持穿戴原样、绝不凭空消失；回包保留自定义名称与强化等级
//（走 addBackpackItemStack，与换装时旧装备回包同一路径）。返回被卸下的装备堆栈。
func UnequipBackpackItem(record *Record, slot string) (ItemStack, error) {
	if record == nil {
		return ItemStack{}, fmt.Errorf("nil record")
	}
	stack, occupied := record.Inventory.Equipment[slot]
	if !occupied || stack.ItemID == "" {
		return ItemStack{}, fmt.Errorf("slot %s is empty", slot)
	}
	if err := addBackpackItemStack(&record.Inventory, stack); err != nil {
		return ItemStack{}, err
	}
	delete(record.Inventory.Equipment, slot)
	RecalculateDerivedStats(record)
	return stack, nil
}

// UpgradeItem 强化一件已拥有装备；材料校验由会话生产层负责。
func UpgradeItem(record *Record, itemID string) (ItemStack, error) {
	for slot, stack := range record.Inventory.Equipment {
		if stack.ItemID != itemID {
			continue
		}
		if _, ok := item.Lookup(stack.ItemID); !ok {
			return ItemStack{}, fmt.Errorf("unknown item %s", itemID)
		}
		stack.Level++
		record.Inventory.Equipment[slot] = stack
		RecalculateDerivedStats(record)
		return stack, nil
	}
	for index, stack := range record.Inventory.Backpack {
		if stack.ItemID != itemID {
			continue
		}
		definition, ok := item.Lookup(stack.ItemID)
		if !ok {
			return ItemStack{}, fmt.Errorf("unknown item %s", itemID)
		}
		if definition.Slot == "" {
			return ItemStack{}, fmt.Errorf("item %s is not equipment", itemID)
		}
		stack.Level++
		record.Inventory.Backpack[index] = stack
		RecalculateDerivedStats(record)
		return stack, nil
	}
	return ItemStack{}, fmt.Errorf("equipment %s not found", itemID)
}

// AddBackpackItem 仅向背包添加物品并触发属性重算。
func AddBackpackItem(record *Record, itemID string, quantity int) error {
	if err := addBackpackStack(&record.Inventory, itemID, quantity); err != nil {
		return err
	}
	RecalculateDerivedStats(record)
	return nil
}

// RemoveItem 从装备或背包移除指定物品。
func RemoveItem(record *Record, itemID string) error {
	_, err := TakeItem(record, itemID)
	if err != nil {
		return err
	}
	RecalculateDerivedStats(record)
	return nil
}

// ConsumeBackpackItem 消耗背包内指定数量的物品。
func ConsumeBackpackItem(record *Record, itemID string, quantity int) error {
	if quantity <= 0 {
		return fmt.Errorf("quantity must be positive")
	}

	for index, stack := range record.Inventory.Backpack {
		if stack.ItemID != itemID {
			continue
		}
		if stack.Quantity < quantity {
			return fmt.Errorf("item %s quantity %d is smaller than requested %d", itemID, stack.Quantity, quantity)
		}

		stack.Quantity -= quantity
		if stack.Quantity == 0 {
			record.Inventory.Backpack = append(record.Inventory.Backpack[:index], record.Inventory.Backpack[index+1:]...)
		} else {
			record.Inventory.Backpack[index] = stack
		}
		RecalculateDerivedStats(record)
		return nil
	}

	return fmt.Errorf("item %s not found in backpack", itemID)
}

// TakeItem 取出一件物品并返回对应堆栈信息。
func TakeItem(record *Record, itemID string) (ItemStack, error) {
	for slot, stack := range record.Inventory.Equipment {
		if stack.ItemID == itemID {
			delete(record.Inventory.Equipment, slot)
			return stack, nil
		}
	}

	for index, stack := range record.Inventory.Backpack {
		if stack.ItemID == itemID {
			record.Inventory.Backpack = append(record.Inventory.Backpack[:index], record.Inventory.Backpack[index+1:]...)
			return stack, nil
		}
	}

	return ItemStack{}, fmt.Errorf("item %s not found", itemID)
}

// addBackpackStack 按堆叠规则向背包写入物品。
func addBackpackStack(inventory *Inventory, itemID string, quantity int) error {
	definition, ok := item.Lookup(itemID)
	if !ok {
		return fmt.Errorf("unknown item %s", itemID)
	}

	if definition.Stackable {
		for index := range inventory.Backpack {
			if inventory.Backpack[index].ItemID != itemID {
				continue
			}
			if inventory.Backpack[index].Quantity+quantity > definition.MaxStack {
				break
			}
			inventory.Backpack[index].Quantity += quantity
			return nil
		}
	}

	if len(inventory.Backpack) >= BackpackCapacity {
		return fmt.Errorf("backpack is full")
	}

	if !definition.Stackable {
		quantity = 1
	}
	inventory.Backpack = append(inventory.Backpack, ItemStack{ItemID: itemID, Quantity: quantity})
	return nil
}

// addBackpackItemStack 将完整物品堆栈放入背包，保留装备自定义名称与强化等级。
func addBackpackItemStack(inventory *Inventory, stack ItemStack) error {
	definition, ok := item.Lookup(stack.ItemID)
	if !ok {
		return fmt.Errorf("unknown item %s", stack.ItemID)
	}
	if stack.Quantity <= 0 {
		stack.Quantity = 1
	}
	if definition.Stackable && stack.CustomName == "" && stack.Level == 0 {
		return addBackpackStack(inventory, stack.ItemID, stack.Quantity)
	}
	if len(inventory.Backpack) >= BackpackCapacity {
		return fmt.Errorf("backpack is full")
	}
	if !definition.Stackable {
		stack.Quantity = 1
	}
	inventory.Backpack = append(inventory.Backpack, stack)
	return nil
}

// equipStack 将完整装备堆栈穿戴到对应槽位，原装备完整回包。
func equipStack(record *Record, stack ItemStack) error {
	definition, ok := item.Lookup(stack.ItemID)
	if !ok {
		return fmt.Errorf("unknown item %s", stack.ItemID)
	}
	if definition.Slot == "" {
		return fmt.Errorf("item %s is not equipment", stack.ItemID)
	}
	if record.Inventory.Equipment == nil {
		record.Inventory.Equipment = map[string]ItemStack{}
	}
	stack.Quantity = 1
	slotKey := string(definition.Slot)
	current, occupied := record.Inventory.Equipment[slotKey]
	if occupied && current.ItemID != "" {
		if err := addBackpackItemStack(&record.Inventory, current); err != nil {
			return err
		}
	}
	record.Inventory.Equipment[slotKey] = stack
	return nil
}

// hasHeavyLoad 判断背包/装备是否存在重载物品。
func hasHeavyLoad(inventory Inventory) bool {
	for _, stack := range inventory.Equipment {
		definition, ok := item.Lookup(stack.ItemID)
		if ok && definition.Weight >= 3 {
			return true
		}
	}
	for _, stack := range inventory.Backpack {
		definition, ok := item.Lookup(stack.ItemID)
		if ok && definition.Weight >= 3 {
			return true
		}
	}
	return false
}

// HexDistance 计算六边形坐标系下两点距离。
func HexDistance(leftQ int, leftR int, rightQ int, rightR int) int {
	dq := leftQ - rightQ
	dr := leftR - rightR
	ds := (leftQ + leftR) - (rightQ + rightR)
	return max(absInt(dq), absInt(dr), absInt(ds))
}

// absInt 返回整数绝对值。
func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
