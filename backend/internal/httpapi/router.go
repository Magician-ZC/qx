package httpapi

// 文件说明：HTTP API 路由总装入口，连接会话服务、账户服务、地形接口与实时推送广播。

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"log/slog"

	"qunxiang/backend/internal/account"
	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/combat"
	"qunxiang/backend/internal/config"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/session"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/ws"
)

// Dependencies 聚合 Router 初始化所需的外部依赖。
type Dependencies struct {
	Config    config.Config
	Logger    *slog.Logger
	Hub       *ws.Hub
	Store     *sql.DB
	AI        *ai.Service
	ColdStore *sql.DB
	Accounts  *account.Service
}

// NewRouter 组装 HTTP 路由、会话服务与实时推送链路。
func NewRouter(deps Dependencies) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery(), gin.Logger())
	router.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Role-Token")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})
	_ = router.SetTrustedProxies(nil)

	hub := deps.Hub
	if hub == nil {
		hub = ws.NewHub(deps.Logger)
	}
	duelAuth := newDuelSessionAuthStoreWithDB(deps.Store)

	debugSnapshotRequested := func(c *gin.Context) bool {
		if c == nil {
			return false
		}
		value := strings.ToLower(strings.TrimSpace(c.Query("debug")))
		if value == "1" || value == "true" || value == "yes" {
			return true
		}
		value = strings.ToLower(strings.TrimSpace(c.GetHeader("X-Qunxiang-Debug")))
		return value == "1" || value == "true" || value == "yes"
	}
	publicForRequest := func(c *gin.Context, snapshot session.Snapshot) session.Snapshot {
		return session.PublicSnapshot(snapshot, debugSnapshotRequested(c))
	}
	broadcastSessionSnapshot := func(reason string, snapshot session.Snapshot, extra map[string]any) {
		if strings.TrimSpace(snapshot.ID) == "" {
			return
		}
		publicSnapshot := session.PublicSnapshot(snapshot, false)
		payload := map[string]any{
			"reason":  strings.TrimSpace(reason),
			"session": publicSnapshot,
		}
		for key, value := range extra {
			payload[key] = value
		}
		hub.BroadcastSessionEvent(snapshot.ID, "session_snapshot", payload)

		interactions := publicSnapshot.LLMInteractions
		if len(interactions) > 4 {
			interactions = interactions[len(interactions)-4:]
		}
		for _, interaction := range interactions {
			hub.BroadcastSessionEvent(snapshot.ID, "llm_interaction", interaction)
		}

		if len(snapshot.Logs) > 0 {
			hub.BroadcastSessionEvent(snapshot.ID, "session_log", snapshot.Logs[len(snapshot.Logs)-1])
		}
	}
	broadcastSessionProgress := func(reason string, snapshot session.Snapshot, extra map[string]any) {
		if strings.TrimSpace(snapshot.ID) == "" {
			return
		}
		publicSnapshot := session.PublicSnapshot(snapshot, false)
		payload := map[string]any{
			"reason":  strings.TrimSpace(reason),
			"session": publicSnapshot,
		}
		for key, value := range extra {
			payload[key] = value
		}
		hub.BroadcastSessionEvent(snapshot.ID, "session_snapshot", payload)
		if len(snapshot.Logs) > 0 {
			hub.BroadcastSessionEvent(snapshot.ID, "session_log", snapshot.Logs[len(snapshot.Logs)-1])
		}
	}
	newSessionService := func() *session.Service {
		service := session.NewServiceWithColdStore(deps.Store, deps.AI, deps.ColdStore)
		service.SetAsyncExecution(true)
		service.SetProgressReporter(broadcastSessionProgress)
		// 开启归因强制：无源戏剧性自治选择优雅回退安全决策（设计宪法 §5）。
		// 遥测见 Service.AttributionStats()；若线上 OOC 率过高可改回 false。
		service.SetAttributionEnforcement(true)
		return service
	}
	resolveCommanderFaction := func(c *gin.Context, sessionID string, fallbackFactionID string) (string, bool) {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			return "", false
		}
		if !duelAuth.requiresToken(c.Request.Context(), sessionID) {
			return strings.TrimSpace(fallbackFactionID), true
		}

		roleToken := strings.TrimSpace(c.GetHeader("X-Session-Role-Token"))
		if roleToken == "" {
			roleToken = strings.TrimSpace(c.Query("role_token"))
		}
		role, ok := duelAuth.resolveRole(c.Request.Context(), sessionID, roleToken)
		if !ok {
			return "", false
		}
		switch role {
		case duelRoleEnemy:
			return "enemy", true
		default:
			return "player", true
		}
	}

	router.GET("/healthz", func(c *gin.Context) {
		aiStatus := map[string]any{
			"ready": false,
		}
		if deps.AI != nil {
			aiStatus = deps.AI.Status()
		}
		attrTotal, attrOOC := session.AttributionStats()
		attrDegraded := session.AttributionDegraded()
		attrOOCRate := 0.0
		if attrTotal > 0 {
			attrOOCRate = float64(attrOOC) / float64(attrTotal)
		}

		status := gin.H{
			"status":                     "ok",
			"service":                    "qunxiang-backend",
			"client_count":               hub.ClientCount(),
			"room_count":                 hub.RoomCount(),
			"queue_count":                hub.MatchmakingQueueCount(),
			"session_subscription_count": hub.SessionSubscriptionCount(),
			"sqlite_path":                deps.Config.SQLitePath,
			"timestamp":                  time.Now().UTC().Format(time.RFC3339),
			"ai":                         aiStatus,
			"accounts":                   deps.Accounts != nil,
			"cold_storage":               deps.ColdStore != nil,
			"attribution": gin.H{
				"enforced": true,
				"degraded": attrDegraded,
				"active":   !attrDegraded,
				"total":    attrTotal,
				"ooc":      attrOOC,
				"ooc_rate": attrOOCRate,
			},
		}

		if err := deps.Store.PingContext(c.Request.Context()); err != nil {
			status["status"] = "degraded"
			status["error"] = err.Error()
			c.JSON(http.StatusServiceUnavailable, status)
			return
		}

		c.JSON(http.StatusOK, status)
	})

	router.GET("/api/world/terrains", func(c *gin.Context) {
		if err := world.SeedTerrainCatalog(c.Request.Context(), deps.Store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		terrains, err := world.LoadTerrainCatalog(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"terrains": terrains})
	})

	router.GET("/api/world/map-scripts", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"map_scripts": session.BattlefieldScripts()})
	})
	router.GET("/api/world/map-sizes", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"map_sizes": session.BattlefieldSizes()})
	})

	router.GET("/api/events/reason-codes", func(c *gin.Context) {
		if err := events.SeedReasonCodeCatalog(c.Request.Context(), deps.Store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		definitions, err := events.LoadReasonCodeCatalog(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"reason_codes": definitions})
	})

	router.GET("/api/items/catalog", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"items": item.Catalog()})
	})

	parseUnitCount := func(c *gin.Context) (int, bool) {
		count := session.RecommendedOpeningUnitCount()
		if raw := strings.TrimSpace(c.Query("unit_count")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 || parsed > session.MaxOpeningUnitCount() {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unit_count must be between 1 and %d", session.MaxOpeningUnitCount())})
				return 0, false
			}
			count = session.NormalizeOpeningUnitCount(parsed)
		}
		return count, true
	}
	parseMapSizeID := func(c *gin.Context) (string, bool) {
		sizeID := session.NormalizeBattlefieldSizeID(strings.TrimSpace(c.Query("map_size")))
		if raw := strings.TrimSpace(c.Query("map_size")); raw != "" && raw != sizeID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "map_size must be one of: small, medium, large"})
			return "", false
		}
		return sizeID, true
	}
	parseFogOfWarEnabled := func(c *gin.Context) (bool, bool) {
		raw := strings.TrimSpace(c.Query("fog_of_war"))
		if raw == "" {
			return false, true
		}
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "fog_of_war must be a boolean"})
			return false, false
		}
		return parsed, true
	}
	parseRandomEventsEnabled := func(c *gin.Context) (bool, bool) {
		raw := strings.TrimSpace(c.Query("random_events"))
		if raw == "" {
			return true, true
		}
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "random_events must be a boolean"})
			return false, false
		}
		return parsed, true
	}

	router.POST("/api/sessions/single-player", func(c *gin.Context) {
		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}
		mapScriptID := strings.TrimSpace(c.Query("map_script_id"))
		if mapScriptID != "" && !session.IsBattlefieldScriptID(mapScriptID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "map_script_id is invalid"})
			return
		}
		unitCount, ok := parseUnitCount(c)
		if !ok {
			return
		}
		mapSizeID, ok := parseMapSizeID(c)
		if !ok {
			return
		}
		fogOfWarEnabled, ok := parseFogOfWarEnabled(c)
		if !ok {
			return
		}
		randomEventsEnabled, ok := parseRandomEventsEnabled(c)
		if !ok {
			return
		}

		snapshot, err := newSessionService().CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogAndRandomEvents(c.Request.Context(), seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, randomEventsEnabled)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("session_created", snapshot, nil)

		c.JSON(http.StatusCreated, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/duel", func(c *gin.Context) {
		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}
		mapScriptID := strings.TrimSpace(c.Query("map_script_id"))
		if mapScriptID != "" && !session.IsBattlefieldScriptID(mapScriptID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "map_script_id is invalid"})
			return
		}
		unitCount, ok := parseUnitCount(c)
		if !ok {
			return
		}
		mapSizeID, ok := parseMapSizeID(c)
		if !ok {
			return
		}
		fogOfWarEnabled, ok := parseFogOfWarEnabled(c)
		if !ok {
			return
		}
		randomEventsEnabled, ok := parseRandomEventsEnabled(c)
		if !ok {
			return
		}
		creatorRole := normalizeDuelRole(c.Query("creator_role"))

		service := newSessionService()
		snapshot, err := service.CreateDuelWithMapScriptSizeUnitCountFogAndRandomEvents(c.Request.Context(), seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, randomEventsEnabled)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		playerToken := uuid.NewString()
		enemyToken := uuid.NewString()
		room, roomErr := duelAuth.register(c.Request.Context(), snapshot.ID, playerToken, enemyToken, creatorRole)
		if roomErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": roomErr.Error()})
			return
		}
		broadcastSessionSnapshot("session_created_duel", snapshot, map[string]any{
			"room_status": room.status(),
		})
		commanderFactionID := snapshot.PlayerFactionID
		if creatorRole == duelRoleEnemy {
			commanderFactionID = snapshot.EnemyFactionID
		}

		c.JSON(http.StatusCreated, gin.H{
			"session":              publicForRequest(c, snapshot),
			"mode":                 "duel",
			"room_code":            room.RoomCode,
			"player_role_token":    playerToken,
			"enemy_role_token":     enemyToken,
			"commander_faction_id": commanderFactionID,
			"room_status":          room.status(),
		})
	})

	router.POST("/api/sessions/duel/join", func(c *gin.Context) {
		var request struct {
			RoomCode      string `json:"room_code"`
			PreferredRole string `json:"preferred_role"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid duel join payload"})
			return
		}

		sessionID, roleToken, role, room, ok := duelAuth.joinByRoomCode(c.Request.Context(), request.RoomCode, request.PreferredRole)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "room_code is invalid"})
			return
		}
		snapshot, err := newSessionService().GetSnapshot(c.Request.Context(), sessionID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		commanderFactionID := snapshot.PlayerFactionID
		if role == duelRoleEnemy {
			commanderFactionID = snapshot.EnemyFactionID
		}
		broadcastSessionSnapshot("duel_room_joined", snapshot, map[string]any{
			"room_status": room.status(),
			"joined_role": role,
		})
		c.JSON(http.StatusOK, gin.H{
			"session":              publicForRequest(c, snapshot),
			"mode":                 "duel",
			"room_code":            room.RoomCode,
			"role":                 role,
			"role_token":           roleToken,
			"commander_faction_id": commanderFactionID,
			"room_status":          room.status(),
		})
	})

	router.POST("/api/accounts/register", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		var request struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid register payload"})
			return
		}
		user, err := deps.Accounts.Register(c.Request.Context(), request.Username, request.DisplayName, request.Password)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		login, err := deps.Accounts.Login(c.Request.Context(), user.Username, request.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"user": user, "auth": login})
	})

	router.POST("/api/accounts/login", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		var request struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid login payload"})
			return
		}
		login, err := deps.Accounts.Login(c.Request.Context(), request.Username, request.Password)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"auth": login, "user": login.User})
	})

	router.GET("/api/accounts/me", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		token := account.ExtractBearerToken(c.GetHeader("Authorization"))
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		user, err := deps.Accounts.CurrentUser(c.Request.Context(), token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"user": user})
	})

	router.POST("/api/accounts/logout", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		token := account.ExtractBearerToken(c.GetHeader("Authorization"))
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		if err := deps.Accounts.Logout(c.Request.Context(), token); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	router.GET("/api/sessions/:id", func(c *gin.Context) {
		snapshot, err := newSessionService().GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		commanderFactionID, ok := resolveCommanderFaction(c, snapshot.ID, snapshot.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}
		previousRoomStatus, hadPreviousRoomStatus := duelAuth.roomStatusForSession(c.Request.Context(), snapshot.ID)
		roomStatus, hasRoomStatus := duelAuth.markJoinedBySessionRole(c.Request.Context(), snapshot.ID, commanderFactionID)
		if !hasRoomStatus {
			roomStatus, _ = duelAuth.roomStatusForSession(c.Request.Context(), snapshot.ID)
		} else if !hadPreviousRoomStatus || previousRoomStatus.PlayerJoined != roomStatus.PlayerJoined || previousRoomStatus.EnemyJoined != roomStatus.EnemyJoined {
			broadcastSessionSnapshot("duel_room_joined", snapshot, map[string]any{
				"room_status": roomStatus,
				"joined_role": commanderFactionID,
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"session":              publicForRequest(c, snapshot),
			"room_code":            duelAuth.roomCodeForSession(c.Request.Context(), snapshot.ID),
			"commander_faction_id": commanderFactionID,
			"room_status":          roomStatus,
		})
	})

	router.GET("/api/sessions/:id/reconnect", func(c *gin.Context) {
		reconnect, err := newSessionService().GetReconnectSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		includeDebug := debugSnapshotRequested(c)
		reconnect.Session = session.PublicSnapshot(reconnect.Session, includeDebug)
		reconnect.BoundarySession = session.PublicSnapshot(reconnect.BoundarySession, includeDebug)

		c.JSON(http.StatusOK, gin.H{"reconnect": reconnect})
	})

	router.POST("/api/sessions/:id/directive", func(c *gin.Context) {
		var request struct {
			Text   string `json:"text"`
			Scope  string `json:"scope"`
			UnitID string `json:"unit_id"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid directive payload"})
			return
		}

		service := newSessionService()
		current, err := service.GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		commanderFactionID, ok := resolveCommanderFaction(c, current.ID, current.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}
		scope := strings.ToLower(strings.TrimSpace(request.Scope))
		var (
			snapshot session.Snapshot
		)
		switch scope {
		case "", string(session.DirectiveKindDoctrine):
			snapshot, err = service.SetFactionGlobalDirective(c.Request.Context(), c.Param("id"), commanderFactionID, request.Text)
		case string(session.DirectiveKindTask):
			snapshot, err = service.SetFactionDirective(
				c.Request.Context(),
				c.Param("id"),
				commanderFactionID,
				session.DirectiveKindTask,
				request.UnitID,
				request.Text,
			)
		case string(session.DirectiveKindOrder):
			snapshot, err = service.SetFactionDirective(
				c.Request.Context(),
				c.Param("id"),
				commanderFactionID,
				session.DirectiveKindOrder,
				request.UnitID,
				request.Text,
			)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "directive scope must be doctrine/task/order"})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("directive_updated", snapshot, map[string]any{
			"scope": scope,
		})

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/:id/opening-draft", func(c *gin.Context) {
		var request struct {
			Units []unit.Record `json:"units"`
		}
		_ = c.ShouldBindJSON(&request)
		service := newSessionService()
		snapshot, err := service.ApplyOpeningDraft(c.Request.Context(), c.Param("id"), request.Units)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("opening_draft_confirmed", snapshot, nil)
		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/:id/faction-relation", func(c *gin.Context) {
		var request struct {
			LeftFactionID  string `json:"left_faction_id"`
			RightFactionID string `json:"right_faction_id"`
			State          string `json:"state"`
			Reason         string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid faction relation payload"})
			return
		}

		snapshot, err := newSessionService().SetFactionRelation(
			c.Request.Context(),
			c.Param("id"),
			request.LeftFactionID,
			request.RightFactionID,
			session.FactionRelationState(request.State),
			request.Reason,
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("faction_relation_updated", snapshot, map[string]any{
			"left_faction_id":  strings.TrimSpace(request.LeftFactionID),
			"right_faction_id": strings.TrimSpace(request.RightFactionID),
			"state":            strings.ToLower(strings.TrimSpace(request.State)),
		})

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/:id/dialogue", func(c *gin.Context) {
		var request struct {
			UnitID  string `json:"unit_id"`
			Message string `json:"message"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dialogue payload"})
			return
		}
		service := newSessionService()
		current, err := service.GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		commanderFactionID, ok := resolveCommanderFaction(c, current.ID, current.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}
		if strings.TrimSpace(request.UnitID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unit_id is required"})
			return
		}
		unitFactionID := ""
		for _, record := range current.PlayerUnits {
			if record.ID == request.UnitID {
				unitFactionID = record.FactionID
				break
			}
		}
		if unitFactionID == "" {
			for _, record := range current.EnemyUnits {
				if record.ID == request.UnitID {
					unitFactionID = record.FactionID
					break
				}
			}
		}
		if unitFactionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unit_id is invalid"})
			return
		}
		if unitFactionID != commanderFactionID {
			c.JSON(http.StatusForbidden, gin.H{"error": "unit does not belong to commander faction"})
			return
		}

		snapshot, reply, err := service.TalkToUnit(
			c.Request.Context(),
			c.Param("id"),
			request.UnitID,
			request.Message,
		)
		if err != nil {
			body := gin.H{"error": err.Error()}
			if snapshot.ID != "" {
				body["session"] = publicForRequest(c, snapshot)
			}
			c.JSON(http.StatusBadRequest, body)
			return
		}
		broadcastSessionSnapshot("dialogue", snapshot, map[string]any{
			"reply": reply,
		})

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot), "reply": reply})
	})

	router.POST("/api/sessions/:id/reports", func(c *gin.Context) {
		var request struct {
			Reporter string `json:"reporter"`
			UnitID   string `json:"unit_id"`
			Category string `json:"category"`
			Detail   string `json:"detail"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid report payload"})
			return
		}

		service := newSessionService()
		snapshot, report, err := service.SubmitModerationReport(
			c.Request.Context(),
			c.Param("id"),
			request.Reporter,
			request.UnitID,
			request.Category,
			request.Detail,
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("moderation_report", snapshot, map[string]any{
			"report": report,
		})

		c.JSON(http.StatusCreated, gin.H{"session": publicForRequest(c, snapshot), "report": report})
	})

	router.GET("/api/sessions/:id/audit", func(c *gin.Context) {
		limit := 80
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be an integer"})
				return
			}
			limit = parsed
		}

		audit, err := newSessionService().GetAuditBundle(c.Request.Context(), c.Param("id"), limit)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"audit": audit})
	})

	router.POST("/api/sessions/:id/privacy/erase", func(c *gin.Context) {
		var request struct {
			EraseDialogue   bool `json:"erase_dialogue"`
			EraseLLMDetails bool `json:"erase_llm_details"`
			EraseAuditTrail bool `json:"erase_audit_trail"`
			EraseMemories   bool `json:"erase_memories"`
			EraseReports    bool `json:"erase_reports"`
		}
		_ = c.ShouldBindJSON(&request)

		service := newSessionService()
		snapshot, result, err := service.EraseSessionPrivateData(
			c.Request.Context(),
			c.Param("id"),
			session.PrivacyEraseOptions{
				EraseDialogue:   request.EraseDialogue,
				EraseLLMDetails: request.EraseLLMDetails,
				EraseAuditTrail: request.EraseAuditTrail,
				EraseMemories:   request.EraseMemories,
				EraseReports:    request.EraseReports,
			},
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("privacy_erased", snapshot, map[string]any{
			"privacy_erase_result": result,
		})
		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot), "result": result})
	})

	router.POST("/api/privacy/purge", func(c *gin.Context) {
		var request struct {
			RetentionDays int `json:"retention_days"`
			Limit         int `json:"limit"`
		}
		_ = c.ShouldBindJSON(&request)

		result, err := newSessionService().PurgeExpiredSessionData(
			c.Request.Context(),
			request.RetentionDays,
			request.Limit,
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/sessions/:id/advance-phase", func(c *gin.Context) {
		service := newSessionService()
		current, err := service.GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		commanderFactionID, ok := resolveCommanderFaction(c, current.ID, current.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}

		snapshot, advanced, err := service.RequestAdvancePhase(c.Request.Context(), c.Param("id"), commanderFactionID)
		if err != nil {
			body := gin.H{"error": err.Error()}
			if snapshot.ID != "" {
				body["session"] = publicForRequest(c, snapshot)
			}
			c.JSON(http.StatusBadRequest, body)
			return
		}
		reason := "phase_ready"
		if advanced {
			reason = "phase_advanced"
		}
		broadcastSessionSnapshot(reason, snapshot, nil)

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/units/bootstrap", func(c *gin.Context) {
		name := c.DefaultQuery("name", "Prototype Unit")
		sessionID := c.DefaultQuery("session_id", "prototype-session")
		factionID := c.DefaultQuery("faction_id", "prototype-faction")
		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}

		repository := unit.NewRepository(deps.Store)
		record := unit.BootstrapRecord(seed, sessionID, factionID, name)
		if raw := c.Query("q"); raw != "" {
			q, err := strconv.Atoi(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "q must be an integer"})
				return
			}
			record.Status.PositionQ = q
		}
		if raw := c.Query("r"); raw != "" {
			r, err := strconv.Atoi(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "r must be an integer"})
				return
			}
			record.Status.PositionR = r
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"unit": record})
	})

	router.POST("/api/units/:id/mutations", func(c *gin.Context) {
		field := status.Field(c.Query("field"))
		if field == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "field is required"})
			return
		}
		delta, err := strconv.ParseFloat(c.DefaultQuery("delta", "0"), 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "delta must be numeric"})
			return
		}
		turn, err := strconv.Atoi(c.DefaultQuery("turn", "1"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "turn must be an integer"})
			return
		}

		mutator := status.NewMutator(deps.Store, unit.NewRepository(deps.Store))
		result, err := mutator.Apply(c.Request.Context(), status.Mutation{
			UnitID:       c.Param("id"),
			Turn:         turn,
			Field:        field,
			Delta:        delta,
			ReasonCode:   events.ReasonCode(c.Query("reason_code")),
			ReasonText:   c.Query("reason_text"),
			Location:     c.Query("location"),
			EmotionalTag: c.Query("emotional_tag"),
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/units/:id/rewards/grant", func(c *gin.Context) {
		gold, err := strconv.ParseFloat(c.DefaultQuery("gold", "0"), 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "gold must be numeric"})
			return
		}
		turn, err := strconv.Atoi(c.DefaultQuery("turn", "1"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "turn must be an integer"})
			return
		}

		mutator := status.NewMutator(deps.Store, unit.NewRepository(deps.Store))
		result, err := mutator.Apply(c.Request.Context(), status.Mutation{
			UnitID:     c.Param("id"),
			Turn:       turn,
			Field:      status.FieldWallet,
			Delta:      gold,
			ReasonCode: events.ReasonEconomyReward,
			ReasonText: c.Query("reason_text"),
			Location:   c.Query("location"),
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/combat/loot/resolve", func(c *gin.Context) {
		killerID := c.Query("killer_id")
		victimID := c.Query("victim_id")
		if killerID == "" || victimID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "killer_id and victim_id are required"})
			return
		}

		service := combat.NewLootInheritor(deps.Store, unit.NewRepository(deps.Store), deps.AI)
		result, err := service.Resolve(c.Request.Context(), combat.ResolveRequest{
			KillerUnitID: killerID,
			VictimUnitID: victimID,
			Location:     c.DefaultQuery("location", "hex_0_0"),
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/units/:id/life/down", func(c *gin.Context) {
		repository := unit.NewRepository(deps.Store)
		record, err := repository.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := unit.ApplyFatalDamage(&record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"unit": record})
	})

	router.POST("/api/units/:id/life/rescue", func(c *gin.Context) {
		repository := unit.NewRepository(deps.Store)
		record, err := repository.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := unit.Rescue(&record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"unit": record})
	})

	router.POST("/api/units/:id/life/self-revive", func(c *gin.Context) {
		repository := unit.NewRepository(deps.Store)
		record, err := repository.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := unit.SelfRevive(&record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"unit": record})
	})

	router.POST("/api/world/bootstrap", func(c *gin.Context) {
		if err := world.SeedTerrainCatalog(c.Request.Context(), deps.Store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}

		snapshot := world.GenerateMap(seed, world.DefaultMapWidth, world.DefaultMapHeight)
		if err := world.SaveMap(c.Request.Context(), deps.Store, snapshot); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		turnState := turns.NewState(time.Now().UTC(), turns.DefaultBudgets())

		c.JSON(http.StatusCreated, gin.H{
			"map": gin.H{
				"id":             snapshot.ID,
				"seed":           snapshot.Seed,
				"width":          snapshot.Width,
				"height":         snapshot.Height,
				"generated_at":   snapshot.GeneratedAt,
				"tile_count":     len(snapshot.Tiles),
				"terrain_counts": snapshot.Counts,
			},
			"turn": turnState,
		})
	})

	router.GET("/api/world/maps/latest", func(c *gin.Context) {
		summary, err := world.LoadLatestMapSummary(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"map": summary})
	})

	router.GET("/api/world/maps/latest/fov", func(c *gin.Context) {
		snapshot, err := world.LoadLatestMapSnapshot(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		q, err := strconv.Atoi(c.DefaultQuery("q", "0"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "q must be an integer"})
			return
		}
		r, err := strconv.Atoi(c.DefaultQuery("r", "0"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "r must be an integer"})
			return
		}
		baseRange, err := strconv.Atoi(c.DefaultQuery("base_range", "5"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "base_range must be an integer"})
			return
		}

		visible, err := world.ComputeVisibleTiles(snapshot, world.Coord{Q: q, R: r}, baseRange)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"origin":        gin.H{"q": q, "r": r},
			"base_range":    baseRange,
			"visible_count": len(visible),
			"visible_tiles": visible,
		})
	})

	// C-15: client only sends input, server remains the authoritative state owner.
	router.GET("/ws", hub.Handle)

	return router
}
