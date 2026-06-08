package ws

// 文件说明：WebSocket Hub，管理连接生命周期、房间匹配、会话订阅与事件广播分发。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	minRoomPlayers     = 2
	maxRoomPlayers     = 6
	defaultRoomPlayers = 2
)

// Envelope 结构体用于承载该模块的核心数据。
type Envelope struct {
	Type    string `json:"type"`
	TraceID string `json:"trace_id,omitempty"`
	Payload any    `json:"payload,omitempty"`
}

// IncomingEnvelope 结构体用于承载该模块的核心数据。
type IncomingEnvelope struct {
	Type    string          `json:"type"`
	TraceID string          `json:"trace_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// socketConn 接口定义该模块需要实现的能力约束。
type socketConn interface {
	ReadJSON(v any) error
	WriteJSON(v any) error
	SetReadDeadline(time.Time) error
	SetPongHandler(func(string) error)
	Close() error
}

// clientState 结构体用于承载该模块的核心数据。
type clientState struct {
	ID          string
	Name        string
	Conn        socketConn
	ConnectedAt time.Time

	RoomID    string
	Queued    bool
	MatchSize int
	writeM    sync.Mutex
}

// roomState 结构体用于承载该模块的核心数据。
type roomState struct {
	ID         string
	PlayerIDs  []string
	MaxPlayers int
	CreatedAt  time.Time
}

// delivery 结构体用于承载该模块的核心数据。
type delivery struct {
	TargetID string
	Envelope Envelope
}

// Hub 结构体用于承载该模块的核心数据。
type Hub struct {
	logger   *slog.Logger
	upgrader websocket.Upgrader
	mu       sync.RWMutex

	clientsByID    map[string]*clientState
	clientIDByConn map[socketConn]string
	rooms          map[string]*roomState
	matchmakingQ   []string

	sessionSubscribers map[string]map[string]struct{}
	clientSessions     map[string]map[string]struct{}
}

// NewHub 创建 WebSocket Hub，并初始化客户端、房间、匹配队列与会话订阅表。
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		logger: logger,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(*http.Request) bool {
				return true
			},
		},
		clientsByID:        map[string]*clientState{},
		clientIDByConn:     map[socketConn]string{},
		rooms:              map[string]*roomState{},
		matchmakingQ:       []string{},
		sessionSubscribers: map[string]map[string]struct{}{},
		clientSessions:     map[string]map[string]struct{}{},
	}
}

// Handle 处理单条 WebSocket 连接生命周期。
// 包含注册、消息收发、路由分发、回执与断连清理。
func (h *Hub) Handle(c *gin.Context) {
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.logger.Error("upgrade websocket", "error", err)
		return
	}

	clientID := h.register(conn, c.Query("name"))
	defer h.unregister(clientID)

	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	})

	h.deliver(delivery{
		TargetID: clientID,
		Envelope: Envelope{
			Type: "welcome",
			Payload: map[string]any{
				"message":       "qunxiang backend connected",
				"authoritative": true,
				"connected_at":  time.Now().UTC().Format(time.RFC3339),
				"client_id":     clientID,
			},
		},
	})

	for {
		var incoming IncomingEnvelope
		if err := conn.ReadJSON(&incoming); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				h.logger.Warn("read websocket message", "error", err)
			}
			return
		}
		// 任何成功入站消息（含前端每 60s 的应用层心跳 {type:"ping"}）即证明连接存活——
		// 把读超时 deadline 顺延 120s。否则 deadline 恒为连接建立时的 T0+120s（gorilla 的
		// SetReadDeadline 是绝对时刻、读成功不自动延长），且后端从不发协议级 ping 帧→PongHandler
		// 永不触发，导致活跃连接也会每约 120s 被读超时强断重连（churn，§5 风险4）。
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))

		outbound := h.handleIncoming(clientID, incoming)
		for _, item := range outbound {
			h.deliver(item)
		}

		ackType := "ack"
		if incoming.Type == "ping" {
			ackType = "pong"
		}
		h.deliver(delivery{
			TargetID: clientID,
			Envelope: Envelope{
				Type:    ackType,
				TraceID: incoming.TraceID,
				Payload: h.snapshotForClient(clientID, incoming.Type),
			},
		})
	}
}

// ClientCount 返回当前在线客户端数量。
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clientsByID)
}

// RoomCount 返回当前房间数量。
func (h *Hub) RoomCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms)
}

// MatchmakingQueueCount 返回当前匹配队列人数。
func (h *Hub) MatchmakingQueueCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.matchmakingQ)
}

// SessionSubscriptionCount 返回所有会话订阅总数。
func (h *Hub) SessionSubscriptionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	total := 0
	for _, subscribers := range h.sessionSubscribers {
		total += len(subscribers)
	}
	return total
}

// register 注册新客户端连接并返回 clientID。
func (h *Hub) register(conn socketConn, name string) string {
	now := time.Now().UTC()
	clientID := uuid.NewString()
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("guest-%s", clientID[:6])
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.clientsByID[clientID] = &clientState{
		ID:          clientID,
		Name:        name,
		Conn:        conn,
		ConnectedAt: now,
		MatchSize:   defaultRoomPlayers,
	}
	h.clientIDByConn[conn] = clientID
	h.clientSessions[clientID] = map[string]struct{}{}
	return clientID
}

// unregister 清理客户端状态。
// 包括匹配队列、房间成员、会话订阅与连接映射。
func (h *Hub) unregister(clientID string) {
	if strings.TrimSpace(clientID) == "" {
		return
	}

	h.mu.Lock()
	client := h.clientsByID[clientID]
	if client == nil {
		h.mu.Unlock()
		return
	}

	outbound := []delivery{}
	if client.Queued {
		h.removeFromQueueLocked(clientID)
		client.Queued = false
	}
	if strings.TrimSpace(client.RoomID) != "" {
		room := h.rooms[client.RoomID]
		if room != nil {
			room.PlayerIDs = removeString(room.PlayerIDs, clientID)
			if len(room.PlayerIDs) == 0 {
				delete(h.rooms, room.ID)
			} else {
				for _, peerID := range room.PlayerIDs {
					outbound = append(outbound, delivery{
						TargetID: peerID,
						Envelope: Envelope{
							Type: "peer_left_room",
							Payload: map[string]any{
								"room_id":   room.ID,
								"peer_id":   client.ID,
								"peer_name": client.Name,
							},
						},
					})
					outbound = append(outbound, delivery{
						TargetID: peerID,
						Envelope: h.roomStateEnvelopeLocked(room.ID),
					})
				}
			}
		}
	}
	if subscriptions := h.clientSessions[clientID]; len(subscriptions) > 0 {
		for sessionID := range subscriptions {
			subscribers := h.sessionSubscribers[sessionID]
			delete(subscribers, clientID)
			if len(subscribers) == 0 {
				delete(h.sessionSubscribers, sessionID)
			}
		}
	}

	delete(h.clientIDByConn, client.Conn)
	delete(h.clientsByID, clientID)
	delete(h.clientSessions, clientID)
	h.mu.Unlock()

	for _, item := range outbound {
		h.deliver(item)
	}

	_ = client.Conn.Close()
}

// deliver 向指定客户端投递消息，写失败只记录日志不抛出。
func (h *Hub) deliver(item delivery) {
	h.mu.RLock()
	client := h.clientsByID[item.TargetID]
	h.mu.RUnlock()
	if client == nil {
		return
	}

	client.writeM.Lock()
	defer client.writeM.Unlock()
	if err := client.Conn.WriteJSON(item.Envelope); err != nil {
		h.logger.Warn("write websocket message", "client_id", item.TargetID, "type", item.Envelope.Type, "error", err)
	}
}

// handleIncoming 路由并处理客户端入站消息，返回待投递消息列表。
func (h *Hub) handleIncoming(clientID string, incoming IncomingEnvelope) []delivery {
	switch incoming.Type {
	case "matchmaking_join":
		targetSize := defaultRoomPlayers
		if len(incoming.Payload) > 0 {
			var payload struct {
				TargetSize int `json:"target_size"`
			}
			if err := json.Unmarshal(incoming.Payload, &payload); err != nil {
				return []delivery{h.errorDelivery(clientID, "invalid matchmaking_join payload")}
			}
			if payload.TargetSize != 0 {
				if payload.TargetSize < minRoomPlayers || payload.TargetSize > maxRoomPlayers {
					return []delivery{h.errorDelivery(clientID, "target_size must be between 2 and 6")}
				}
				targetSize = payload.TargetSize
			}
		}
		return h.joinMatchmaking(clientID, targetSize)
	case "matchmaking_cancel":
		return h.cancelMatchmaking(clientID)
	case "room_join":
		var payload struct {
			RoomID     string `json:"room_id"`
			MaxPlayers int    `json:"max_players"`
		}
		if err := json.Unmarshal(incoming.Payload, &payload); err != nil {
			return []delivery{h.errorDelivery(clientID, "invalid room_join payload")}
		}
		if payload.MaxPlayers != 0 && (payload.MaxPlayers < minRoomPlayers || payload.MaxPlayers > maxRoomPlayers) {
			return []delivery{h.errorDelivery(clientID, "max_players must be between 2 and 6")}
		}
		return h.joinRoom(clientID, payload.RoomID, payload.MaxPlayers)
	case "room_leave":
		return h.leaveRoom(clientID)
	case "room_message":
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(incoming.Payload, &payload); err != nil {
			return []delivery{h.errorDelivery(clientID, "invalid room_message payload")}
		}
		return h.publishRoomMessage(clientID, payload.Message)
	case "room_sync":
		return h.syncRoom(clientID)
	case "session_subscribe":
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(incoming.Payload, &payload); err != nil {
			return []delivery{h.errorDelivery(clientID, "invalid session_subscribe payload")}
		}
		return h.subscribeSession(clientID, payload.SessionID)
	case "session_unsubscribe":
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(incoming.Payload, &payload); err != nil {
			return []delivery{h.errorDelivery(clientID, "invalid session_unsubscribe payload")}
		}
		return h.unsubscribeSession(clientID, payload.SessionID)
	case "ping":
		return nil
	default:
		return []delivery{h.errorDelivery(clientID, fmt.Sprintf("unsupported message type: %s", incoming.Type))}
	}
}

// joinMatchmaking 把客户端加入匹配队列，并在人数满足时组房间发放 match_found。
func (h *Hub) joinMatchmaking(clientID string, targetSize int) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	if client.RoomID != "" {
		return []delivery{h.errorDeliveryLocked(clientID, "already in room")}
	}
	if targetSize < minRoomPlayers || targetSize > maxRoomPlayers {
		targetSize = defaultRoomPlayers
	}
	if client.Queued {
		if client.MatchSize != targetSize {
			h.removeFromQueueLocked(clientID)
			client.Queued = false
		} else {
			return []delivery{{
				TargetID: clientID,
				Envelope: Envelope{
					Type: "matchmaking_queued",
					Payload: map[string]any{
						"position":    h.queuePositionLocked(clientID),
						"target_size": client.MatchSize,
					},
				},
			}}
		}
	}

	client.MatchSize = targetSize
	client.Queued = true
	h.matchmakingQ = append(h.matchmakingQ, clientID)

	outbound := []delivery{{
		TargetID: clientID,
		Envelope: Envelope{
			Type: "matchmaking_queued",
			Payload: map[string]any{
				"position":    h.queuePositionLocked(clientID),
				"target_size": targetSize,
			},
		},
	}}

	for {
		groupIDs := h.dequeueMatchmakingGroupLocked(targetSize)
		if len(groupIDs) < targetSize {
			break
		}

		roomID := fmt.Sprintf("room-%s", uuid.NewString()[:8])
		room := &roomState{
			ID:         roomID,
			PlayerIDs:  append([]string{}, groupIDs...),
			MaxPlayers: targetSize,
			CreatedAt:  time.Now().UTC(),
		}
		h.rooms[roomID] = room

		for _, memberID := range groupIDs {
			member := h.clientsByID[memberID]
			if member == nil {
				continue
			}
			member.RoomID = roomID
			member.MatchSize = targetSize
		}

		for _, memberID := range groupIDs {
			outbound = append(outbound, h.matchFoundDeliveryLocked(memberID, groupIDs, roomID, targetSize))
			outbound = append(outbound, delivery{TargetID: memberID, Envelope: h.roomStateEnvelopeLocked(roomID)})
		}
	}

	return outbound
}

// cancelMatchmaking 取消客户端匹配状态。
func (h *Hub) cancelMatchmaking(clientID string) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	if !client.Queued {
		return []delivery{{
			TargetID: clientID,
			Envelope: Envelope{
				Type: "matchmaking_idle",
			},
		}}
	}

	h.removeFromQueueLocked(clientID)
	client.Queued = false
	return []delivery{{
		TargetID: clientID,
		Envelope: Envelope{
			Type: "matchmaking_canceled",
		},
	}}
}

// joinRoom 让客户端加入/创建指定房间，并广播最新房间状态。
func (h *Hub) joinRoom(clientID string, roomID string, maxPlayers int) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return []delivery{h.errorDeliveryLocked(clientID, "room_id is required")}
	}
	if client.RoomID != "" && client.RoomID != roomID {
		return []delivery{h.errorDeliveryLocked(clientID, "already in another room")}
	}
	if client.Queued {
		h.removeFromQueueLocked(clientID)
		client.Queued = false
	}

	room := h.rooms[roomID]
	if room == nil {
		if maxPlayers < minRoomPlayers || maxPlayers > maxRoomPlayers {
			maxPlayers = defaultRoomPlayers
		}
		room = &roomState{
			ID:         roomID,
			PlayerIDs:  []string{},
			MaxPlayers: maxPlayers,
			CreatedAt:  time.Now().UTC(),
		}
		h.rooms[roomID] = room
	}
	if room.MaxPlayers < minRoomPlayers || room.MaxPlayers > maxRoomPlayers {
		room.MaxPlayers = defaultRoomPlayers
	}

	if !containsString(room.PlayerIDs, clientID) {
		if len(room.PlayerIDs) >= room.MaxPlayers {
			return []delivery{h.errorDeliveryLocked(clientID, "room is full")}
		}
		room.PlayerIDs = append(room.PlayerIDs, clientID)
	}
	client.RoomID = roomID

	outbound := []delivery{}
	for _, memberID := range room.PlayerIDs {
		outbound = append(outbound, delivery{TargetID: memberID, Envelope: h.roomStateEnvelopeLocked(roomID)})
	}
	if len(room.PlayerIDs) == room.MaxPlayers {
		for _, memberID := range room.PlayerIDs {
			outbound = append(outbound, delivery{
				TargetID: memberID,
				Envelope: Envelope{
					Type: "room_ready",
					Payload: map[string]any{
						"room_id":     roomID,
						"max_players": room.MaxPlayers,
					},
				},
			})
		}
	}
	return outbound
}

// leaveRoom 将客户端移出房间并向其他成员广播离开事件。
func (h *Hub) leaveRoom(clientID string) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	if client.RoomID == "" {
		return []delivery{{
			TargetID: clientID,
			Envelope: Envelope{
				Type: "room_idle",
			},
		}}
	}

	roomID := client.RoomID
	room := h.rooms[roomID]
	client.RoomID = ""
	if room == nil {
		return []delivery{{
			TargetID: clientID,
			Envelope: Envelope{
				Type: "room_left",
				Payload: map[string]any{
					"room_id": roomID,
				},
			},
		}}
	}

	room.PlayerIDs = removeString(room.PlayerIDs, clientID)
	outbound := []delivery{{
		TargetID: clientID,
		Envelope: Envelope{
			Type: "room_left",
			Payload: map[string]any{
				"room_id": roomID,
			},
		},
	}}

	if len(room.PlayerIDs) == 0 {
		delete(h.rooms, roomID)
		return outbound
	}
	for _, peerID := range room.PlayerIDs {
		outbound = append(outbound, delivery{
			TargetID: peerID,
			Envelope: Envelope{
				Type: "peer_left_room",
				Payload: map[string]any{
					"room_id":   roomID,
					"peer_id":   client.ID,
					"peer_name": client.Name,
				},
			},
		})
		outbound = append(outbound, delivery{TargetID: peerID, Envelope: h.roomStateEnvelopeLocked(roomID)})
	}

	return outbound
}

// publishRoomMessage 在房间内广播聊天消息（含发送者信息与时间戳）。
func (h *Hub) publishRoomMessage(clientID string, message string) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	roomID := strings.TrimSpace(client.RoomID)
	if roomID == "" {
		return []delivery{h.errorDeliveryLocked(clientID, "not in room")}
	}
	room := h.rooms[roomID]
	if room == nil {
		return []delivery{h.errorDeliveryLocked(clientID, "room not found")}
	}

	message = strings.TrimSpace(message)
	if message == "" {
		return []delivery{h.errorDeliveryLocked(clientID, "message is empty")}
	}
	message = limitRunes(message, 160)

	outbound := make([]delivery, 0, len(room.PlayerIDs))
	for _, peerID := range room.PlayerIDs {
		outbound = append(outbound, delivery{
			TargetID: peerID,
			Envelope: Envelope{
				Type: "room_message",
				Payload: map[string]any{
					"room_id":     roomID,
					"sender_id":   client.ID,
					"sender_name": client.Name,
					"message":     message,
					"sent_at":     time.Now().UTC().Format(time.RFC3339Nano),
				},
			},
		})
	}
	return outbound
}

// syncRoom 向请求方回传其所在房间状态；若未入房则返回 room_idle 事件。
func (h *Hub) syncRoom(clientID string) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	if client.RoomID == "" {
		return []delivery{{
			TargetID: clientID,
			Envelope: Envelope{
				Type: "room_idle",
			},
		}}
	}
	return []delivery{{
		TargetID: clientID,
		Envelope: h.roomStateEnvelopeLocked(client.RoomID),
	}}
}

// subscribeSession 为客户端注册会话订阅关系，并返回当前订阅人数。
func (h *Hub) subscribeSession(clientID string, sessionID string) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return []delivery{h.errorDeliveryLocked(clientID, "session_id is required")}
	}
	if h.sessionSubscribers == nil {
		h.sessionSubscribers = map[string]map[string]struct{}{}
	}
	if h.clientSessions == nil {
		h.clientSessions = map[string]map[string]struct{}{}
	}

	subscribers := h.sessionSubscribers[sessionID]
	if subscribers == nil {
		subscribers = map[string]struct{}{}
		h.sessionSubscribers[sessionID] = subscribers
	}
	subscribers[clientID] = struct{}{}

	sessions := h.clientSessions[clientID]
	if sessions == nil {
		sessions = map[string]struct{}{}
		h.clientSessions[clientID] = sessions
	}
	sessions[sessionID] = struct{}{}

	return []delivery{{
		TargetID: clientID,
		Envelope: Envelope{
			Type: "session_subscribed",
			Payload: map[string]any{
				"session_id":       sessionID,
				"subscriber_count": len(subscribers),
			},
		},
	}}
}

// unsubscribeSession 取消会话订阅并清理空订阅集合，返回剩余订阅人数。
func (h *Hub) unsubscribeSession(clientID string, sessionID string) []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()

	client := h.clientsByID[clientID]
	if client == nil {
		return nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return []delivery{h.errorDeliveryLocked(clientID, "session_id is required")}
	}

	subscriberCount := 0
	if subscribers := h.sessionSubscribers[sessionID]; subscribers != nil {
		delete(subscribers, clientID)
		subscriberCount = len(subscribers)
		if len(subscribers) == 0 {
			delete(h.sessionSubscribers, sessionID)
		}
	}
	if sessions := h.clientSessions[clientID]; sessions != nil {
		delete(sessions, sessionID)
	}

	return []delivery{{
		TargetID: clientID,
		Envelope: Envelope{
			Type: "session_unsubscribed",
			Payload: map[string]any{
				"session_id":       sessionID,
				"subscriber_count": subscriberCount,
			},
		},
	}}
}

// BroadcastSessionEvent 向订阅某 session 的所有客户端并发广播事件。
func (h *Hub) BroadcastSessionEvent(sessionID string, eventType string, payload any) int {
	sessionID = strings.TrimSpace(sessionID)
	eventType = strings.TrimSpace(eventType)
	if sessionID == "" || eventType == "" {
		return 0
	}

	targets := h.sessionSubscriberIDs(sessionID)
	if len(targets) == 0 {
		return 0
	}

	event := Envelope{
		Type: eventType,
		Payload: map[string]any{
			"session_id": sessionID,
			"payload":    payload,
			"sent_at":    time.Now().UTC().Format(time.RFC3339Nano),
		},
	}

	var wait sync.WaitGroup
	for _, targetID := range targets {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			h.deliver(delivery{
				TargetID: id,
				Envelope: event,
			})
		}(targetID)
	}
	wait.Wait()
	return len(targets)
}

// sessionSubscriberIDs 返回某会话当前订阅者 ID 列表快照。
func (h *Hub) sessionSubscriberIDs(sessionID string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	subscribers := h.sessionSubscribers[sessionID]
	if len(subscribers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(subscribers))
	for id := range subscribers {
		ids = append(ids, id)
	}
	return ids
}

// snapshotForClient 构造 ack/pong 附带的服务端状态快照。
func (h *Hub) snapshotForClient(clientID string, receivedType string) map[string]any {
	h.mu.RLock()
	defer h.mu.RUnlock()

	payload := map[string]any{
		"received_type": receivedType,
		"client_count":  len(h.clientsByID),
		"room_count":    len(h.rooms),
		"queue_count":   len(h.matchmakingQ),
		"server_time":   time.Now().UTC().Format(time.RFC3339),
	}
	client := h.clientsByID[clientID]
	if client != nil {
		payload["client_id"] = client.ID
		payload["room_id"] = client.RoomID
		payload["queued"] = client.Queued
	}
	return payload
}

// roomStateEnvelope 线程安全获取指定房间的状态消息封装。
func (h *Hub) roomStateEnvelope(roomID string) Envelope {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.roomStateEnvelopeLocked(roomID)
}

// roomStateEnvelopeLocked 在已持锁状态下构造房间状态消息。
func (h *Hub) roomStateEnvelopeLocked(roomID string) Envelope {
	room := h.rooms[roomID]
	if room == nil {
		return Envelope{
			Type: "room_state",
			Payload: map[string]any{
				"room_id":     roomID,
				"member_cnt":  0,
				"max_players": defaultRoomPlayers,
				"ready":       false,
				"members":     []map[string]any{},
			},
		}
	}
	members := make([]map[string]any, 0, len(room.PlayerIDs))
	for _, id := range room.PlayerIDs {
		client := h.clientsByID[id]
		if client == nil {
			continue
		}
		members = append(members, map[string]any{
			"id":   client.ID,
			"name": client.Name,
		})
	}
	return Envelope{
		Type: "room_state",
		Payload: map[string]any{
			"room_id":     roomID,
			"member_cnt":  len(members),
			"max_players": room.MaxPlayers,
			"ready":       len(members) >= room.MaxPlayers,
			"members":     members,
		},
	}
}

// matchFoundDeliveryLocked 构造匹配成功通知，包含对手与同组成员信息。
func (h *Hub) matchFoundDeliveryLocked(targetID string, groupIDs []string, roomID string, targetSize int) delivery {
	peerIDs := make([]string, 0, len(groupIDs))
	peers := make([]map[string]any, 0, len(groupIDs))
	for _, memberID := range groupIDs {
		if memberID == targetID {
			continue
		}
		peerIDs = append(peerIDs, memberID)
		peerName := ""
		if peer := h.clientsByID[memberID]; peer != nil {
			peerName = peer.Name
		}
		peers = append(peers, map[string]any{
			"id":   memberID,
			"name": peerName,
		})
	}
	opponentID := ""
	opponentName := ""
	if len(peerIDs) > 0 {
		opponentID = peerIDs[0]
		opponentName = peers[0]["name"].(string)
	}
	return delivery{
		TargetID: targetID,
		Envelope: Envelope{
			Type: "matchmaking_found",
			Payload: map[string]any{
				"room_id":       roomID,
				"opponent_id":   opponentID,
				"opponent_name": opponentName,
				"peer_ids":      peerIDs,
				"peers":         peers,
				"target_size":   targetSize,
			},
		},
	}
}

// dequeueMatchmakingGroupLocked 从队列中弹出一组满足 targetSize 的匹配成员。
func (h *Hub) dequeueMatchmakingGroupLocked(targetSize int) []string {
	if targetSize < minRoomPlayers || targetSize > maxRoomPlayers {
		return nil
	}

	candidates := make([]string, 0, targetSize)
	for _, id := range h.matchmakingQ {
		client := h.clientsByID[id]
		if client == nil {
			continue
		}
		if !client.Queued || client.RoomID != "" {
			continue
		}
		if client.MatchSize != targetSize {
			continue
		}
		candidates = append(candidates, id)
		if len(candidates) == targetSize {
			break
		}
	}
	if len(candidates) < targetSize {
		return nil
	}

	selected := make(map[string]struct{}, len(candidates))
	for _, id := range candidates {
		selected[id] = struct{}{}
	}
	nextQueue := make([]string, 0, len(h.matchmakingQ)-len(candidates))
	for _, id := range h.matchmakingQ {
		if _, ok := selected[id]; ok {
			continue
		}
		nextQueue = append(nextQueue, id)
	}
	h.matchmakingQ = nextQueue

	for _, id := range candidates {
		client := h.clientsByID[id]
		if client == nil {
			continue
		}
		client.Queued = false
	}
	return candidates
}

// removeFromQueueLocked 从匹配队列移除指定 clientID。
func (h *Hub) removeFromQueueLocked(clientID string) {
	if len(h.matchmakingQ) == 0 {
		return
	}
	next := make([]string, 0, len(h.matchmakingQ))
	for _, id := range h.matchmakingQ {
		if id != clientID {
			next = append(next, id)
		}
	}
	h.matchmakingQ = next
}

// queuePositionLocked 返回客户端在队列中的 1-based 排名。
func (h *Hub) queuePositionLocked(clientID string) int {
	for index, id := range h.matchmakingQ {
		if id == clientID {
			return index + 1
		}
	}
	return 0
}

// errorDelivery 在线程安全上下文中构造 ws_error 投递消息。
func (h *Hub) errorDelivery(clientID string, message string) delivery {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.errorDeliveryLocked(clientID, message)
}

// errorDeliveryLocked 在已持锁上下文中构造 ws_error 投递消息。
func (h *Hub) errorDeliveryLocked(clientID string, message string) delivery {
	return delivery{
		TargetID: clientID,
		Envelope: Envelope{
			Type: "ws_error",
			Payload: map[string]any{
				"message": strings.TrimSpace(message),
			},
		},
	}
}

// removeString 返回去除目标值后的新切片。
func removeString(values []string, target string) []string {
	if len(values) == 0 {
		return values
	}
	next := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			next = append(next, value)
		}
	}
	return next
}

// containsString 判断切片是否包含指定字符串。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// limitRunes 截断字符串到最大 rune 数并去除首尾空白。
func limitRunes(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}
