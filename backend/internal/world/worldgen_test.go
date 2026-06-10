package world

import "testing"

// TestGenerateWorldZoneContent 校验阶段2 §3/§4：每个 capital/wild 区都有 boss（坐标/名/等级），
// neutral/starter 区无 boss；只有 capital 区标了副本入口。同时校验确定性（同 seed 同内容）。
func TestGenerateWorldZoneContent(t *testing.T) {
	zones := GenerateWorld(42)
	if len(zones) == 0 {
		t.Fatal("世界应生成至少一个区域")
	}
	byID := map[string]Zone{}
	for _, z := range zones {
		byID[z.ID] = z
	}

	for _, z := range zones {
		switch z.Kind {
		case ZoneCapital, ZoneWild:
			if z.BossCoord == "" {
				t.Errorf("区域 %s(%s) 应有 boss 坐标", z.ID, z.Kind)
			}
			if z.BossName == "" {
				t.Errorf("区域 %s(%s) 应有 boss 名", z.ID, z.Kind)
			}
			if z.BossLevel != z.LevelMax {
				t.Errorf("区域 %s boss 等级应=LevelMax(%d)，得到 %d", z.ID, z.LevelMax, z.BossLevel)
			}
		case ZoneStarter:
			if z.BossCoord != "" || z.BossName != "" || z.BossLevel != 0 {
				t.Errorf("新手区 %s 不应有 boss，得到 coord=%q name=%q level=%d", z.ID, z.BossCoord, z.BossName, z.BossLevel)
			}
		}
		// 副本入口仅 capital 区有（且有城镇时）。
		if z.Kind != ZoneCapital && z.DungeonCoord != "" {
			t.Errorf("非主城区 %s 不应有副本入口，得到 %q", z.ID, z.DungeonCoord)
		}
	}

	// 至少有一个 capital 区标了副本入口（默认布局三主城各有城镇）。
	dungeonZones := 0
	for _, z := range zones {
		if z.Kind == ZoneCapital && z.DungeonCoord != "" {
			dungeonZones++
		}
	}
	if dungeonZones == 0 {
		t.Error("应至少有一个主城区标了副本入口")
	}

	// 确定性：同 seed 重新生成，boss 坐标/名/等级与副本入口逐字段一致。
	again := GenerateWorld(42)
	if len(again) != len(zones) {
		t.Fatalf("同 seed 区域数应一致：%d vs %d", len(again), len(zones))
	}
	for i := range zones {
		a, b := zones[i], again[i]
		if a.BossCoord != b.BossCoord || a.BossName != b.BossName || a.BossLevel != b.BossLevel || a.DungeonCoord != b.DungeonCoord {
			t.Errorf("同 seed 区域 %s 内容应一致：%+v vs %+v", a.ID, a, b)
		}
	}
}
