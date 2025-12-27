package models

// ForwardInstanceStat 实例维度实时统计（避免同节点多实例覆盖）
type ForwardInstanceStat struct {
	ID            uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	RuleID        uint      `json:"rule_id" gorm:"uniqueIndex:idx_rule_node_instance,priority:1;index"`
	NodeID        string    `json:"node_id" gorm:"type:varchar(36);uniqueIndex:idx_rule_node_instance,priority:2;index"`
	InstanceID    string    `json:"instance_id" gorm:"type:varchar(128);uniqueIndex:idx_rule_node_instance,priority:3;index"`
	Listen        string    `json:"listen" gorm:"type:varchar(128)"`
	ListenPort    int       `json:"listen_port" gorm:"type:int"`
	StatsJSON     string    `json:"stats" gorm:"type:longtext"`
	RouteJSON     string    `json:"route,omitempty" gorm:"type:longtext"`
	LastUpdatedAt LocalTime `json:"last_updated_at" gorm:"type:timestamp;index"`
	CreatedAt     LocalTime `json:"created_at"`
	UpdatedAt     LocalTime `json:"updated_at"`
}
