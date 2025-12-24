package models

// AgentConnectionLog 记录 agent 与主控之间（/api/clients/report）的 WebSocket 连接会话信息
// - ConnectedAt: 连接建立时间
// - DisconnectedAt/OnlineSeconds: 断开时补齐；为空表示当前仍在线
type AgentConnectionLog struct {
	ID             uint       `json:"id,omitempty" gorm:"primaryKey;autoIncrement"`
	Client         string     `json:"client" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_client_conn"`
	ConnectionID   int64      `json:"connection_id" gorm:"not null;index;uniqueIndex:idx_client_conn"`
	ConnectedAt    LocalTime  `json:"connected_at" gorm:"not null;index"`
	DisconnectedAt *LocalTime `json:"disconnected_at,omitempty" gorm:"index"`
	OnlineSeconds  *int64     `json:"online_seconds,omitempty" gorm:"type:bigint"`
}
