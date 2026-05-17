package session

// 文件说明：生成六边形战场快照并提供坐标边界、邻接与键编码等基础几何工具。

import (
	"fmt"
	"math/rand"
	"time"

	"qunxiang/backend/internal/world"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	defaultBattlefieldWidth  = 9
	defaultBattlefieldHeight = 7
	maxLogEntries            = 96
)

const (
	BattlefieldSizeSmall  = "small"
	BattlefieldSizeMedium = "medium"
	BattlefieldSizeLarge  = "large"
)

// BattlefieldSize 结构体用于承载战场尺寸选项。
type BattlefieldSize struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Summary     string `json:"summary"`
}

var battlefieldSizeCatalog = []BattlefieldSize{
	{ID: BattlefieldSizeSmall, DisplayName: "小", Width: 9, Height: 7, Summary: "短兵相接，横向展开更快。"},
	{ID: BattlefieldSizeMedium, DisplayName: "中", Width: 13, Height: 9, Summary: "标准长方形战场，兼顾遭遇和迂回。"},
	{ID: BattlefieldSizeLarge, DisplayName: "大", Width: 17, Height: 11, Summary: "横向空间更大，适合侦察、侧翼和消耗。"},
}

// BattlefieldSizes 返回可选战场尺寸目录。
func BattlefieldSizes() []BattlefieldSize {
	result := make([]BattlefieldSize, 0, len(battlefieldSizeCatalog))
	result = append(result, battlefieldSizeCatalog...)
	return result
}

// NormalizeBattlefieldSizeID 规范化战场尺寸 ID，默认沿用旧版 7x7 小地图以保证兼容。
func NormalizeBattlefieldSizeID(sizeID string) string {
	switch sizeID {
	case BattlefieldSizeSmall, BattlefieldSizeMedium, BattlefieldSizeLarge:
		return sizeID
	default:
		return BattlefieldSizeSmall
	}
}

func battlefieldSizeByID(sizeID string) BattlefieldSize {
	sizeID = NormalizeBattlefieldSizeID(sizeID)
	for _, size := range battlefieldSizeCatalog {
		if size.ID == sizeID {
			return size
		}
	}
	return BattlefieldSize{ID: BattlefieldSizeSmall, DisplayName: "小", Width: defaultBattlefieldWidth, Height: defaultBattlefieldHeight}
}

// generateBattlefield 生成一张固定尺寸的六边形战场快照。
// 地形由分层噪声管线生成，脚本 ID 仅作为战场主题偏置而非固定模板。
func generateBattlefield(sessionID string, seed int64, scriptID string) world.MapSnapshot {
	return generateBattlefieldWithSize(sessionID, seed, scriptID, BattlefieldSizeSmall)
}

func generateBattlefieldWithSize(sessionID string, seed int64, scriptID string, sizeID string) world.MapSnapshot {
	scriptID = normalizeBattlefieldScriptID(scriptID, seed)
	size := battlefieldSizeByID(sizeID)
	rng := rand.New(rand.NewSource(seed + int64(len(scriptID))*7919))
	snapshot := world.GenerateMap(seed, size.Width, size.Height)
	snapshot.ID = fmt.Sprintf("battlefield-%s", sessionID)
	snapshot.GeneratedAt = time.Now().UTC()

	applyBattlefieldTheme(&snapshot, scriptID, rng)
	clearBattlefieldSpawnAreas(&snapshot)
	ensureBattlefieldAllTerrains(&snapshot, rng)
	recountBattlefield(&snapshot)

	return snapshot
}

// applyBattlefieldTheme 在世界地图分层噪声结果上叠加少量主题偏置。
// 这里不再按坐标硬编码整张战场，只用随机散布/蜿蜒路径保证每个剧本有可辨识风格。
func applyBattlefieldTheme(snapshot *world.MapSnapshot, scriptID string, rng *rand.Rand) {
	if snapshot == nil || rng == nil {
		return
	}
	area := snapshot.Width * snapshot.Height
	switch scriptID {
	case "mountain_pass", "iron_ridge":
		paintBattlefieldPatches(snapshot, rng, world.TerrainMountain, sessionMaxInt(3, area/8), 0.45)
		paintBattlefieldPatches(snapshot, rng, world.TerrainRoad, sessionMaxInt(2, snapshot.Width/3), 0.15)
	case "twin_cities":
		placeBattlefieldLandmarks(snapshot, rng, world.TerrainCity, "city", 2)
		paintBattlefieldPatches(snapshot, rng, world.TerrainRoad, sessionMaxInt(2, snapshot.Width/4), 0.18)
	case "swamp_delta":
		carveBattlefieldRiver(snapshot, rng, sessionMaxInt(1, snapshot.Width/5))
		paintBattlefieldPatches(snapshot, rng, world.TerrainSwamp, sessionMaxInt(4, area/7), 0.5)
	case "desert_outpost":
		paintBattlefieldPatches(snapshot, rng, world.TerrainDesert, sessionMaxInt(5, area/5), 0.55)
		placeBattlefieldLandmarks(snapshot, rng, world.TerrainVillage, "village", 1)
	case "forest_maze":
		paintBattlefieldPatches(snapshot, rng, world.TerrainForest, sessionMaxInt(6, area/4), 0.65)
	case "frozen_front":
		paintBattlefieldPatches(snapshot, rng, world.TerrainSnowfield, sessionMaxInt(6, area/3), 0.55)
		paintBattlefieldPatches(snapshot, rng, world.TerrainMountain, sessionMaxInt(2, area/12), 0.25)
	case "river_fork":
		carveBattlefieldRiver(snapshot, rng, 2)
		paintBattlefieldPatches(snapshot, rng, world.TerrainRiverValley, sessionMaxInt(5, area/6), 0.45)
	case "grassland_charge":
		paintBattlefieldPatches(snapshot, rng, world.TerrainGrassland, sessionMaxInt(8, area/3), 0.5)
	case "village_belt":
		placeBattlefieldLandmarks(snapshot, rng, world.TerrainVillage, "village", 3)
		paintBattlefieldPatches(snapshot, rng, world.TerrainRoad, sessionMaxInt(3, snapshot.Width/3), 0.2)
	case "ruins_ring", defaultBattlefieldScriptID:
		placeBattlefieldLandmarks(snapshot, rng, world.TerrainRuins, "ruins", 2)
	}
}

func clearBattlefieldSpawnAreas(snapshot *world.MapSnapshot) {
	if snapshot == nil {
		return
	}
	for _, coord := range append(spawnCoordsForMap("player", snapshot.Width, snapshot.Height, 10), spawnCoordsForMap("enemy", snapshot.Width, snapshot.Height, 10)...) {
		tile := battlefieldTile(snapshot, coord)
		if tile == nil {
			continue
		}
		switch tile.Terrain {
		case world.TerrainMountain, world.TerrainRiver, world.TerrainSwamp, world.TerrainDesert, world.TerrainSnowfield:
			tile.Terrain = world.TerrainGrassland
			tile.Landmark = ""
		}
	}
}

func paintBattlefieldPatches(snapshot *world.MapSnapshot, rng *rand.Rand, terrain world.TerrainID, targetCount int, spreadChance float64) {
	if targetCount <= 0 {
		return
	}
	painted := 0
	for attempts := 0; attempts < targetCount*12 && painted < targetCount; attempts++ {
		coord := world.Coord{Q: rng.Intn(snapshot.Width), R: rng.Intn(snapshot.Height)}
		if paintBattlefieldTile(snapshot, coord, terrain, "") {
			painted++
		}
		for _, neighbor := range axialNeighbors(coord) {
			if painted >= targetCount || rng.Float64() > spreadChance {
				continue
			}
			if paintBattlefieldTile(snapshot, neighbor, terrain, "") {
				painted++
			}
		}
	}
}

func carveBattlefieldRiver(snapshot *world.MapSnapshot, rng *rand.Rand, branches int) {
	if branches <= 0 {
		branches = 1
	}
	for branch := 0; branch < branches; branch++ {
		q := rng.Intn(sessionMaxInt(1, snapshot.Width))
		for r := 0; r < snapshot.Height; r++ {
			paintBattlefieldTile(snapshot, world.Coord{Q: q, R: r}, world.TerrainRiver, "")
			if rng.Float64() < 0.55 {
				if rng.Intn(2) == 0 {
					q--
				} else {
					q++
				}
			}
			if q < 0 {
				q = 0
			}
			if q >= snapshot.Width {
				q = snapshot.Width - 1
			}
		}
	}
}

func placeBattlefieldLandmarks(snapshot *world.MapSnapshot, rng *rand.Rand, terrain world.TerrainID, landmark string, count int) {
	placed := 0
	for attempts := 0; attempts < count*30 && placed < count; attempts++ {
		coord := world.Coord{Q: rng.Intn(snapshot.Width), R: rng.Intn(snapshot.Height)}
		if isBattlefieldSpawnCoord(snapshot, coord) {
			continue
		}
		if paintBattlefieldTile(snapshot, coord, terrain, landmark) {
			placed++
		}
	}
}

func paintBattlefieldTile(snapshot *world.MapSnapshot, coord world.Coord, terrain world.TerrainID, landmark string) bool {
	if isBattlefieldSpawnCoord(snapshot, coord) {
		return false
	}
	tile := battlefieldTile(snapshot, coord)
	if tile == nil {
		return false
	}
	if landmark == "" && (tile.Terrain == world.TerrainCity || tile.Terrain == world.TerrainVillage || tile.Terrain == world.TerrainRuins) {
		return false
	}
	tile.Terrain = terrain
	tile.Landmark = landmark
	return true
}

func isBattlefieldSpawnCoord(snapshot *world.MapSnapshot, coord world.Coord) bool {
	if snapshot == nil {
		return false
	}
	if coord.Q == 1 || coord.Q == 2 || coord.Q == snapshot.Width-2 || coord.Q == snapshot.Width-3 {
		return coord.R > 0 && coord.R < snapshot.Height-1
	}
	return false
}

func battlefieldTile(snapshot *world.MapSnapshot, coord world.Coord) *world.Tile {
	if snapshot == nil || coord.Q < 0 || coord.Q >= snapshot.Width || coord.R < 0 || coord.R >= snapshot.Height {
		return nil
	}
	index := coord.R*snapshot.Width + coord.Q
	if index >= 0 && index < len(snapshot.Tiles) && snapshot.Tiles[index].Coord == coord {
		return &snapshot.Tiles[index]
	}
	for index := range snapshot.Tiles {
		if snapshot.Tiles[index].Coord == coord {
			return &snapshot.Tiles[index]
		}
	}
	return nil
}

func recountBattlefield(snapshot *world.MapSnapshot) {
	if snapshot == nil {
		return
	}
	snapshot.Counts = make(map[world.TerrainID]int)
	for _, tile := range snapshot.Tiles {
		snapshot.Counts[tile.Terrain]++
	}
}

func ensureBattlefieldAllTerrains(snapshot *world.MapSnapshot, rng *rand.Rand) {
	if snapshot == nil || len(snapshot.Tiles) == 0 {
		return
	}
	counts := make(map[world.TerrainID]int)
	for _, tile := range snapshot.Tiles {
		counts[tile.Terrain]++
	}
	for _, definition := range world.TerrainCatalog() {
		if counts[definition.ID] > 0 {
			continue
		}
		index := battlefieldBackfillTerrainIndex(snapshot, rng, counts)
		if index < 0 {
			return
		}
		previous := snapshot.Tiles[index].Terrain
		counts[previous]--
		snapshot.Tiles[index].Terrain = definition.ID
		snapshot.Tiles[index].Landmark = battlefieldLandmarkForTerrain(definition.ID)
		counts[definition.ID]++
	}
}

func battlefieldBackfillTerrainIndex(snapshot *world.MapSnapshot, rng *rand.Rand, counts map[world.TerrainID]int) int {
	start := 0
	if rng != nil && len(snapshot.Tiles) > 0 {
		start = rng.Intn(len(snapshot.Tiles))
	}
	for offset := 0; offset < len(snapshot.Tiles); offset++ {
		index := (start + offset) % len(snapshot.Tiles)
		tile := snapshot.Tiles[index]
		if isBattlefieldSpawnCoord(snapshot, tile.Coord) {
			continue
		}
		if counts[tile.Terrain] > 1 {
			return index
		}
	}
	return -1
}

func battlefieldLandmarkForTerrain(terrain world.TerrainID) string {
	switch terrain {
	case world.TerrainCity:
		return "city"
	case world.TerrainVillage:
		return "village"
	case world.TerrainRuins:
		return "ruins"
	default:
		return ""
	}
}

// inBounds 判断坐标是否落在战场边界内。
func inBounds(snapshot world.MapSnapshot, coord world.Coord) bool {
	return coord.Q >= 0 && coord.Q < snapshot.Width && coord.R >= 0 && coord.R < snapshot.Height
}

// axialNeighbors 返回六边形轴坐标系下的六个相邻格。
func axialNeighbors(coord world.Coord) []world.Coord {
	return []world.Coord{
		{Q: coord.Q + 1, R: coord.R},
		{Q: coord.Q + 1, R: coord.R - 1},
		{Q: coord.Q, R: coord.R - 1},
		{Q: coord.Q - 1, R: coord.R},
		{Q: coord.Q - 1, R: coord.R + 1},
		{Q: coord.Q, R: coord.R + 1},
	}
}

// coordString 把坐标编码成稳定字符串，便于做 map key。
func coordString(coord world.Coord) string {
	return fmt.Sprintf("%d:%d", coord.Q, coord.R)
}
