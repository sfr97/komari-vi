package forward

import (
	"encoding/json"
	"log"
	"time"

	"github.com/komari-monitor/komari-agent/ws"
)

type InstanceStatsReporter struct {
	supervisor *RealmApiSupervisor
	registry   *ForwardInstanceRegistry

	interval time.Duration

	stopCh chan struct{}
	conn   *ws.SafeConn
}

func NewInstanceStatsReporter(supervisor *RealmApiSupervisor, registry *ForwardInstanceRegistry) *InstanceStatsReporter {
	return &InstanceStatsReporter{
		supervisor: supervisor,
		registry:   registry,
		interval:   10 * time.Second,
		stopCh:     make(chan struct{}),
	}
}

func (r *InstanceStatsReporter) RebindConn(conn *ws.SafeConn) {
	r.conn = conn
}

func (r *InstanceStatsReporter) Start() {
	go r.loop()
}

func (r *InstanceStatsReporter) Stop() {
	select {
	case <-r.stopCh:
	default:
		close(r.stopCh)
	}
}

func (r *InstanceStatsReporter) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			conn := r.conn
			if conn == nil {
				continue
			}

			if _, err := r.supervisor.Ensure(RealmApiEnsureRequest{}); err != nil {
				continue
			}
			baseURL, ok := r.supervisor.BaseURL()
			if !ok {
				continue
			}
			client := NewRealmApiClient(baseURL)

			metas := r.registry.Snapshot()
			for _, meta := range metas {
				statsRaw, err := client.GetInstanceStatsRaw(meta.InstanceID)
				if err != nil {
					continue
				}
				routeRaw, err := client.GetInstanceRouteRaw(meta.InstanceID)
				if err != nil {
					routeRaw = nil
				}

				payload := map[string]any{
					"type":        "forward_instance_stats",
					"rule_id":     meta.RuleID,
					"node_id":     meta.NodeID,
					"instance_id": meta.InstanceID,
					"listen":      meta.Listen,
					"listen_port": meta.ListenPort,
					"stats":       json.RawMessage(statsRaw),
				}
				if len(routeRaw) > 0 {
					payload["route"] = json.RawMessage(routeRaw)
				}
				payload["last_updated_at"] = time.Now().UTC()

				if err := conn.WriteJSON(payload); err != nil {
					log.Printf("send forward_instance_stats failed: %v", err)
					break
				}
			}
		}
	}
}
