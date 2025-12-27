package forward

import "encoding/json"

// TaskType 统一的转发任务类型
type TaskType string

const (
	TaskCheckPort TaskType = "CHECK_PORT"

	TaskRealmApiEnsure TaskType = "REALM_API_ENSURE"

	TaskRealmInstanceApply          TaskType = "REALM_INSTANCE_APPLY"
	TaskRealmInstanceStatsGet       TaskType = "REALM_INSTANCE_STATS_GET"
	TaskRealmInstanceConnectionsGet TaskType = "REALM_INSTANCE_CONNECTIONS_GET"
	TaskRealmInstanceRouteGet       TaskType = "REALM_INSTANCE_ROUTE_GET"

	TaskTestConnectivity TaskType = "TEST_CONNECTIVITY"
)

// TaskEnvelope 任务封装，便于 WS / HTTP 传输
type TaskEnvelope struct {
	TaskID   string          `json:"task_id"`
	TaskType TaskType        `json:"task_type"`
	Payload  json.RawMessage `json:"payload"`
}

// --- 请求与响应结构定义 ---

type CheckPortRequest struct {
	PortSpec      string `json:"port_spec"`                // 固定端口 | "8000-9000" | "8881,8882,8883"
	ExcludedPorts []int  `json:"excluded_ports,omitempty"` // 可选：需要排除的端口
}

type CheckPortResponse struct {
	Success       bool   `json:"success"`
	AvailablePort *int   `json:"available_port,omitempty"`
	Message       string `json:"message"`
}

type RealmApiEnsureRequest struct {
	RealmDownloadURL string `json:"realm_download_url"`
	ForceReinstall   bool   `json:"force_reinstall,omitempty"`
	Restart          bool   `json:"restart,omitempty"`
}

type RealmApiEnsureResponse struct {
	Success      bool   `json:"success"`
	Pid          int    `json:"pid,omitempty"`
	Port         int    `json:"port,omitempty"`
	RealmVersion string `json:"realm_version,omitempty"`
	Message      string `json:"message"`
}

type RealmInstanceApplyRequest struct {
	RuleID uint                   `json:"rule_id"`
	NodeID string                 `json:"node_id"`
	Ops    []RealmInstanceApplyOp `json:"ops"`
}

type RealmInstanceApplyOp struct {
	Op         string          `json:"op"`
	InstanceID string          `json:"instance_id"`
	Config     json.RawMessage `json:"config,omitempty"`
}

type RealmInstanceApplyResponse struct {
	Success bool                         `json:"success"`
	Results []RealmInstanceApplyOpResult `json:"results,omitempty"`
	Message string                       `json:"message"`
}

type RealmInstanceApplyOpResult struct {
	Op         string `json:"op"`
	InstanceID string `json:"instance_id"`
	Success    bool   `json:"success"`
	Message    string `json:"message,omitempty"`
}

type RealmInstanceStatsGetRequest struct {
	InstanceIDs []string `json:"instance_ids"`
}

type RealmInstanceStatsGetResponse struct {
	Success         bool                       `json:"success"`
	StatsByInstance map[string]json.RawMessage `json:"stats_by_instance,omitempty"`
	Message         string                     `json:"message"`
}

type RealmInstanceConnectionsGetRequest struct {
	InstanceID string `json:"instance_id"`
	Protocol   string `json:"protocol,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Offset     int    `json:"offset,omitempty"`
}

type RealmInstanceConnectionsGetResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Message string          `json:"message"`
}

type RealmInstanceRouteGetRequest struct {
	InstanceID string `json:"instance_id"`
}

type RealmInstanceRouteGetResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Message string          `json:"message"`
}

type TestConnectivityRequest struct {
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Timeout    int    `json:"timeout"` // 秒
}

type TestConnectivityResponse struct {
	Success   bool   `json:"success"`
	Reachable bool   `json:"reachable"`
	LatencyMs *int64 `json:"latency_ms,omitempty"`
	Message   string `json:"message"`
}
