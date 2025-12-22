package ws

import "github.com/gorilla/websocket"

// BroadcastToUsers 向所有已连接的用户（前端）广播消息
func BroadcastToUsers(message string, payload string) {
	mu.RLock()
	conns := make([]*websocket.Conn, 0, len(ConnectedUsers))
	for conn := range ConnectedUsers {
		conns = append(conns, conn)
	}
	mu.RUnlock()

	for _, conn := range conns {
		if conn == nil {
			continue
		}
		if conn.WriteMessage(websocket.TextMessage, []byte(payload)) != nil {
			RemoveConnectedUser(conn)
		}
	}
}
