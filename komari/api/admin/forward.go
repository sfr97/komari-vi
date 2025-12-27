package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/api"
	dbforward "github.com/komari-monitor/komari/database/forward"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/forward"
)

type forwardRulePayload struct {
	IsEnabled   *bool  `json:"is_enabled"`
	Name        string `json:"name"`
	GroupName   string `json:"group_name"`
	SortOrder   *int   `json:"sort_order"`
	Tags        string `json:"tags"`
	Notes       string `json:"notes"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	ConfigJSON  string `json:"config_json"`
}

func ListForwards(c *gin.Context) {
	rules, err := dbforward.ListForwardRules()
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	api.RespondSuccess(c, rules)
}

func GetForward(c *gin.Context) {
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
	api.RespondSuccess(c, rule)
}

func CreateForward(c *gin.Context) {
	var payload forwardRulePayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	configJSON := payload.ConfigJSON
	if configJSON != "" {
		configJSON = sanitizeConfigJSON(configJSON)
		if err := validateConfigJSONStrategies(payload.Type, configJSON); err != nil {
			api.RespondError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	rule := models.ForwardRule{
		IsEnabled:   payload.IsEnabled != nil && *payload.IsEnabled,
		Name:        payload.Name,
		GroupName:   payload.GroupName,
		SortOrder:   valueOrDefaultInt(payload.SortOrder, 0),
		Tags:        payload.Tags,
		Notes:       payload.Notes,
		Type:        payload.Type,
		Status:      payload.Status,
		ConfigJSON:  configJSON,
	}
	if rule.Status == "" {
		rule.Status = "stopped"
	}
	if err := dbforward.CreateForwardRule(&rule); err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	api.RespondSuccess(c, rule)
}

func UpdateForward(c *gin.Context) {
	id, err := api.GetUintParam(c, "id")
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	var payload forwardRulePayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	var oldRule *models.ForwardRule
	oldRule, _ = dbforward.GetForwardRule(id)
	updates := map[string]interface{}{}
	if payload.IsEnabled != nil {
		updates["is_enabled"] = *payload.IsEnabled
	}
	if payload.Name != "" {
		updates["name"] = payload.Name
	}
	updates["group_name"] = payload.GroupName
	if payload.SortOrder != nil {
		updates["sort_order"] = *payload.SortOrder
	}
	updates["tags"] = payload.Tags
	updates["notes"] = payload.Notes
	if payload.Type != "" {
		updates["type"] = payload.Type
	}
	if payload.Status != "" {
		updates["status"] = payload.Status
	}
	if payload.ConfigJSON != "" {
		configJSON := sanitizeConfigJSON(payload.ConfigJSON)
		if err := validateConfigJSONStrategies(payload.Type, configJSON); err != nil {
			api.RespondError(c, http.StatusBadRequest, err.Error())
			return
		}
		updates["config_json"] = configJSON
	}
	if len(updates) > 0 && oldRule != nil && oldRule.Status == "running" && updates["config_json"] != nil {
		newRule := *oldRule
		if v, ok := updates["type"].(string); ok && v != "" {
			newRule.Type = v
		}
		if v, ok := updates["config_json"].(string); ok {
			newRule.ConfigJSON = v
		}
		if err := forward.ApplyHotUpdate(oldRule, &newRule); err != nil {
			api.RespondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		// ApplyHotUpdate may fill *_current_port for newly-added instances; persist the mutated config.
		updates["config_json"] = newRule.ConfigJSON
	}
	if err := dbforward.UpdateForwardRule(id, updates); err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	rule, _ := dbforward.GetForwardRule(id)
	api.RespondSuccess(c, rule)
}

func DeleteForward(c *gin.Context) {
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

	// P3: delete requires full cleanup to avoid leftovers.
	if rule != nil && strings.TrimSpace(rule.ConfigJSON) != "" {
		results, ok, stopErr := stopForwardRule(rule)
		if stopErr != nil {
			api.RespondError(c, http.StatusBadGateway, stopErr.Error())
			return
		}
		if !ok {
			api.RespondError(c, http.StatusBadGateway, "cleanup failed on some nodes, abort delete")
			return
		}
		_ = results
		_ = dbforward.UpdateForwardRule(id, map[string]interface{}{"status": "stopped"})
	}
	// P4: cleanup instance/node stats to avoid orphan rows.
	_ = dbforward.DeleteForwardInstanceStatsByRule(id)
	_ = dbforward.DeleteForwardStatsByRule(id)
	if err := dbforward.DeleteForwardRule(id); err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	api.RespondSuccess(c, gin.H{"deleted": true})
}

func EnableForward(c *gin.Context) {
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

	updates := map[string]interface{}{"is_enabled": true}
	if rule != nil && rule.Status != "running" {
		results, ok, startErr := startForwardRule(rule)
		if startErr != nil {
			api.RespondError(c, http.StatusInternalServerError, startErr.Error())
			return
		}
		_ = results // 保持接口返回兼容性（前端仅需成功/失败）
		if ok {
			updates["status"] = "running"
		} else {
			updates["status"] = "error"
		}
	}
	if err := dbforward.UpdateForwardRule(id, updates); err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _ := dbforward.GetForwardRule(id)
	api.RespondSuccess(c, updated)
}

func DisableForward(c *gin.Context) {
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
	updates := map[string]interface{}{"is_enabled": false}
	if rule != nil && rule.Status == "running" {
		results, ok, stopErr := stopForwardRule(rule)
		if stopErr != nil {
			api.RespondError(c, http.StatusInternalServerError, stopErr.Error())
			return
		}
		_ = results
		if ok {
			updates["status"] = "stopped"
		} else {
			updates["status"] = "error"
		}
	}
	if err := dbforward.UpdateForwardRule(id, updates); err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _ := dbforward.GetForwardRule(id)
	api.RespondSuccess(c, updated)
}

func GetForwardSystemSettings(c *gin.Context) {
	settings, err := dbforward.GetSystemSettings()
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	api.RespondSuccess(c, settings)
}

func UpdateForwardSystemSettings(c *gin.Context) {
	var payload models.ForwardSystemSettings
	if err := c.ShouldBindJSON(&payload); err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	payload.ID = 1
	if err := dbforward.UpdateSystemSettings(&payload); err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	api.RespondSuccess(c, payload)
}

func valueOrDefaultInt(ptr *int, def int) int {
	if ptr == nil {
		return def
	}
	return *ptr
}

func sanitizeConfigJSON(raw string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw
	}

	// Realm TOML has been deprecated: drop any realm_config fields from incoming config_json.
	delete(obj, "entry_realm_config")
	delete(obj, "realm_config")

	if relays, ok := obj["relays"].([]any); ok {
		for _, item := range relays {
			if m, ok := item.(map[string]any); ok {
				delete(m, "realm_config")
			}
		}
	}
	if hops, ok := obj["hops"].([]any); ok {
		for _, item := range hops {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			delete(m, "realm_config")
			if rs, ok := m["relays"].([]any); ok {
				for _, r := range rs {
					if rm, ok := r.(map[string]any); ok {
						delete(rm, "realm_config")
					}
				}
			}
		}
	}

	b, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return string(b)
}

func validateConfigJSONStrategies(ruleType string, configJSON string) error {
	var cfg forward.RuleConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return err
	}
	return forward.ValidateRuleConfigStrategies(ruleType, cfg)
}
