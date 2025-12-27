package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/api"
	dbforward "github.com/komari-monitor/komari/database/forward"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/forward"
)

type checkPortReq struct {
	RuleID      uint   `json:"rule_id"`
	NodeID      string `json:"node_id" binding:"required"`
	PortSpec    string `json:"port_spec" binding:"required"`
	Excluded    []int  `json:"excluded_ports"`
	TimeoutSecs int    `json:"timeout"`
}

// CheckPort 调用 Agent 进行端口检测
func CheckPort(c *gin.Context) {
	var req checkPortReq
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	timeout := time.Duration(req.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	excluded := forward.MergeExcludedPorts(req.Excluded, forward.CollectReservedPortsForNode(req.NodeID, req.RuleID))
	resp, err := forward.SendTaskToNode(req.NodeID, forward.TaskCheckPort, forward.CheckPortRequest{
		PortSpec:      req.PortSpec,
		ExcludedPorts: excluded,
	}, timeout)
	if err != nil && resp.Message == "" {
		resp.Message = err.Error()
	}
	api.RespondSuccess(c, resp)
}

func decodeRealmEnsureOK(res forward.AgentTaskResult) bool {
	if !res.Success {
		return false
	}
	var payload forward.RealmApiEnsureResponse
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		return false
	}
	return payload.Success
}

func decodeApplyOK(res forward.AgentTaskResult) bool {
	if !res.Success {
		return false
	}
	var payload forward.RealmInstanceApplyResponse
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		return false
	}
	return payload.Success
}

func persistRuleConfigJSON(ruleID uint, cfg forward.RuleConfig) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	newJSON := string(b)
	if err := dbforward.UpdateForwardRule(ruleID, map[string]interface{}{"config_json": newJSON}); err != nil {
		return "", err
	}
	return newJSON, nil
}

func ensureRulePortsAndPersist(rule *models.ForwardRule, verifyCurrent bool) (forward.RuleConfig, bool, error) {
	var cfg forward.RuleConfig
	if rule == nil {
		return cfg, false, fmt.Errorf("rule is nil")
	}
	if rule.ConfigJSON == "" {
		return cfg, false, fmt.Errorf("missing config_json")
	}
	if err := json.Unmarshal([]byte(rule.ConfigJSON), &cfg); err != nil {
		return cfg, false, err
	}

	changed, err := forward.EnsureRuleCurrentPorts(rule.Type, rule.ID, &cfg, forward.EnsurePortsOptions{
		VerifyCurrentAvailability: verifyCurrent,
		Timeout:                   10 * time.Second,
	})
	if err != nil {
		return cfg, false, err
	}
	if !changed {
		return cfg, false, nil
	}
	newJSON, err := persistRuleConfigJSON(rule.ID, cfg)
	if err != nil {
		return cfg, false, err
	}
	rule.ConfigJSON = newJSON
	return cfg, true, nil
}

func buildInstancePlan(rule *models.ForwardRule, cfg forward.RuleConfig) ([]forward.PlannedInstance, error) {
	if rule == nil {
		return nil, fmt.Errorf("rule is nil")
	}
	return forward.BuildPlannedInstances(rule.Type, rule.ID, cfg, resolveNodeIP)
}

func groupPlanByNode(plan []forward.PlannedInstance) map[string][]forward.PlannedInstance {
	out := map[string][]forward.PlannedInstance{}
	for _, ins := range plan {
		out[ins.NodeID] = append(out[ins.NodeID], ins)
	}
	return out
}

func buildUpsertStartOps(instances []forward.PlannedInstance) []forward.RealmInstanceApplyOp {
	ops := make([]forward.RealmInstanceApplyOp, 0, len(instances)*2)
	for _, ins := range instances {
		ops = append(ops, forward.RealmInstanceApplyOp{
			Op:         "upsert",
			InstanceID: ins.InstanceID,
			Config:     ins.Config,
		})
		ops = append(ops, forward.RealmInstanceApplyOp{
			Op:         "start",
			InstanceID: ins.InstanceID,
		})
	}
	return ops
}

func buildStopDeleteOps(instanceIDs []string) []forward.RealmInstanceApplyOp {
	ops := make([]forward.RealmInstanceApplyOp, 0, len(instanceIDs)*2)
	for _, id := range instanceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ops = append(ops, forward.RealmInstanceApplyOp{Op: "stop", InstanceID: id})
		ops = append(ops, forward.RealmInstanceApplyOp{Op: "delete", InstanceID: id})
	}
	return ops
}

func listRuleInstanceIDsByNode(ruleType string, ruleID uint, cfg *forward.RuleConfig) (map[string][]string, error) {
	bindings, err := forward.ListInstanceBindings(ruleType, ruleID, cfg)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, b := range bindings {
		out[b.NodeID] = append(out[b.NodeID], b.InstanceID)
	}
	return out, nil
}

func isLikelyBindConflict(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "already in use") ||
		strings.Contains(msg, "bind") ||
		strings.Contains(msg, "listen")
}

func extractBindConflictInstanceIDs(applyPayload forward.RealmInstanceApplyResponse) []string {
	if applyPayload.Success {
		return nil
	}
	ids := make([]string, 0, 4)
	for _, r := range applyPayload.Results {
		if r.Success {
			continue
		}
		op := strings.ToLower(strings.TrimSpace(r.Op))
		if op != "upsert" && op != "start" {
			continue
		}
		if !isLikelyBindConflict(r.Message) {
			continue
		}
		if r.InstanceID != "" {
			ids = append(ids, r.InstanceID)
		}
	}
	return ids
}

func reselectPortsForInstances(rule *models.ForwardRule, cfg *forward.RuleConfig, nodeID string, instanceIDs []string) (bool, error) {
	if rule == nil || cfg == nil {
		return false, fmt.Errorf("rule/cfg is nil")
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return false, fmt.Errorf("node_id is empty")
	}
	if len(instanceIDs) == 0 {
		return false, nil
	}

	bindings, err := forward.ListInstanceBindings(rule.Type, rule.ID, cfg)
	if err != nil {
		return false, err
	}
	byID := map[string]*forward.InstanceBinding{}
	for i := range bindings {
		b := bindings[i]
		if b.NodeID != nodeID || b.Current == nil {
			continue
		}
		copy := b
		byID[b.InstanceID] = &copy
	}

	reserved := forward.CollectReservedPortsForNode(nodeID, rule.ID)
	used := map[int]struct{}{}
	for _, b := range bindings {
		if b.NodeID != nodeID || b.Current == nil {
			continue
		}
		if p := *b.Current; p > 0 {
			used[p] = struct{}{}
		}
	}

	changed := false
	for _, instanceID := range instanceIDs {
		b := byID[instanceID]
		if b == nil || b.Current == nil {
			continue
		}
		current := *b.Current
		excluded := forward.MergeExcludedPorts(reserved, portsFromSet(used))
		if current > 0 {
			excluded = forward.MergeExcludedPorts(excluded, []int{current})
		}
		resp, err := forward.SendTaskToNode(nodeID, forward.TaskCheckPort, forward.CheckPortRequest{
			PortSpec:      b.PortSpec,
			ExcludedPorts: excluded,
		}, 10*time.Second)
		if err != nil {
			return changed, err
		}
		var payload forward.CheckPortResponse
		if err := json.Unmarshal(resp.Payload, &payload); err != nil {
			return changed, fmt.Errorf("decode CHECK_PORT response failed: %w", err)
		}
		if !payload.Success || payload.AvailablePort == nil || *payload.AvailablePort <= 0 {
			return changed, fmt.Errorf("reselect port failed for %s: %s", instanceID, payload.Message)
		}
		*b.Current = *payload.AvailablePort
		used[*payload.AvailablePort] = struct{}{}
		changed = true
	}
	return changed, nil
}

func portsFromSet(set map[int]struct{}) []int {
	if len(set) == 0 {
		return nil
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}

func startForwardRule(rule *models.ForwardRule) ([]forward.AgentTaskResult, bool, error) {
	if rule == nil {
		return nil, false, fmt.Errorf("rule is nil")
	}
	var cfg forward.RuleConfig
	_ = json.Unmarshal([]byte(rule.ConfigJSON), &cfg)
	if err := forward.ValidateRuleConfigStrategies(rule.Type, cfg); err != nil {
		return nil, false, err
	}

	// 1) 启动前确保 current_port 已确定并回写（且校验端口可用）
	cfg, _, err := ensureRulePortsAndPersist(rule, true)
	if err != nil {
		return nil, false, err
	}

	order := forward.BuildApplyNodeOrder(rule.Type, cfg)

	results := make([]forward.AgentTaskResult, 0, len(order)*2)
	allOK := true
	for _, nodeID := range order {
		ensureRes, ensureErr := forward.SendTaskToNode(nodeID, forward.TaskRealmApiEnsure, forward.RealmApiEnsureRequest{
			RealmDownloadURL: "",
			ForceReinstall:   false,
		}, 60*time.Second)
		if ensureErr != nil && ensureRes.Message == "" {
			ensureRes.Message = ensureErr.Error()
		}
		results = append(results, ensureRes)
		if !decodeRealmEnsureOK(ensureRes) {
			allOK = false
			continue
		}

		// 2) 生成实例计划并下发（若 bind 冲突，则自动换端口并回写后重试）
		applied := false
		for attempt := 0; attempt < 3; attempt++ {
			plan, err := buildInstancePlan(rule, cfg)
			if err != nil {
				return results, false, err
			}
			byNode := groupPlanByNode(plan)
			instances := byNode[nodeID]
			if len(instances) == 0 {
				applied = true
				break
			}

			applyRes, applyErr := forward.SendTaskToNode(nodeID, forward.TaskRealmInstanceApply, forward.RealmInstanceApplyRequest{
				RuleID: rule.ID,
				NodeID: nodeID,
				Ops:    buildUpsertStartOps(instances),
			}, 30*time.Second)
			if applyErr != nil && applyRes.Message == "" {
				applyRes.Message = applyErr.Error()
			}
			results = append(results, applyRes)
			if decodeApplyOK(applyRes) {
				applied = true
				break
			}

			var payload forward.RealmInstanceApplyResponse
			if err := json.Unmarshal(applyRes.Payload, &payload); err != nil {
				break
			}
			conflicts := extractBindConflictInstanceIDs(payload)
			if len(conflicts) == 0 {
				break
			}
			if changed, err := reselectPortsForInstances(rule, &cfg, nodeID, conflicts); err != nil {
				break
			} else if changed {
				newJSON, err := persistRuleConfigJSON(rule.ID, cfg)
				if err != nil {
					break
				}
				rule.ConfigJSON = newJSON
			}
		}
		if !applied {
			allOK = false
		}
	}
	return results, allOK, nil
}

func stopForwardRule(rule *models.ForwardRule) ([]forward.AgentTaskResult, bool, error) {
	if rule == nil {
		return nil, false, fmt.Errorf("rule is nil")
	}
	if rule.ConfigJSON == "" {
		return nil, false, fmt.Errorf("missing config_json")
	}
	var cfg forward.RuleConfig
	if err := json.Unmarshal([]byte(rule.ConfigJSON), &cfg); err != nil {
		return nil, false, err
	}

	idsByNode, err := listRuleInstanceIDsByNode(rule.Type, rule.ID, &cfg)
	if err != nil {
		return nil, false, err
	}

	order := forward.BuildApplyNodeOrder(rule.Type, cfg)
	results := make([]forward.AgentTaskResult, 0, len(order)*2)
	allOK := true

	for _, nodeID := range order {
		instanceIDs := idsByNode[nodeID]
		if len(instanceIDs) == 0 {
			continue
		}

		ensureRes, ensureErr := forward.SendTaskToNode(nodeID, forward.TaskRealmApiEnsure, forward.RealmApiEnsureRequest{
			RealmDownloadURL: "",
			ForceReinstall:   false,
		}, 60*time.Second)
		if ensureErr != nil && ensureRes.Message == "" {
			ensureRes.Message = ensureErr.Error()
		}
		results = append(results, ensureRes)
		if !decodeRealmEnsureOK(ensureRes) {
			allOK = false
			continue
		}

		applyRes, applyErr := forward.SendTaskToNode(nodeID, forward.TaskRealmInstanceApply, forward.RealmInstanceApplyRequest{
			RuleID: rule.ID,
			NodeID: nodeID,
			Ops:    buildStopDeleteOps(instanceIDs),
		}, 30*time.Second)
		if applyErr != nil && applyRes.Message == "" {
			applyRes.Message = applyErr.Error()
		}
		results = append(results, applyRes)
		if !decodeApplyOK(applyRes) {
			allOK = false
		}
	}
	return results, allOK, nil
}

// StartForward 启动规则（入口+相关节点），当前实现直连/中继组/链式
func StartForward(c *gin.Context) {
	id, err := api.GetUintParam(c, "id")
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	rule, err := dbforward.GetForwardRule(id)
	if err != nil {
		api.RespondError(c, http.StatusNotFound, err.Error())
		return
	}
	results, ok, err := startForwardRule(rule)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	status := "running"
	if !ok {
		status = "error"
	}
	_ = dbforward.UpdateForwardRule(rule.ID, map[string]interface{}{"status": status})
	api.RespondSuccess(c, results)
}

// StopForward 停止规则相关节点（stop + delete）
func StopForward(c *gin.Context) {
	id, err := api.GetUintParam(c, "id")
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	rule, err := dbforward.GetForwardRule(id)
	if err != nil {
		api.RespondError(c, http.StatusNotFound, err.Error())
		return
	}
	results, ok, err := stopForwardRule(rule)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	status := "stopped"
	if !ok {
		status = "error"
	}
	_ = dbforward.UpdateForwardRule(rule.ID, map[string]interface{}{"status": status})
	api.RespondSuccess(c, results)
}

// ApplyForwardConfigs 批量下发配置（仅用于运行中规则）
func ApplyForwardConfigs(c *gin.Context) {
	id, err := api.GetUintParam(c, "id")
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	rule, err := dbforward.GetForwardRule(id)
	if err != nil {
		api.RespondError(c, http.StatusNotFound, err.Error())
		return
	}
	if strings.ToLower(rule.Status) != "running" {
		api.RespondError(c, http.StatusBadRequest, "rule not running")
		return
	}

	var cfg forward.RuleConfig
	_ = json.Unmarshal([]byte(rule.ConfigJSON), &cfg)
	if err := forward.ValidateRuleConfigStrategies(rule.Type, cfg); err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}

	// running rule: only fill missing current ports (do not treat existing ports as conflicts)
	cfg, _, err = ensureRulePortsAndPersist(rule, false)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	plan, err := buildInstancePlan(rule, cfg)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	byNode := groupPlanByNode(plan)
	order := forward.BuildApplyNodeOrder(rule.Type, cfg)

	results := make([]forward.AgentTaskResult, 0, len(order)*2)
	for _, nodeID := range order {
		instances := byNode[nodeID]
		if len(instances) == 0 {
			continue
		}

		ensureRes, ensureErr := forward.SendTaskToNode(nodeID, forward.TaskRealmApiEnsure, forward.RealmApiEnsureRequest{
			RealmDownloadURL: "",
			ForceReinstall:   false,
		}, 60*time.Second)
		if ensureErr != nil && ensureRes.Message == "" {
			ensureRes.Message = ensureErr.Error()
		}
		results = append(results, ensureRes)
		if !decodeRealmEnsureOK(ensureRes) {
			continue
		}

		applyRes, applyErr := forward.SendTaskToNode(nodeID, forward.TaskRealmInstanceApply, forward.RealmInstanceApplyRequest{
			RuleID: rule.ID,
			NodeID: nodeID,
			Ops:    buildUpsertStartOps(instances),
		}, 30*time.Second)
		if applyErr != nil && applyRes.Message == "" {
			applyRes.Message = applyErr.Error()
		}
		results = append(results, applyRes)
	}

	api.RespondSuccess(c, results)
}
