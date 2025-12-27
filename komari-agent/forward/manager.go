package forward

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/komari-monitor/komari-agent/ws"
)

type Manager struct {
	firewall   *FirewallManager
	supervisor *RealmApiSupervisor

	registry *ForwardInstanceRegistry
	reporter *InstanceStatsReporter
}

func NewManager() *Manager {
	supervisor := NewRealmApiSupervisor()
	registry := NewForwardInstanceRegistry()
	reporter := NewInstanceStatsReporter(supervisor, registry)
	reporter.Start()

	return &Manager{
		firewall:   NewFirewallManager(),
		supervisor: supervisor,
		registry:   registry,
		reporter:   reporter,
	}
}

func (m *Manager) RebindConn(conn *ws.SafeConn) {
	if m == nil || m.reporter == nil {
		return
	}
	m.reporter.RebindConn(conn)
}

func (m *Manager) HandleTask(conn *ws.SafeConn, env TaskEnvelope) (interface{}, error) {
	switch env.TaskType {
	case TaskCheckPort:
		var req CheckPortRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleCheckPort(req), nil
	case TaskRealmApiEnsure:
		var req RealmApiEnsureRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleRealmApiEnsure(req), nil
	case TaskRealmInstanceApply:
		var req RealmInstanceApplyRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleRealmInstanceApply(req), nil
	case TaskRealmInstanceStatsGet:
		var req RealmInstanceStatsGetRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleRealmInstanceStatsGet(req), nil
	case TaskRealmInstanceConnectionsGet:
		var req RealmInstanceConnectionsGetRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleRealmInstanceConnectionsGet(req), nil
	case TaskRealmInstanceRouteGet:
		var req RealmInstanceRouteGetRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleRealmInstanceRouteGet(req), nil
	case TaskTestConnectivity:
		var req TestConnectivityRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return handleTestConnectivity(req), nil
	default:
		return nil, fmt.Errorf("unknown forward task type: %s", env.TaskType)
	}
}

func (m *Manager) handleRealmApiEnsure(req RealmApiEnsureRequest) RealmApiEnsureResponse {
	st, err := m.supervisor.Ensure(req)
	if err != nil {
		return RealmApiEnsureResponse{Success: false, Message: err.Error()}
	}
	return RealmApiEnsureResponse{
		Success:      true,
		Pid:          st.Pid,
		Port:         st.Port,
		RealmVersion: st.Version,
		Message:      "realm api ready",
	}
}

func (m *Manager) handleCheckPort(req CheckPortRequest) CheckPortResponse {
	ports, err := parsePortSpec(req.PortSpec)
	if err != nil {
		return CheckPortResponse{Success: false, Message: err.Error()}
	}
	excludeSet := make(map[int]struct{}, len(req.ExcludedPorts))
	for _, p := range req.ExcludedPorts {
		excludeSet[p] = struct{}{}
	}

	realmReserved := map[int]struct{}{}
	if _, err := m.supervisor.Ensure(RealmApiEnsureRequest{}); err == nil {
		if baseURL, ok := m.supervisor.BaseURL(); ok {
			client := NewRealmApiClient(baseURL)
			if instances, err := client.ListInstances(); err == nil {
				for _, ins := range instances {
					if p, ok := listenPortFromAddr(ins.Config.Listen); ok && p > 0 {
						realmReserved[p] = struct{}{}
					}
				}
			}
		}
	}

	for _, p := range ports {
		if _, skip := excludeSet[p]; skip {
			continue
		}
		if _, used := realmReserved[p]; used {
			continue
		}
		if isPortAvailable(p) {
			return CheckPortResponse{
				Success:       true,
				AvailablePort: &p,
				Message:       fmt.Sprintf("Port %d is available", p),
			}
		}
	}
	return CheckPortResponse{Success: false, Message: fmt.Sprintf("no available port in spec: %s", req.PortSpec)}
}

func (m *Manager) handleRealmInstanceApply(req RealmInstanceApplyRequest) RealmInstanceApplyResponse {
	if req.RuleID == 0 || strings.TrimSpace(req.NodeID) == "" {
		return RealmInstanceApplyResponse{Success: false, Message: "rule_id and node_id are required"}
	}

	if _, err := m.supervisor.Ensure(RealmApiEnsureRequest{}); err != nil {
		return RealmInstanceApplyResponse{Success: false, Message: err.Error()}
	}
	baseURL, ok := m.supervisor.BaseURL()
	if !ok {
		return RealmInstanceApplyResponse{Success: false, Message: "realm api not ready"}
	}
	client := NewRealmApiClient(baseURL)

	allOK := true
	results := make([]RealmInstanceApplyOpResult, 0, len(req.Ops))
	for _, op := range req.Ops {
		opName := strings.ToLower(strings.TrimSpace(op.Op))
		instanceID := strings.TrimSpace(op.InstanceID)
		res := RealmInstanceApplyOpResult{Op: opName, InstanceID: instanceID, Success: false}

		if instanceID == "" {
			res.Message = "instance_id is required"
			allOK = false
			results = append(results, res)
			continue
		}

		switch opName {
		case "upsert":
			body, listen, listenPort, allowTCP, allowUDP, err := buildUpsertBody(instanceID, op.Config)
			if err != nil {
				res.Message = err.Error()
				allOK = false
				results = append(results, res)
				continue
			}
			if err := client.UpsertInstance(body); err != nil {
				res.Message = err.Error()
				allOK = false
				results = append(results, res)
				continue
			}

			m.registry.Upsert(ForwardInstanceMeta{
				RuleID:     req.RuleID,
				NodeID:     req.NodeID,
				InstanceID: instanceID,
				Listen:     listen,
				ListenPort: listenPort,
				AllowTCP:   allowTCP,
				AllowUDP:   allowUDP,
				UpdatedAt:  time.Now().UTC(),
			})

			res.Success = true
			res.Message = "upsert ok"
			results = append(results, res)
		case "start":
			if err := client.StartInstance(instanceID); err != nil {
				res.Message = err.Error()
				allOK = false
				results = append(results, res)
				continue
			}
			if err := m.ensureFirewallForInstance(client, instanceID, true); err != nil {
				res.Message = "started but open firewall failed: " + err.Error()
				allOK = false
				results = append(results, res)
				continue
			}
			res.Success = true
			res.Message = "start ok"
			results = append(results, res)
		case "stop":
			if err := client.StopInstance(instanceID); err != nil {
				res.Message = err.Error()
				allOK = false
				results = append(results, res)
				continue
			}
			_ = m.ensureFirewallForInstance(client, instanceID, false)
			res.Success = true
			res.Message = "stop ok"
			results = append(results, res)
		case "delete":
			_ = m.ensureFirewallForInstance(client, instanceID, false)
			if err := client.DeleteInstance(instanceID); err != nil {
				res.Message = err.Error()
				allOK = false
				results = append(results, res)
				continue
			}
			m.registry.Delete(instanceID)
			res.Success = true
			res.Message = "delete ok"
			results = append(results, res)
		default:
			res.Message = "unknown op: " + opName
			allOK = false
			results = append(results, res)
		}
	}

	msg := "ok"
	if !allOK {
		msg = "one or more ops failed"
	}
	return RealmInstanceApplyResponse{Success: allOK, Results: results, Message: msg}
}

func (m *Manager) handleRealmInstanceStatsGet(req RealmInstanceStatsGetRequest) RealmInstanceStatsGetResponse {
	if _, err := m.supervisor.Ensure(RealmApiEnsureRequest{}); err != nil {
		return RealmInstanceStatsGetResponse{Success: false, Message: err.Error()}
	}
	baseURL, ok := m.supervisor.BaseURL()
	if !ok {
		return RealmInstanceStatsGetResponse{Success: false, Message: "realm api not ready"}
	}
	client := NewRealmApiClient(baseURL)

	out := make(map[string]json.RawMessage, len(req.InstanceIDs))
	allOK := true
	for _, id := range req.InstanceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		raw, err := client.GetInstanceStatsRaw(id)
		if err != nil {
			allOK = false
			continue
		}
		out[id] = raw
	}
	msg := "ok"
	if !allOK {
		msg = "partial failure"
	}
	return RealmInstanceStatsGetResponse{Success: allOK, StatsByInstance: out, Message: msg}
}

func (m *Manager) handleRealmInstanceConnectionsGet(req RealmInstanceConnectionsGetRequest) RealmInstanceConnectionsGetResponse {
	if strings.TrimSpace(req.InstanceID) == "" {
		return RealmInstanceConnectionsGetResponse{Success: false, Message: "instance_id is required"}
	}
	if _, err := m.supervisor.Ensure(RealmApiEnsureRequest{}); err != nil {
		return RealmInstanceConnectionsGetResponse{Success: false, Message: err.Error()}
	}
	baseURL, ok := m.supervisor.BaseURL()
	if !ok {
		return RealmInstanceConnectionsGetResponse{Success: false, Message: "realm api not ready"}
	}
	client := NewRealmApiClient(baseURL)
	raw, err := client.GetInstanceConnectionsRaw(req.InstanceID, req.Protocol, req.Limit, req.Offset)
	if err != nil {
		return RealmInstanceConnectionsGetResponse{Success: false, Message: err.Error()}
	}
	return RealmInstanceConnectionsGetResponse{Success: true, Data: raw, Message: "ok"}
}

func (m *Manager) handleRealmInstanceRouteGet(req RealmInstanceRouteGetRequest) RealmInstanceRouteGetResponse {
	if strings.TrimSpace(req.InstanceID) == "" {
		return RealmInstanceRouteGetResponse{Success: false, Message: "instance_id is required"}
	}
	if _, err := m.supervisor.Ensure(RealmApiEnsureRequest{}); err != nil {
		return RealmInstanceRouteGetResponse{Success: false, Message: err.Error()}
	}
	baseURL, ok := m.supervisor.BaseURL()
	if !ok {
		return RealmInstanceRouteGetResponse{Success: false, Message: "realm api not ready"}
	}
	client := NewRealmApiClient(baseURL)
	raw, err := client.GetInstanceRouteRaw(req.InstanceID)
	if err != nil {
		return RealmInstanceRouteGetResponse{Success: false, Message: err.Error()}
	}
	return RealmInstanceRouteGetResponse{Success: true, Data: raw, Message: "ok"}
}

func buildUpsertBody(instanceID string, config json.RawMessage) (json.RawMessage, string, int, bool, bool, error) {
	if len(config) == 0 {
		return nil, "", 0, false, false, fmt.Errorf("config is required for upsert")
	}
	var m map[string]any
	if err := json.Unmarshal(config, &m); err != nil {
		return nil, "", 0, false, false, fmt.Errorf("invalid config json: %w", err)
	}
	if m == nil {
		return nil, "", 0, false, false, fmt.Errorf("config must be an object")
	}
	m["id"] = instanceID

	listen, _ := m["listen"].(string)
	listenPort, _ := listenPortFromAddr(listen)

	allowTCP, allowUDP := protocolFromNetwork(m["network"])
	// sanity: at least one transport must be enabled, otherwise realm will reject the config at start.
	if !allowTCP && !allowUDP {
		return nil, "", 0, false, false, fmt.Errorf("invalid config: both tcp and udp are disabled (network.no_tcp=true and network.use_udp=false)")
	}

	b, err := json.Marshal(m)
	if err != nil {
		return nil, "", 0, false, false, err
	}
	return b, listen, listenPort, allowTCP, allowUDP, nil
}

func (m *Manager) ensureFirewallForInstance(client *RealmApiClient, instanceID string, open bool) error {
	meta, ok := m.registry.Get(instanceID)
	listenPort := 0
	allowTCP := true
	allowUDP := false
	if ok {
		listenPort = meta.ListenPort
		allowTCP = meta.AllowTCP
		allowUDP = meta.AllowUDP
	}
	if listenPort <= 0 {
		ins, err := client.GetInstance(instanceID)
		if err != nil {
			return err
		}
		if p, ok := listenPortFromAddr(ins.Config.Listen); ok {
			listenPort = p
		}
		allowTCP = true
		allowUDP = false
		if len(ins.Config.Network) > 0 {
			var network any
			if err := json.Unmarshal(ins.Config.Network, &network); err == nil {
				allowTCP, allowUDP = protocolFromNetwork(network)
			}
		}
	}
	if listenPort <= 0 {
		return fmt.Errorf("invalid listen port for instance %s", instanceID)
	}

	if open {
		if allowTCP {
			if err := m.firewall.OpenPort(listenPort, "tcp"); err != nil {
				return err
			}
		}
		if allowUDP {
			if err := m.firewall.OpenPort(listenPort, "udp"); err != nil {
				return err
			}
		}
		return nil
	}
	if allowTCP {
		_ = m.firewall.ClosePort(listenPort, "tcp")
	}
	if allowUDP {
		_ = m.firewall.ClosePort(listenPort, "udp")
	}
	return nil
}

func handleTestConnectivity(req TestConnectivityRequest) TestConnectivityResponse {
	address := net.JoinHostPort(req.TargetHost, strconv.Itoa(req.TargetPort))
	timeout := time.Duration(req.Timeout) * time.Second
	start := time.Now()
	c, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return TestConnectivityResponse{
			Success:   false,
			Reachable: false,
			Message:   err.Error(),
		}
	}
	_ = c.Close()
	latency := time.Since(start).Milliseconds()
	return TestConnectivityResponse{
		Success:   true,
		Reachable: true,
		LatencyMs: &latency,
		Message:   "Target is reachable",
	}
}

func protocolFromNetwork(network any) (allowTCP bool, allowUDP bool) {
	// Realm defaults: tcp enabled, udp disabled.
	allowTCP = true
	allowUDP = false

	m, ok := network.(map[string]any)
	if !ok || m == nil {
		return allowTCP, allowUDP
	}

	noTCP, ok := m["no_tcp"].(bool)
	if ok && noTCP {
		allowTCP = false
	}
	useUDP, ok := m["use_udp"].(bool)
	if ok && useUDP {
		allowUDP = true
	}
	return allowTCP, allowUDP
}
