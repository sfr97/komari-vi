package forward

// RuleConfig 映射 forward_rules.config_json
type RuleConfig struct {
	Type             string         `json:"type"`
	EntryNodeID      string         `json:"entry_node_id"`
	EntryPort        string         `json:"entry_port"`
	EntryCurrentPort int            `json:"entry_current_port"`
	Protocol         string         `json:"protocol"`
	Relays           []RelayNode    `json:"relays"`
	Strategy         string         `json:"strategy"`
	ActiveRelayNode  string         `json:"active_relay_node_id"`
	Network          *NetworkConfig `json:"network,omitempty"`
	TargetType       string         `json:"target_type"`
	TargetNodeID     string         `json:"target_node_id"`
	TargetHost       string         `json:"target_host"`
	TargetPort       int            `json:"target_port"`
	Hops             []ChainHop     `json:"hops"`
}

type RelayNode struct {
	NodeID      string `json:"node_id"`
	Port        string `json:"port"`
	CurrentPort int    `json:"current_port"`
	SortOrder   int    `json:"sort_order"`
}

type ChainHop struct {
	Type            string         `json:"type"` // direct / relay_group
	NodeID          string         `json:"node_id"`
	Port            string         `json:"port"`
	CurrentPort     int            `json:"current_port"`
	Relays          []RelayNode    `json:"relays"`
	Strategy        string         `json:"strategy"`
	ActiveRelayNode string         `json:"active_relay_node_id"`
	Network         *NetworkConfig `json:"network,omitempty"`
	SortOrder       int            `json:"sort_order"`
	TargetType      string         `json:"target_type"`
	TargetNodeID    string         `json:"target_node_id"`
	TargetHost      string         `json:"target_host"`
	TargetPort      int            `json:"target_port"`
}

// NetworkConfig maps to realm's [network] options (subset).
// We use pointers so explicit 0 can be preserved (and not dropped by omitempty).
type NetworkConfig struct {
	FailoverProbeIntervalMs   *uint64 `json:"failover_probe_interval_ms,omitempty"`
	FailoverProbeTimeoutMs    *uint64 `json:"failover_probe_timeout_ms,omitempty"`
	FailoverFailfastTimeoutMs *uint64 `json:"failover_failfast_timeout_ms,omitempty"`
	FailoverOkTTLms           *uint64 `json:"failover_ok_ttl_ms,omitempty"`
	FailoverBackoffBaseMs     *uint64 `json:"failover_backoff_base_ms,omitempty"`
	FailoverBackoffMaxMs      *uint64 `json:"failover_backoff_max_ms,omitempty"`
	FailoverRetryWindowMs     *uint64 `json:"failover_retry_window_ms,omitempty"`
	FailoverRetrySleepMs      *uint64 `json:"failover_retry_sleep_ms,omitempty"`
}

// NodeResolver 解析 nodeID -> IP/域名
type NodeResolver func(nodeID string) (string, error)
