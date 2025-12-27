package forward

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
)

type PlannedInstance struct {
	InstanceID   string          `json:"instance_id"`
	NodeID       string          `json:"node_id"`
	Listen       string          `json:"listen"`
	ListenPort   int             `json:"listen_port"`
	Remote       string          `json:"remote"`
	ExtraRemotes []string        `json:"extra_remotes,omitempty"`
	Balance      string          `json:"balance,omitempty"`
	Config       json.RawMessage `json:"config"`
}

type InstanceBinding struct {
	InstanceID string
	NodeID     string
	PortSpec   string
	Current    *int
}

func ListInstanceBindings(ruleType string, ruleID uint, cfg *RuleConfig) ([]InstanceBinding, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	entryNodeID := strings.TrimSpace(cfg.EntryNodeID)
	if entryNodeID == "" {
		return nil, fmt.Errorf("missing entry_node_id")
	}
	out := make([]InstanceBinding, 0, 8)

	out = append(out, InstanceBinding{
		InstanceID: InstanceIDEntry(ruleID, entryNodeID),
		NodeID:     entryNodeID,
		PortSpec:   strings.TrimSpace(cfg.EntryPort),
		Current:    &cfg.EntryCurrentPort,
	})

	switch strings.ToLower(strings.TrimSpace(ruleType)) {
	case "direct":
		// only entry
	case "relay_group":
		for stableIdx, ref := range sortedRelayRefs(cfg.Relays) {
			relay := ref.relay
			out = append(out, InstanceBinding{
				InstanceID: InstanceIDRelay(ruleID, relay.NodeID, stableIdx),
				NodeID:     relay.NodeID,
				PortSpec:   strings.TrimSpace(relay.Port),
				Current:    &relay.CurrentPort,
			})
		}
	case "chain":
		hopRefs := sortedHopRefs(cfg.Hops)
		for stableHopIndex, ref := range hopRefs {
			hop := ref.hop
			if strings.ToLower(strings.TrimSpace(hop.Type)) == "direct" {
				out = append(out, InstanceBinding{
					InstanceID: InstanceIDHop(ruleID, hop.NodeID, stableHopIndex),
					NodeID:     hop.NodeID,
					PortSpec:   strings.TrimSpace(hop.Port),
					Current:    &hop.CurrentPort,
				})
				continue
			}
			if strings.ToLower(strings.TrimSpace(hop.Type)) == "relay_group" {
				for stableRelayIndex, relayRef := range sortedRelayRefs(hop.Relays) {
					relay := relayRef.relay
					out = append(out, InstanceBinding{
						InstanceID: InstanceIDHopRelay(ruleID, relay.NodeID, stableHopIndex, stableRelayIndex),
						NodeID:     relay.NodeID,
						PortSpec:   strings.TrimSpace(relay.Port),
						Current:    &relay.CurrentPort,
					})
				}
				continue
			}
			return nil, fmt.Errorf("unsupported hop type: %s", hop.Type)
		}
	default:
		return nil, fmt.Errorf("unsupported rule type %q", ruleType)
	}

	for _, b := range out {
		if strings.TrimSpace(b.NodeID) == "" {
			return nil, fmt.Errorf("missing node_id for instance %s", b.InstanceID)
		}
		if strings.TrimSpace(b.PortSpec) == "" {
			return nil, fmt.Errorf("missing port_spec for instance %s", b.InstanceID)
		}
		if b.Current == nil {
			return nil, fmt.Errorf("missing current port binding for instance %s", b.InstanceID)
		}
	}

	return out, nil
}

func BuildPlannedInstances(ruleType string, ruleID uint, cfg RuleConfig, resolver NodeResolver) ([]PlannedInstance, error) {
	if resolver == nil {
		return nil, fmt.Errorf("node resolver is required")
	}
	entryNodeID := strings.TrimSpace(cfg.EntryNodeID)
	if entryNodeID == "" {
		return nil, fmt.Errorf("missing entry_node_id")
	}
	entryListenPort := ResolvePortFallback(cfg.EntryPort, cfg.EntryCurrentPort)
	if entryListenPort <= 0 {
		return nil, fmt.Errorf("entry listen port is not set")
	}

	targetHost, targetPort, err := resolveRuleTarget(cfg, resolver)
	if err != nil {
		return nil, err
	}
	targetAddr := net.JoinHostPort(targetHost, fmt.Sprintf("%d", targetPort))

	plan := make([]PlannedInstance, 0, 8)
	addInstance := func(instanceID string, nodeID string, listenPort int, remote string, extra []string, balance string, network *NetworkConfig) error {
		if listenPort <= 0 {
			return fmt.Errorf("listen port is not set for instance %s", instanceID)
		}
		listen := fmt.Sprintf("0.0.0.0:%d", listenPort)

		cfgMap := map[string]any{
			"listen":        listen,
			"remote":        remote,
			"extra_remotes": extra,
		}
		if strings.TrimSpace(balance) != "" {
			cfgMap["balance"] = strings.TrimSpace(balance)
		}

		networkObj, err := buildEndpointNetwork(cfg.Protocol, network)
		if err != nil {
			return err
		}
		if networkObj != nil {
			cfgMap["network"] = networkObj
		}

		raw, err := json.Marshal(cfgMap)
		if err != nil {
			return err
		}
		plan = append(plan, PlannedInstance{
			InstanceID:   instanceID,
			NodeID:       nodeID,
			Listen:       listen,
			ListenPort:   listenPort,
			Remote:       remote,
			ExtraRemotes: extra,
			Balance:      strings.TrimSpace(balance),
			Config:       raw,
		})
		return nil
	}

	switch strings.ToLower(strings.TrimSpace(ruleType)) {
	case "direct":
		if err := addInstance(
			InstanceIDEntry(ruleID, entryNodeID),
			entryNodeID,
			entryListenPort,
			targetAddr,
			nil,
			"",
			cfg.Network,
		); err != nil {
			return nil, err
		}
	case "relay_group":
		relays := sortedRelayRefs(cfg.Relays)
		if len(relays) == 0 {
			return nil, fmt.Errorf("relay_group requires relays")
		}
		relayAddrs, err := relayAddressesFromRefs(relays, resolver)
		if err != nil {
			return nil, err
		}
		balance := buildBalanceFrozen(cfg.Strategy, relays)

		if err := addInstance(
			InstanceIDEntry(ruleID, entryNodeID),
			entryNodeID,
			entryListenPort,
			relayAddrs[0],
			relayAddrs[1:],
			balance,
			cfg.Network,
		); err != nil {
			return nil, err
		}
		for stableRelayIndex, ref := range relays {
			relay := ref.relay
			listenPort := ResolvePortFallback(relay.Port, relay.CurrentPort)
			if listenPort <= 0 {
				return nil, fmt.Errorf("relay listen port is not set for node %s", relay.NodeID)
			}
			if err := addInstance(
				InstanceIDRelay(ruleID, relay.NodeID, stableRelayIndex),
				relay.NodeID,
				listenPort,
				targetAddr,
				nil,
				"",
				cfg.Network,
			); err != nil {
				return nil, err
			}
		}
	case "chain":
		hopRefs := sortedHopRefs(cfg.Hops)
		if len(hopRefs) == 0 {
			return nil, fmt.Errorf("chain requires hops")
		}
		firstHopTargets, firstHopBalance, err := hopTargetsForEntry(hopRefs[0].hop, resolver)
		if err != nil {
			return nil, err
		}
		if len(firstHopTargets) == 0 {
			return nil, fmt.Errorf("first hop has no targets")
		}
		if err := addInstance(
			InstanceIDEntry(ruleID, entryNodeID),
			entryNodeID,
			entryListenPort,
			firstHopTargets[0],
			firstHopTargets[1:],
			firstHopBalance,
			cfg.Network,
		); err != nil {
			return nil, err
		}

		for hopIndex := range hopRefs {
			hop := hopRefs[hopIndex].hop
			nextAddr := targetAddr
			if hopIndex+1 < len(hopRefs) {
				nextTargets, _, err := hopTargetsForEntry(hopRefs[hopIndex+1].hop, resolver)
				if err != nil {
					return nil, err
				}
				if len(nextTargets) == 0 {
					return nil, fmt.Errorf("next hop has no targets")
				}
				// Keep existing behavior: always use the first target as next hop.
				nextAddr = nextTargets[0]
			}

			switch strings.ToLower(strings.TrimSpace(hop.Type)) {
			case "direct":
				network := cfg.Network
				if hop.Network != nil {
					network = hop.Network
				}
				listenPort := ResolvePortFallback(hop.Port, hop.CurrentPort)
				if listenPort <= 0 {
					return nil, fmt.Errorf("hop listen port is not set for node %s", hop.NodeID)
				}
				if err := addInstance(
					InstanceIDHop(ruleID, hop.NodeID, hopIndex),
					hop.NodeID,
					listenPort,
					nextAddr,
					nil,
					"",
					network,
				); err != nil {
					return nil, err
				}
			case "relay_group":
				network := cfg.Network
				if hop.Network != nil {
					network = hop.Network
				}

				relays := sortedRelayRefs(hop.Relays)
				if len(relays) == 0 {
					return nil, fmt.Errorf("hop relay_group requires relays")
				}
				for relayIndex, ref := range relays {
					relay := ref.relay
					listenPort := ResolvePortFallback(relay.Port, relay.CurrentPort)
					if listenPort <= 0 {
						return nil, fmt.Errorf("hop relay listen port is not set for node %s", relay.NodeID)
					}
					if err := addInstance(
						InstanceIDHopRelay(ruleID, relay.NodeID, hopIndex, relayIndex),
						relay.NodeID,
						listenPort,
						nextAddr,
						nil,
						"",
						network,
					); err != nil {
						return nil, err
					}
				}
			default:
				return nil, fmt.Errorf("unsupported hop type: %s", hop.Type)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported rule type %q", ruleType)
	}

	sort.Slice(plan, func(i, j int) bool {
		if plan[i].NodeID == plan[j].NodeID {
			return plan[i].InstanceID < plan[j].InstanceID
		}
		return plan[i].NodeID < plan[j].NodeID
	})
	return plan, nil
}

func resolveRuleTarget(cfg RuleConfig, resolver NodeResolver) (string, int, error) {
	if strings.ToLower(strings.TrimSpace(cfg.TargetType)) == "node" {
		host, err := resolver(cfg.TargetNodeID)
		if err != nil {
			return "", 0, err
		}
		return host, cfg.TargetPort, nil
	}
	host := strings.TrimSpace(cfg.TargetHost)
	if host == "" {
		return "", 0, fmt.Errorf("missing target_host")
	}
	return host, cfg.TargetPort, nil
}

func buildEndpointNetwork(protocol string, network *NetworkConfig) (map[string]any, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	useUDP := protocol == "udp" || protocol == "both"
	noTCP := protocol == "udp"

	out := map[string]any{}

	if useUDP || noTCP {
		out["use_udp"] = useUDP
		out["no_tcp"] = noTCP
	}

	if network != nil {
		b, err := json.Marshal(network)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		for k, v := range m {
			// omit nil
			if v == nil {
				continue
			}
			out[strings.ToLower(k)] = v
		}
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

type relayRef struct {
	idx   int
	relay *RelayNode
}

func sortedRelayRefs(relays []RelayNode) []relayRef {
	refs := make([]relayRef, 0, len(relays))
	for i := range relays {
		refs = append(refs, relayRef{idx: i, relay: &relays[i]})
	}
	sort.Slice(refs, func(i, j int) bool {
		a := refs[i].relay
		b := refs[j].relay
		if a.SortOrder == b.SortOrder {
			if a.NodeID == b.NodeID {
				return refs[i].idx < refs[j].idx
			}
			return a.NodeID < b.NodeID
		}
		return a.SortOrder < b.SortOrder
	})
	return refs
}

type hopRef struct {
	idx int
	hop *ChainHop
}

func sortedHopRefs(hops []ChainHop) []hopRef {
	refs := make([]hopRef, 0, len(hops))
	for i := range hops {
		refs = append(refs, hopRef{idx: i, hop: &hops[i]})
	}
	sort.Slice(refs, func(i, j int) bool {
		a := refs[i].hop
		b := refs[j].hop
		if a.SortOrder == b.SortOrder {
			return refs[i].idx < refs[j].idx
		}
		return a.SortOrder < b.SortOrder
	})
	return refs
}

func relayAddressesFromRefs(relays []relayRef, resolver NodeResolver) ([]string, error) {
	addrs := make([]string, 0, len(relays))
	for _, r := range relays {
		host, err := resolver(r.relay.NodeID)
		if err != nil {
			return nil, err
		}
		port := ResolvePortFallback(r.relay.Port, r.relay.CurrentPort)
		if port <= 0 {
			return nil, fmt.Errorf("missing relay port for node %s", r.relay.NodeID)
		}
		addrs = append(addrs, net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	}
	return addrs, nil
}

func buildBalanceFrozen(strategy string, relays []relayRef) string {
	strategy = strings.ToLower(strings.TrimSpace(strategy))
	switch strategy {
	case "failover":
		return "failover"
	case "roundrobin":
		weights := make([]string, 0, len(relays))
		hasWeight := false
		for _, r := range relays {
			w := r.relay.SortOrder
			if w <= 0 {
				w = 1
			} else {
				hasWeight = true
			}
			weights = append(weights, fmt.Sprintf("%d", w))
		}
		if hasWeight {
			return fmt.Sprintf("roundrobin: %s", strings.Join(weights, ", "))
		}
		return "roundrobin"
	case "iphash":
		return "iphash"
	default:
		return ""
	}
}

func hopTargetsForEntry(hop *ChainHop, resolver NodeResolver) ([]string, string, error) {
	switch strings.ToLower(strings.TrimSpace(hop.Type)) {
	case "direct":
		host, err := resolver(hop.NodeID)
		if err != nil {
			return nil, "", err
		}
		port := ResolvePortFallback(hop.Port, hop.CurrentPort)
		if port <= 0 {
			return nil, "", fmt.Errorf("hop port missing for node %s", hop.NodeID)
		}
		return []string{net.JoinHostPort(host, fmt.Sprintf("%d", port))}, "", nil
	case "relay_group":
		relays := sortedRelayRefs(hop.Relays)
		if len(relays) == 0 {
			return nil, "", fmt.Errorf("hop relay_group requires relays")
		}
		addrs, err := relayAddressesFromRefs(relays, resolver)
		if err != nil {
			return nil, "", err
		}
		return addrs, buildBalanceFrozen(hop.Strategy, relays), nil
	default:
		return nil, "", fmt.Errorf("unsupported hop type: %s", hop.Type)
	}
}
