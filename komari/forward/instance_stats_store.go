package forward

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	dbforward "github.com/komari-monitor/komari/database/forward"
	"github.com/komari-monitor/komari/database/models"
)

type ForwardInstanceStats struct {
	RuleID        uint            `json:"rule_id"`
	NodeID        string          `json:"node_id"`
	InstanceID    string          `json:"instance_id"`
	Listen        string          `json:"listen"`
	ListenPort    int             `json:"listen_port"`
	Stats         json.RawMessage `json:"stats"`
	Route         json.RawMessage `json:"route,omitempty"`
	LastUpdatedAt time.Time       `json:"last_updated_at"`
}

type realmInstanceStatsSummary struct {
	TotalInboundBytes  int64 `json:"total_inbound_bytes"`
	TotalOutboundBytes int64 `json:"total_outbound_bytes"`
	CurrentConnections int   `json:"current_connections"`
}

// UpdateForwardInstanceStats persists instance stats to DB and updates aggregated forward_stats (node-level).
// It also reuses existing UpdateStatsAndBroadcast to keep rule totals & WS events consistent.
func UpdateForwardInstanceStats(st ForwardInstanceStats) error {
	st.NodeID = strings.TrimSpace(st.NodeID)
	st.InstanceID = strings.TrimSpace(st.InstanceID)
	if st.RuleID == 0 || st.NodeID == "" || st.InstanceID == "" {
		return nil
	}

	record := &models.ForwardInstanceStat{
		RuleID:        st.RuleID,
		NodeID:        st.NodeID,
		InstanceID:    st.InstanceID,
		Listen:        strings.TrimSpace(st.Listen),
		ListenPort:    st.ListenPort,
		StatsJSON:     strings.TrimSpace(string(st.Stats)),
		RouteJSON:     strings.TrimSpace(string(st.Route)),
		LastUpdatedAt: models.FromTime(st.LastUpdatedAt),
	}
	if time.Time(record.LastUpdatedAt).IsZero() {
		record.LastUpdatedAt = models.FromTime(time.Now().UTC())
	}
	if err := dbforward.UpsertForwardInstanceStat(record); err != nil {
		return err
	}

	agg, ok, err := aggregateNodeStatFromDB(st.RuleID, st.NodeID)
	if err != nil {
		return err
	}
	if ok {
		UpdateStatsAndBroadcast(agg)
	}
	return nil
}

func ListForwardInstanceStatsByRule(ruleID uint) ([]ForwardInstanceStats, error) {
	stats, err := dbforward.ListForwardInstanceStats(ruleID)
	if err != nil {
		return nil, err
	}
	out := make([]ForwardInstanceStats, 0, len(stats))
	for _, s := range stats {
		st := ForwardInstanceStats{
			RuleID:     s.RuleID,
			NodeID:     s.NodeID,
			InstanceID: s.InstanceID,
			Listen:     s.Listen,
			ListenPort: s.ListenPort,
		}
		if s.StatsJSON != "" {
			st.Stats = json.RawMessage(s.StatsJSON)
		}
		if s.RouteJSON != "" {
			st.Route = json.RawMessage(s.RouteJSON)
		}
		st.LastUpdatedAt = time.Time(s.LastUpdatedAt)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID == out[j].NodeID {
			return out[i].InstanceID < out[j].InstanceID
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}

func aggregateNodeStatFromDB(ruleID uint, nodeID string) (*models.ForwardStat, bool, error) {
	nodeID = strings.TrimSpace(nodeID)
	if ruleID == 0 || nodeID == "" {
		return nil, false, nil
	}
	instances, err := dbforward.ListForwardInstanceStatsByNode(ruleID, nodeID)
	if err != nil {
		return nil, false, err
	}
	if len(instances) == 0 {
		return nil, false, nil
	}

	now := time.Now().UTC()
	var totalIn int64
	var totalOut int64
	var currentConn int
	healthy := false

	for _, ins := range instances {
		if time.Time(ins.LastUpdatedAt).IsZero() {
			continue
		}
		if now.Sub(time.Time(ins.LastUpdatedAt)) <= 60*time.Second {
			healthy = true
		}
		if ins.StatsJSON == "" {
			continue
		}
		var sum realmInstanceStatsSummary
		if err := json.Unmarshal([]byte(ins.StatsJSON), &sum); err != nil {
			continue
		}
		totalIn += sum.TotalInboundBytes
		totalOut += sum.TotalOutboundBytes
		currentConn += sum.CurrentConnections
	}

	linkStatus := "faulty"
	if healthy {
		linkStatus = "healthy"
	}
	return &models.ForwardStat{
		RuleID:            ruleID,
		NodeID:            nodeID,
		LinkStatus:        linkStatus,
		ActiveConnections: currentConn,
		TrafficInBytes:    totalIn,
		TrafficOutBytes:   totalOut,
	}, true, nil
}
