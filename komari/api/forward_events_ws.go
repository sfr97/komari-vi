package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/komari-monitor/komari/database/config"
	"github.com/komari-monitor/komari/ws"
)

// ForwardEventsWS 提供给前端的转发事件推送通道（如 forward_config_updated）。
// 该连接仅用于服务端主动推送；客户端可不发送任何消息。
func ForwardEventsWS(c *gin.Context) {
	cfg, _ := config.Get()
	if !websocket.IsWebSocketUpgrade(c.Request) {
		RespondError(c, http.StatusBadRequest, "Require WebSocket upgrade")
		return
	}
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			if cfg.AllowCors {
				return true
			}
			return ws.CheckOrigin(r)
		},
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		RespondError(c, http.StatusBadRequest, "Failed to upgrade to WebSocket")
		return
	}
	defer conn.Close()

	ws.AddConnectedUser(conn)
	defer ws.RemoveConnectedUser(conn)

	// 阻塞读循环，用于感知断开；客户端发送的消息内容不做处理。
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
