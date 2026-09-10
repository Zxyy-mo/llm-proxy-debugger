package hub

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Hub WebSocket 广播中心
type Hub struct {
	clients    map[*websocket.Conn]bool
	Broadcast  chan interface{}
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mu         sync.Mutex
	logger     *zap.Logger
}

// Publish must never hold up a proxy deadline or a disconnected HTTP client.
// The UI recovers dropped notifications from authoritative HTTP snapshots.
func (h *Hub) Publish(message interface{}) {
	select {
	case h.Broadcast <- message:
	default:
	}
}

// New 创建并返回一个新的 Hub
func New(logger *zap.Logger) *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		Broadcast:  make(chan interface{}, 256),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		logger:     logger,
	}
}

// Run 启动 Hub 的事件循环（应在 goroutine 中运行）
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Close()
			}
			h.mu.Unlock()
		case message := <-h.Broadcast:
			h.mu.Lock()
			for client := range h.clients {
				client.SetWriteDeadline(time.Now().Add(3 * time.Second))
				err := client.WriteJSON(message)
				if err != nil {
					client.Close()
					delete(h.clients, client)
				}
			}
			h.mu.Unlock()
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ServeWS 处理 WebSocket 升级请求
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("WebSocket upgrade failed", zap.Error(err))
		return
	}
	h.register <- conn
}
