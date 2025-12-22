package forward

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	dbforward "github.com/komari-monitor/komari/database/forward"
)

// ResyncNodeOnReconnect is triggered by agent after (re)connecting to server.
// It will:
// 1) Ensure PREPARE_FORWARD_ENV for rules that should be running on this node.
// 2) Start realms for running rules.
// 3) Stop realms for non-running rules (best-effort cleanup of leftovers).
func ResyncNodeOnReconnect(nodeID string) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return
	}

	template, err := dbforward.GetRealmConfigTemplate()
	if err != nil {
		log.Printf("forward resync: get template failed: %v", err)
		return
	}
	settings, err := dbforward.GetSystemSettings()
	if err != nil {
		log.Printf("forward resync: get system settings failed: %v", err)
		return
	}
	rules, err := dbforward.ListForwardRules()
	if err != nil {
		log.Printf("forward resync: list rules failed: %v", err)
		return
	}

	for _, r := range rules {
		rule := r // copy
		if rule.ConfigJSON == "" {
			continue
		}
		var rc RuleConfig
		if err := json.Unmarshal([]byte(rule.ConfigJSON), &rc); err != nil {
			continue
		}

		// quickly skip rules not related to this node
		ports := collectNodePorts(&rule)
		port, ok := ports[nodeID]
		if !ok || port <= 0 {
			continue
		}
		protocol := strings.TrimSpace(rc.Protocol)
		if protocol == "" {
			protocol = "tcp"
		}

		if !rule.IsEnabled || strings.ToLower(strings.TrimSpace(rule.Status)) != "running" {
			// Best-effort stop cleanup for leftovers (agent restart may leave old realms running).
			_, _ = SendTaskToNode(nodeID, TaskStopRealm, StopRealmRequest{
				RuleID:   rule.ID,
				NodeID:   nodeID,
				Protocol: protocol,
				Port:     port,
				Timeout:  settings.ProcessStopTimeout,
			}, 15*time.Second)
			continue
		}

		// Running: prepare env first (agent will auto-build realm download url when empty).
		if _, err := SendTaskToNode(nodeID, TaskPrepareForwardEnv, PrepareForwardEnvRequest{
			RealmDownloadURL: "",
			ForceReinstall:   false,
		}, 60*time.Second); err != nil {
			log.Printf("forward resync: PREPARE failed (rule=%d node=%s): %v", rule.ID, nodeID, err)
			continue
		}

		cfgs, err := GenerateRealmConfigs(rule, template.TemplateToml, resolveNodeIP)
		if err != nil {
			log.Printf("forward resync: generate config failed (rule=%d): %v", rule.ID, err)
			continue
		}
		reqs := buildStartRequests(&rule, cfgs, settings.StatsReportInterval, settings.HealthCheckInterval, settings.RealmCrashRestartLimit, settings.ProcessStopTimeout, template.TemplateToml)
		req, ok := reqs[nodeID]
		if !ok || req.Port <= 0 || strings.TrimSpace(req.Config) == "" {
			continue
		}
		if _, err := SendTaskToNode(nodeID, TaskStartRealm, req, 20*time.Second); err != nil {
			log.Printf("forward resync: START failed (rule=%d node=%s): %v", rule.ID, nodeID, err)
			continue
		}
	}
}
