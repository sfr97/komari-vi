package forward

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	dbforward "github.com/komari-monitor/komari/database/forward"
)

// ResyncNodeOnReconnect is triggered by agent after (re)connecting to server.
// It will ensure that rules which should be running on this node are applied via REALM_INSTANCE_APPLY,
// and best-effort cleanup leftovers for non-running rules.
func ResyncNodeOnReconnect(nodeID string) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return
	}

	rules, err := dbforward.ListForwardRules()
	if err != nil {
		log.Printf("forward resync: list rules failed: %v", err)
		return
	}

	for _, r := range rules {
		rule := r // copy
		if strings.TrimSpace(rule.ConfigJSON) == "" {
			continue
		}
		var cfg RuleConfig
		if err := json.Unmarshal([]byte(rule.ConfigJSON), &cfg); err != nil {
			continue
		}

		bindings, err := ListInstanceBindings(rule.Type, rule.ID, &cfg)
		if err != nil {
			continue
		}
		instanceIDs := make([]string, 0, 4)
		for _, b := range bindings {
			if strings.TrimSpace(b.NodeID) == nodeID {
				instanceIDs = append(instanceIDs, b.InstanceID)
			}
		}
		if len(instanceIDs) == 0 {
			continue
		}

		shouldRun := rule.IsEnabled && strings.ToLower(strings.TrimSpace(rule.Status)) == "running"
		if !shouldRun {
			if _, err := SendTaskToNode(nodeID, TaskRealmApiEnsure, RealmApiEnsureRequest{}, 60*time.Second); err != nil {
				continue
			}
			_, _ = SendTaskToNode(nodeID, TaskRealmInstanceApply, RealmInstanceApplyRequest{
				RuleID: rule.ID,
				NodeID: nodeID,
				Ops:    buildStopDeleteOps(instanceIDs),
			}, 30*time.Second)
			continue
		}

		// Ensure missing current ports for newly-added instances (best-effort, and persist to DB).
		changed, err := EnsureRuleCurrentPorts(rule.Type, rule.ID, &cfg, EnsurePortsOptions{
			VerifyCurrentAvailability: false,
			Timeout:                   10 * time.Second,
		})
		if err == nil && changed {
			if b, err := json.Marshal(cfg); err == nil {
				_ = dbforward.UpdateForwardRule(rule.ID, map[string]interface{}{"config_json": string(b)})
			}
		}

		plan, err := BuildPlannedInstances(rule.Type, rule.ID, cfg, resolveNodeIP)
		if err != nil {
			continue
		}
		instances := make([]PlannedInstance, 0, 4)
		for _, ins := range plan {
			if strings.TrimSpace(ins.NodeID) == nodeID {
				instances = append(instances, ins)
			}
		}
		if len(instances) == 0 {
			continue
		}

		if _, err := SendTaskToNode(nodeID, TaskRealmApiEnsure, RealmApiEnsureRequest{}, 60*time.Second); err != nil {
			continue
		}
		_, _ = SendTaskToNode(nodeID, TaskRealmInstanceApply, RealmInstanceApplyRequest{
			RuleID: rule.ID,
			NodeID: nodeID,
			Ops:    buildUpsertStartOps(instances),
		}, 30*time.Second)
	}
}

func buildUpsertStartOps(instances []PlannedInstance) []RealmInstanceApplyOp {
	ops := make([]RealmInstanceApplyOp, 0, len(instances)*2)
	for _, ins := range instances {
		ops = append(ops, RealmInstanceApplyOp{
			Op:         "upsert",
			InstanceID: ins.InstanceID,
			Config:     ins.Config,
		})
		ops = append(ops, RealmInstanceApplyOp{
			Op:         "start",
			InstanceID: ins.InstanceID,
		})
	}
	return ops
}

func buildStopDeleteOps(instanceIDs []string) []RealmInstanceApplyOp {
	ops := make([]RealmInstanceApplyOp, 0, len(instanceIDs)*2)
	for _, id := range instanceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ops = append(ops, RealmInstanceApplyOp{Op: "stop", InstanceID: id})
		ops = append(ops, RealmInstanceApplyOp{Op: "delete", InstanceID: id})
	}
	return ops
}
