package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/api"
	dbforward "github.com/komari-monitor/komari/database/forward"
	"github.com/komari-monitor/komari/forward"
)

// ListForwardInstances returns the current Instance Plan for a rule.
func ListForwardInstances(c *gin.Context) {
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
	var cfg forward.RuleConfig
	if err := json.Unmarshal([]byte(rule.ConfigJSON), &cfg); err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	plan, err := forward.BuildPlannedInstances(rule.Type, rule.ID, cfg, resolveNodeIP)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	statsList, err := forward.ListForwardInstanceStatsByRule(rule.ID)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	statsByInstance := make(map[string]forward.ForwardInstanceStats, len(statsList))
	for _, st := range statsList {
		statsByInstance[st.InstanceID] = st
	}
	api.RespondSuccess(c, gin.H{
		"rule":              rule,
		"instances":         plan,
		"stats_by_instance": statsByInstance,
	})
}

func GetForwardInstanceConnections(c *gin.Context) {
	instanceID := strings.TrimSpace(c.Param("instance_id"))
	if instanceID == "" {
		api.RespondError(c, http.StatusBadRequest, "missing instance_id")
		return
	}
	nodeID, ok := forward.ParseNodeIDFromInstanceID(instanceID)
	if !ok {
		api.RespondError(c, http.StatusBadRequest, "invalid instance_id")
		return
	}

	protocol := strings.TrimSpace(c.Query("protocol"))
	limit := 100
	offset := 0
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := strings.TrimSpace(c.Query("offset")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	res, err := forward.SendTaskToNode(nodeID, forward.TaskRealmInstanceConnectionsGet, forward.RealmInstanceConnectionsGetRequest{
		InstanceID: instanceID,
		Protocol:   protocol,
		Limit:      limit,
		Offset:     offset,
	}, 20*time.Second)
	if err != nil && res.Message == "" {
		res.Message = err.Error()
	}
	if !res.Success {
		api.RespondError(c, http.StatusBadGateway, res.Message)
		return
	}
	var payload forward.RealmInstanceConnectionsGetResponse
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		api.RespondError(c, http.StatusBadGateway, err.Error())
		return
	}
	if !payload.Success {
		api.RespondError(c, http.StatusBadGateway, payload.Message)
		return
	}
	api.RespondSuccess(c, json.RawMessage(payload.Data))
}

func GetForwardInstanceRoute(c *gin.Context) {
	instanceID := strings.TrimSpace(c.Param("instance_id"))
	if instanceID == "" {
		api.RespondError(c, http.StatusBadRequest, "missing instance_id")
		return
	}
	nodeID, ok := forward.ParseNodeIDFromInstanceID(instanceID)
	if !ok {
		api.RespondError(c, http.StatusBadRequest, "invalid instance_id")
		return
	}

	res, err := forward.SendTaskToNode(nodeID, forward.TaskRealmInstanceRouteGet, forward.RealmInstanceRouteGetRequest{
		InstanceID: instanceID,
	}, 20*time.Second)
	if err != nil && res.Message == "" {
		res.Message = err.Error()
	}
	if !res.Success {
		api.RespondError(c, http.StatusBadGateway, res.Message)
		return
	}
	var payload forward.RealmInstanceRouteGetResponse
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		api.RespondError(c, http.StatusBadGateway, err.Error())
		return
	}
	if !payload.Success {
		api.RespondError(c, http.StatusBadGateway, payload.Message)
		return
	}
	api.RespondSuccess(c, json.RawMessage(payload.Data))
}
