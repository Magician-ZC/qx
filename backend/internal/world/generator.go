package world

// 文件说明：世界地图生成与存取模块，负责地形/河流/聚落布局生成及快照读写。

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	DefaultMapWidth  = 64
	DefaultMapHeight = 40
)

const (
	minLargeMapCities   = 1
	maxLargeMapCities   = 4
	minLargeMapVillages = 3
	maxLargeMapVillages = 10
	minLargeMapRuins    = 4
	maxLargeMapRuins    = 12
)

// Coord 表示六边格地图上的轴坐标。
type Coord struct {
	Q int `json:"q"`
	R int `json:"r"`
}

// Tile 表示地图单格地形与地标信息。
type Tile struct {
	Coord    Coord     `json:"coord"`
	Terrain  TerrainID `json:"terrain"`
	RegionID string    `json:"region_id"`
	Landmark string    `json:"landmark,omitempty"`
}

// MapSnapshot 表示一张完整可回放的地图快照。
type MapSnapshot struct {
	ID          string            `json:"id"`
	Seed        int64             `json:"seed"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	GeneratedAt time.Time         `json:"generated_at"`
	Tiles       []Tile            `json:"tiles"`
	Counts      map[TerrainID]int `json:"counts"`
}

// MapSummary 表示地图摘要（用于列表与概览场景）。
type MapSummary struct {
	ID          string            `json:"id"`
	Seed        int64             `json:"seed"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	GeneratedAt time.Time         `json:"generated_at"`
	TileCount   int               `json:"tile_count"`
	Counts      map[TerrainID]int `json:"counts"`
}

type terrainClimate struct {
	Elevation   float64
	Temperature float64
	Humidity    float64
}

// GenerateMap 按随机种子生成地形、河流、聚落与道路。
// 生成流程参考 Unciv 的分层管线：先用种子噪声生成高度/温度/湿度，再做山脉聚类、河流、植被与地标分布。
func GenerateMap(seed int64, width int, height int) MapSnapshot {
	if width <= 0 {
		width = DefaultMapWidth
	}
	if height <= 0 {
		height = DefaultMapHeight
	}

	rng := rand.New(rand.NewSource(seed))
	now := time.Now().UTC()
	tiles := make([]Tile, 0, width*height)
	for r := 0; r < height; r++ {
		for q := 0; q < width; q++ {
			tiles = append(tiles, Tile{
				Coord:    Coord{Q: q, R: r},
				Terrain:  TerrainPlains,
				RegionID: regionFor(q, r, width, height),
			})
		}
	}

	snapshot := MapSnapshot{
		ID:          fmt.Sprintf("map-%d-%d", seed, time.Now().UTC().UnixNano()),
		Seed:        seed,
		Width:       width,
		Height:      height,
		GeneratedAt: now,
		Tiles:       tiles,
		Counts:      make(map[TerrainID]int),
	}

	climate := buildTerrainClimate(seed, width, height)
	snapshot.applyBiomeTerrain(climate)
	snapshot.shapeMountainChains(rng)
	snapshot.spawnRivers(rng, climate)
	snapshot.spawnRiverValleys(rng)

	area := width * height
	cityCount := clampInt((area/900)+1, minLargeMapCities, maxLargeMapCities)
	villageCount := clampInt((area/220)+4, minLargeMapVillages, maxLargeMapVillages)
	ruinsCount := clampInt((area/260)+4, minLargeMapRuins, maxLargeMapRuins)

	settlementCoords := make([]Coord, 0, cityCount+villageCount)
	settlementCoords = append(settlementCoords, snapshot.placeLandmarksSpread(rng, TerrainCity, "city", cityCount, maxInt(width, height)/5)...)
	settlementCoords = append(settlementCoords, snapshot.placeLandmarksSpread(rng, TerrainVillage, "village", villageCount, maxInt(width, height)/8)...)
	snapshot.placeLandmarksSpread(rng, TerrainRuins, "ruins", ruinsCount, maxInt(width, height)/9)

	for _, road := range connectSettlements(settlementCoords) {
		tile := snapshot.tile(road.Q, road.R)
		if tile.Terrain == TerrainRiver || tile.Terrain == TerrainMountain {
			continue
		}
		if tile.Terrain == TerrainCity || tile.Terrain == TerrainVillage {
			continue
		}
		tile.Terrain = TerrainRoad
	}

	snapshot.EnsureAllTerrains(rng)
	snapshot.recount()
	return snapshot
}

// SaveMap 把地图快照持久化到 world_maps/world_tiles 表。
func SaveMap(ctx context.Context, db *sql.DB, snapshot MapSnapshot) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin map transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(
		ctx,
		`
		INSERT INTO world_maps (id, seed, width, height, generated_at)
		VALUES (?, ?, ?, ?, ?)
		`,
		snapshot.ID,
		snapshot.Seed,
		snapshot.Width,
		snapshot.Height,
		snapshot.GeneratedAt.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("insert world map: %w", err)
	}

	statement, err := tx.PrepareContext(
		ctx,
		`
		INSERT INTO world_tiles (map_id, q, r, terrain_id, region_id, landmark)
		VALUES (?, ?, ?, ?, ?, ?)
		`,
	)
	if err != nil {
		return fmt.Errorf("prepare world tile insert: %w", err)
	}
	defer statement.Close()

	for _, tile := range snapshot.Tiles {
		if _, err := statement.ExecContext(
			ctx,
			snapshot.ID,
			tile.Coord.Q,
			tile.Coord.R,
			string(tile.Terrain),
			tile.RegionID,
			tile.Landmark,
		); err != nil {
			return fmt.Errorf("insert world tile %d,%d: %w", tile.Coord.Q, tile.Coord.R, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit map transaction: %w", err)
	}

	return nil
}

// LoadLatestMapSummary 读取最近一张地图的摘要与地形计数。
func LoadLatestMapSummary(ctx context.Context, db *sql.DB) (MapSummary, error) {
	var summary MapSummary
	var generatedAt string
	if err := db.QueryRowContext(
		ctx,
		`
		SELECT id, seed, width, height, generated_at
		FROM world_maps
		ORDER BY generated_at DESC
		LIMIT 1
		`,
	).Scan(&summary.ID, &summary.Seed, &summary.Width, &summary.Height, &generatedAt); err != nil {
		return MapSummary{}, fmt.Errorf("query latest world map: %w", err)
	}

	timestamp, err := time.Parse(time.RFC3339Nano, generatedAt)
	if err != nil {
		timestamp, err = time.Parse(time.RFC3339, generatedAt)
		if err != nil {
			return MapSummary{}, fmt.Errorf("parse generated_at: %w", err)
		}
	}
	summary.GeneratedAt = timestamp

	rows, err := db.QueryContext(
		ctx,
		`
		SELECT terrain_id, COUNT(*)
		FROM world_tiles
		WHERE map_id = ?
		GROUP BY terrain_id
		`,
		summary.ID,
	)
	if err != nil {
		return MapSummary{}, fmt.Errorf("query world tile counts: %w", err)
	}
	defer rows.Close()

	summary.Counts = make(map[TerrainID]int)
	for rows.Next() {
		var terrainID TerrainID
		var count int
		if err := rows.Scan(&terrainID, &count); err != nil {
			return MapSummary{}, fmt.Errorf("scan world tile count: %w", err)
		}
		summary.Counts[terrainID] = count
		summary.TileCount += count
	}

	if err := rows.Err(); err != nil {
		return MapSummary{}, fmt.Errorf("iterate world tile counts: %w", err)
	}

	return summary, nil
}

// LoadLatestMapSnapshot 读取最近一张地图的完整快照。
func LoadLatestMapSnapshot(ctx context.Context, db *sql.DB) (MapSnapshot, error) {
	var snapshot MapSnapshot
	var generatedAt string
	if err := db.QueryRowContext(
		ctx,
		`
		SELECT id, seed, width, height, generated_at
		FROM world_maps
		ORDER BY generated_at DESC
		LIMIT 1
		`,
	).Scan(&snapshot.ID, &snapshot.Seed, &snapshot.Width, &snapshot.Height, &generatedAt); err != nil {
		return MapSnapshot{}, fmt.Errorf("query latest world map: %w", err)
	}

	timestamp, err := time.Parse(time.RFC3339Nano, generatedAt)
	if err != nil {
		timestamp, err = time.Parse(time.RFC3339, generatedAt)
		if err != nil {
			return MapSnapshot{}, fmt.Errorf("parse generated_at: %w", err)
		}
	}
	snapshot.GeneratedAt = timestamp

	rows, err := db.QueryContext(
		ctx,
		`
		SELECT q, r, terrain_id, region_id, landmark
		FROM world_tiles
		WHERE map_id = ?
		ORDER BY r, q
		`,
		snapshot.ID,
	)
	if err != nil {
		return MapSnapshot{}, fmt.Errorf("query world tiles: %w", err)
	}
	defer rows.Close()

	snapshot.Tiles = make([]Tile, 0, snapshot.Width*snapshot.Height)
	snapshot.Counts = make(map[TerrainID]int)
	for rows.Next() {
		var tile Tile
		if err := rows.Scan(
			&tile.Coord.Q,
			&tile.Coord.R,
			&tile.Terrain,
			&tile.RegionID,
			&tile.Landmark,
		); err != nil {
			return MapSnapshot{}, fmt.Errorf("scan world tile: %w", err)
		}

		snapshot.Tiles = append(snapshot.Tiles, tile)
		snapshot.Counts[tile.Terrain]++
	}

	if err := rows.Err(); err != nil {
		return MapSnapshot{}, fmt.Errorf("iterate world tiles: %w", err)
	}

	return snapshot, nil
}

// buildTerrainClimate 为每个格子生成高度、温度、湿度三张种子噪声图。
func buildTerrainClimate(seed int64, width int, height int) []terrainClimate {
	climate := make([]terrainClimate, width*height)
	elevationSeed := seed ^ 0x4c414e44
	temperatureSeed := seed ^ 0x54454d50
	humiditySeed := seed ^ 0x48554d49
	maxDimension := float64(maxInt(width, height))
	continentScale := math.Max(3, maxDimension/3.5)
	biomeScale := math.Max(2, maxDimension/4.5)

	for r := 0; r < height; r++ {
		for q := 0; q < width; q++ {
			latitude := 0.0
			if height > 1 {
				latitude = math.Abs((float64(r)/float64(height-1))*2 - 1)
			}
			edgeFalloff := distanceToMapEdgeRatio(q, r, width, height)
			elevation := fractalNoise(elevationSeed, float64(q), float64(r), continentScale, 4, 0.55)
			elevation += (edgeFalloff - 0.5) * 0.32
			humidity := (fractalNoise(humiditySeed, float64(q), float64(r), biomeScale, 3, 0.58) + 1) / 2
			temperatureNoise := fractalNoise(temperatureSeed, float64(q), float64(r), biomeScale*1.2, 2, 0.5) * 0.25
			temperature := (1 - latitude) + temperatureNoise

			climate[(r*width)+q] = terrainClimate{
				Elevation:   elevation,
				Temperature: clampFloat(temperature, 0, 1),
				Humidity:    clampFloat(humidity, 0, 1),
			}
		}
	}
	return climate
}

// applyBiomeTerrain 依据温度/湿度/高度选择基础地形，避免旧版按行列固定分布。
func (snapshot *MapSnapshot) applyBiomeTerrain(climate []terrainClimate) {
	for index := range snapshot.Tiles {
		cell := climate[index]
		switch {
		case cell.Elevation > 0.68:
			snapshot.Tiles[index].Terrain = TerrainMountain
		case cell.Temperature < 0.18:
			snapshot.Tiles[index].Terrain = TerrainSnowfield
		case cell.Temperature > 0.76 && cell.Humidity < 0.34:
			snapshot.Tiles[index].Terrain = TerrainDesert
		case cell.Humidity > 0.78 && cell.Elevation < 0.26:
			snapshot.Tiles[index].Terrain = TerrainSwamp
		case cell.Humidity > 0.58:
			snapshot.Tiles[index].Terrain = TerrainForest
		case cell.Temperature > 0.48 && cell.Humidity > 0.34:
			snapshot.Tiles[index].Terrain = TerrainGrassland
		default:
			snapshot.Tiles[index].Terrain = TerrainPlains
		}
	}
}

// shapeMountainChains 通过邻居元胞规则把山地整理成 Unciv 式连锁山脉。
func (snapshot *MapSnapshot) shapeMountainChains(rng *rand.Rand) {
	for range 3 {
		toMountain := make([]Coord, 0)
		toFlat := make([]Coord, 0)
		for _, tile := range snapshot.Tiles {
			mountainNeighbors := 0
			for _, neighbor := range axialNeighbors(tile.Coord) {
				if snapshot.inBounds(neighbor.Q, neighbor.R) && snapshot.tile(neighbor.Q, neighbor.R).Terrain == TerrainMountain {
					mountainNeighbors++
				}
			}
			if tile.Terrain == TerrainMountain && mountainNeighbors == 0 && rng.Float64() < 0.58 {
				toFlat = append(toFlat, tile.Coord)
			}
			if tile.Terrain != TerrainMountain && mountainNeighbors >= 2 && rng.Float64() < 0.34 {
				toMountain = append(toMountain, tile.Coord)
			}
		}
		for _, coord := range toFlat {
			snapshot.tile(coord.Q, coord.R).Terrain = TerrainPlains
		}
		for _, coord := range toMountain {
			tile := snapshot.tile(coord.Q, coord.R)
			if tile.Terrain != TerrainRiver && tile.Landmark == "" {
				tile.Terrain = TerrainMountain
			}
		}
	}
}

// spawnRivers 从高地/山脉向边缘或低地水口生成多条弯曲河流。
func (snapshot *MapSnapshot) spawnRivers(rng *rand.Rand, climate []terrainClimate) {
	starts := make([]Coord, 0)
	for _, tile := range snapshot.Tiles {
		cell := climate[(tile.Coord.R*snapshot.Width)+tile.Coord.Q]
		if tile.Terrain == TerrainMountain || cell.Elevation > 0.56 {
			starts = append(starts, tile.Coord)
		}
	}
	if len(starts) == 0 {
		starts = append(starts, Coord{Q: snapshot.Width / 2, R: snapshot.Height / 2})
	}
	sort.Slice(starts, func(i int, j int) bool {
		left := climate[(starts[i].R*snapshot.Width)+starts[i].Q].Elevation
		right := climate[(starts[j].R*snapshot.Width)+starts[j].Q].Elevation
		return left > right
	})
	riverCount := clampInt((snapshot.Width*snapshot.Height)/420+1, 1, 5)
	minSpacing := maxInt(2, maxInt(snapshot.Width, snapshot.Height)/5)
	chosen := chooseSpreadOutCoords(rng, starts, riverCount, minSpacing)
	for _, start := range chosen {
		for _, coord := range snapshot.riverPathFrom(rng, start, climate) {
			tile := snapshot.tile(coord.Q, coord.R)
			if tile.Landmark == "" {
				tile.Terrain = TerrainRiver
			}
		}
	}
}

func (snapshot *MapSnapshot) riverPathFrom(rng *rand.Rand, start Coord, climate []terrainClimate) []Coord {
	path := make([]Coord, 0, maxInt(snapshot.Width, snapshot.Height))
	current := start
	visited := map[Coord]struct{}{}
	maxSteps := snapshot.Width + snapshot.Height
	for step := 0; step < maxSteps; step++ {
		if !snapshot.inBounds(current.Q, current.R) {
			break
		}
		path = append(path, current)
		visited[current] = struct{}{}
		if current.Q == 0 || current.R == 0 || current.Q == snapshot.Width-1 || current.R == snapshot.Height-1 {
			break
		}

		type candidate struct {
			coord Coord
			score float64
		}
		candidates := make([]candidate, 0, 6)
		currentElevation := climate[(current.R*snapshot.Width)+current.Q].Elevation
		for _, neighbor := range axialNeighbors(current) {
			if !snapshot.inBounds(neighbor.Q, neighbor.R) {
				continue
			}
			if _, ok := visited[neighbor]; ok {
				continue
			}
			cell := climate[(neighbor.R*snapshot.Width)+neighbor.Q]
			edgeBias := 1 - distanceToMapEdgeRatio(neighbor.Q, neighbor.R, snapshot.Width, snapshot.Height)
			downhill := currentElevation - cell.Elevation
			score := downhill*1.8 + edgeBias*0.75 + cell.Humidity*0.25 + rng.Float64()*0.24
			candidates = append(candidates, candidate{coord: neighbor, score: score})
		}
		if len(candidates) == 0 {
			break
		}
		sort.Slice(candidates, func(i int, j int) bool { return candidates[i].score > candidates[j].score })
		current = candidates[0].coord
	}
	return path
}

// spawnRiverValleys 在河流两侧生成湿润河谷。
func (snapshot *MapSnapshot) spawnRiverValleys(rng *rand.Rand) {
	rivers := make([]Coord, 0)
	for _, tile := range snapshot.Tiles {
		if tile.Terrain == TerrainRiver {
			rivers = append(rivers, tile.Coord)
		}
	}
	for _, coord := range rivers {
		for _, neighbor := range axialNeighbors(coord) {
			if !snapshot.inBounds(neighbor.Q, neighbor.R) {
				continue
			}
			tile := snapshot.tile(neighbor.Q, neighbor.R)
			if tile.Landmark != "" || tile.Terrain == TerrainRiver || tile.Terrain == TerrainMountain || tile.Terrain == TerrainSnowfield {
				continue
			}
			if rng.Float64() < 0.52 {
				tile.Terrain = TerrainRiverValley
			}
		}
	}
}

func (snapshot *MapSnapshot) placeLandmarksSpread(rng *rand.Rand, terrain TerrainID, landmark string, count int, minDistance int) []Coord {
	placed := make([]Coord, 0, count)
	for len(placed) < count {
		coord, ok := snapshot.findLandmarkSite(rng, terrain, placed, minDistance)
		if !ok && minDistance > 1 {
			minDistance--
			continue
		}
		if !ok {
			break
		}
		tile := snapshot.tile(coord.Q, coord.R)
		tile.Terrain = terrain
		tile.Landmark = landmark
		placed = append(placed, coord)
	}
	return placed
}

func (snapshot *MapSnapshot) findLandmarkSite(rng *rand.Rand, terrain TerrainID, existing []Coord, minDistance int) (Coord, bool) {
	for attempts := 0; attempts < 5000; attempts++ {
		q := rng.Intn(snapshot.Width)
		r := rng.Intn(snapshot.Height)
		coord := Coord{Q: q, R: r}
		tile := snapshot.tile(q, r)
		if tile.Landmark != "" || tile.Terrain == TerrainRiver || tile.Terrain == TerrainMountain || tile.Terrain == TerrainSnowfield {
			continue
		}
		if terrain == TerrainCity && (tile.Terrain == TerrainSwamp || tile.Terrain == TerrainDesert) {
			continue
		}
		if !farEnoughFromAll(coord, existing, minDistance) {
			continue
		}
		return coord, true
	}
	return Coord{}, false
}

func chooseSpreadOutCoords(rng *rand.Rand, candidates []Coord, count int, minDistance int) []Coord {
	if count <= 0 || len(candidates) == 0 {
		return nil
	}
	shuffled := append([]Coord(nil), candidates...)
	rng.Shuffle(len(shuffled), func(i int, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	result := make([]Coord, 0, count)
	for len(result) < count && minDistance >= 0 {
		for _, candidate := range shuffled {
			if len(result) >= count {
				break
			}
			if farEnoughFromAll(candidate, result, minDistance) {
				result = append(result, candidate)
			}
		}
		minDistance--
	}
	return result
}

func farEnoughFromAll(coord Coord, existing []Coord, minDistance int) bool {
	for _, other := range existing {
		if hexDistance(coord, other) < minDistance {
			return false
		}
	}
	return true
}

func hexDistance(a Coord, b Coord) int {
	dq := a.Q - b.Q
	dr := a.R - b.R
	ds := (-a.Q - a.R) - (-b.Q - b.R)
	return (absInt(dq) + absInt(dr) + absInt(ds)) / 2
}

func distanceToMapEdgeRatio(q int, r int, width int, height int) float64 {
	if width <= 1 || height <= 1 {
		return 0
	}
	distance := minInt(minInt(q, width-1-q), minInt(r, height-1-r))
	maxDistance := maxInt(1, minInt(width, height)/2)
	return clampFloat(float64(distance)/float64(maxDistance), 0, 1)
}

func fractalNoise(seed int64, x float64, y float64, scale float64, octaves int, persistence float64) float64 {
	if scale <= 0 {
		scale = 1
	}
	total := 0.0
	amplitude := 1.0
	frequency := 1.0
	maxAmplitude := 0.0
	for octave := 0; octave < octaves; octave++ {
		total += smoothValueNoise(seed+int64(octave*7919), x*frequency/scale, y*frequency/scale) * amplitude
		maxAmplitude += amplitude
		amplitude *= persistence
		frequency *= 2
	}
	if maxAmplitude == 0 {
		return 0
	}
	return total / maxAmplitude
}

func smoothValueNoise(seed int64, x float64, y float64) float64 {
	x0 := math.Floor(x)
	y0 := math.Floor(y)
	xf := x - x0
	yf := y - y0
	sx := smootherStep(xf)
	sy := smootherStep(yf)
	n00 := valueNoise(seed, int(x0), int(y0))
	n10 := valueNoise(seed, int(x0)+1, int(y0))
	n01 := valueNoise(seed, int(x0), int(y0)+1)
	n11 := valueNoise(seed, int(x0)+1, int(y0)+1)
	nx0 := lerp(n00, n10, sx)
	nx1 := lerp(n01, n11, sx)
	return lerp(nx0, nx1, sy)
}

func valueNoise(seed int64, x int, y int) float64 {
	n := int64(x)*374761393 + int64(y)*668265263 + seed*1442695040888963407
	n = (n ^ (n >> 13)) * 1274126177
	n = n ^ (n >> 16)
	return (float64(uint64(n)&0xffff)/32767.5 - 1)
}

func smootherStep(t float64) float64               { return t * t * t * (t*(t*6-15) + 10) }
func lerp(a float64, b float64, t float64) float64 { return a + (b-a)*t }

func clampFloat(value float64, minValue float64, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func clampInt(value int, minValue int, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// chooseBaseTerrain 按位置和随机权重选择基础地形。
func chooseBaseTerrain(rng *rand.Rand, q int, r int, width int, height int) TerrainID {
	latitude := math.Abs(float64(r-height/2)) / (float64(height) / 2)
	dryness := math.Abs(float64(q-width/2)) / (float64(width) / 2)

	switch {
	case latitude > 0.78:
		if rng.Float64() < 0.65 {
			return TerrainSnowfield
		}
		return TerrainMountain
	case latitude > 0.62:
		if rng.Float64() < 0.35 {
			return TerrainMountain
		}
		return TerrainForest
	case dryness > 0.72 && rng.Float64() < 0.42:
		return TerrainDesert
	case rng.Float64() < 0.18:
		return TerrainSwamp
	case rng.Float64() < 0.38:
		return TerrainForest
	case rng.Float64() < 0.58:
		return TerrainGrassland
	default:
		return TerrainPlains
	}
}

// generateRiverPath 生成一条纵向贯穿地图的河流路径。
func generateRiverPath(rng *rand.Rand, width int, height int) []Coord {
	path := make([]Coord, 0, height)
	q := rng.Intn(width / 3)
	for r := 0; r < height; r++ {
		path = append(path, Coord{Q: q, R: r})
		switch rng.Intn(3) {
		case 0:
			q--
		case 2:
			q++
		}
		if q < 1 {
			q = 1
		}
		if q > width-2 {
			q = width - 2
		}
	}
	return path
}

// connectSettlements 连接聚落坐标并生成道路路径集合。
func connectSettlements(points []Coord) []Coord {
	if len(points) < 2 {
		return nil
	}

	sorted := append([]Coord(nil), points...)
	sort.Slice(sorted, func(i int, j int) bool {
		if sorted[i].R == sorted[j].R {
			return sorted[i].Q < sorted[j].Q
		}
		return sorted[i].R < sorted[j].R
	})

	roads := make([]Coord, 0, len(sorted)*8)
	for index := 0; index < len(sorted)-1; index++ {
		roads = append(roads, axialLine(sorted[index], sorted[index+1])...)
	}
	return roads
}

// axialLine 计算两点间的六边格近似直线路径。
func axialLine(from Coord, to Coord) []Coord {
	current := from
	line := []Coord{current}
	for current != to {
		switch {
		case current.Q < to.Q:
			current.Q++
		case current.Q > to.Q:
			current.Q--
		}

		switch {
		case current.R < to.R:
			current.R++
		case current.R > to.R:
			current.R--
		}

		line = append(line, current)
	}
	return line
}

// axialNeighbors 返回六边格坐标的六个相邻点。
func axialNeighbors(coord Coord) []Coord {
	return []Coord{
		{Q: coord.Q + 1, R: coord.R},
		{Q: coord.Q - 1, R: coord.R},
		{Q: coord.Q, R: coord.R + 1},
		{Q: coord.Q, R: coord.R - 1},
		{Q: coord.Q + 1, R: coord.R - 1},
		{Q: coord.Q - 1, R: coord.R + 1},
	}
}

// regionFor 根据坐标把地图切分到稳定区域 ID。
func regionFor(q int, r int, width int, height int) string {
	switch {
	case q < width/3 && r < height/2:
		return "northwest"
	case q >= (2*width)/3 && r < height/2:
		return "northeast"
	case q < width/3:
		return "southwest"
	case q >= (2*width)/3:
		return "southeast"
	default:
		return "heartland"
	}
}

// placeLandmark 在可用地块上放置指定地标并返回坐标。
func (snapshot *MapSnapshot) placeLandmark(rng *rand.Rand, terrain TerrainID, landmark string) Coord {
	for attempts := 0; attempts < 5000; attempts++ {
		q := rng.Intn(snapshot.Width)
		r := rng.Intn(snapshot.Height)
		tile := snapshot.tile(q, r)
		if tile.Terrain == TerrainRiver || tile.Terrain == TerrainMountain {
			continue
		}
		if tile.Landmark != "" {
			continue
		}
		tile.Terrain = terrain
		tile.Landmark = landmark
		return tile.Coord
	}

	tile := snapshot.tile(snapshot.Width/2, snapshot.Height/2)
	tile.Terrain = terrain
	tile.Landmark = landmark
	return tile.Coord
}

// EnsureAllTerrains 确保地图至少覆盖地形目录里的每一种地块类型。
func (snapshot *MapSnapshot) EnsureAllTerrains(rng *rand.Rand) {
	if snapshot == nil || len(snapshot.Tiles) == 0 {
		return
	}

	counts := make(map[TerrainID]int)
	for _, tile := range snapshot.Tiles {
		counts[tile.Terrain]++
	}

	for _, definition := range TerrainCatalog() {
		if counts[definition.ID] > 0 {
			continue
		}

		index := snapshot.backfillTerrainIndex(rng, counts)
		if index < 0 {
			return
		}

		previous := snapshot.Tiles[index].Terrain
		counts[previous]--
		snapshot.Tiles[index].Terrain = definition.ID
		snapshot.Tiles[index].Landmark = landmarkForTerrain(definition.ID)
		counts[definition.ID]++
	}
}

func (snapshot *MapSnapshot) backfillTerrainIndex(rng *rand.Rand, counts map[TerrainID]int) int {
	if len(snapshot.Tiles) == 0 {
		return -1
	}
	start := 0
	if rng != nil {
		start = rng.Intn(len(snapshot.Tiles))
	}
	for offset := 0; offset < len(snapshot.Tiles); offset++ {
		index := (start + offset) % len(snapshot.Tiles)
		if counts[snapshot.Tiles[index].Terrain] > 1 {
			return index
		}
	}
	return -1
}

func landmarkForTerrain(terrain TerrainID) string {
	switch terrain {
	case TerrainCity:
		return "city"
	case TerrainVillage:
		return "village"
	case TerrainRuins:
		return "ruins"
	default:
		return ""
	}
}

// recount 重新统计各地形计数。
func (snapshot *MapSnapshot) recount() {
	snapshot.Counts = make(map[TerrainID]int)
	for _, tile := range snapshot.Tiles {
		snapshot.Counts[tile.Terrain]++
	}
}

// inBounds 判断坐标是否落在地图边界内。
func (snapshot *MapSnapshot) inBounds(q int, r int) bool {
	return q >= 0 && q < snapshot.Width && r >= 0 && r < snapshot.Height
}

// tile 返回坐标对应地块指针（调用前需保证坐标合法）。
func (snapshot *MapSnapshot) tile(q int, r int) *Tile {
	return &snapshot.Tiles[(r*snapshot.Width)+q]
}
