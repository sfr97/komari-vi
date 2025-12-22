package forward

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	dbforward "github.com/komari-monitor/komari/database/forward"
	"github.com/komari-monitor/komari/database/models"
	messageevent "github.com/komari-monitor/komari/database/models/messageEvent"
	"github.com/komari-monitor/komari/utils/messageSender"
	"gorm.io/gorm"
)

const (
	alertDedupWindow  = 5 * time.Minute
	alertAckSilence   = 24 * time.Hour
	latencyKeyDefault = "self"
)

type alertCandidate struct {
	alertType string
	eventType string
	severity  string
	message   string
	details   map[string]interface{}
	emoji     string
}

// EvaluateForwardAlerts 基于最新统计触发告警
func EvaluateForwardAlerts(stat *models.ForwardStat) {
	if stat == nil {
		return
	}
	cfg, err := dbforward.GetAlertConfig(stat.RuleID)
	if err != nil || !cfg.Enabled {
		return
	}
	rule, err := dbforward.GetForwardRule(stat.RuleID)
	if err != nil {
		return
	}
	var rc RuleConfig
	_ = json.Unmarshal([]byte(rule.ConfigJSON), &rc)

	candidates := buildAlertCandidates(stat, cfg, rule, rc)
	markAlertClears(stat, cfg, rule, rc, candidates)
	for _, cand := range candidates {
		if cand.alertType == "" {
			continue
		}
		if shouldSuppressAlert(stat.RuleID, cand.alertType) {
			continue
		}
		_ = sendForwardAlert(rule, cand)
	}
}

func markAlertClears(stat *models.ForwardStat, cfg *models.ForwardAlertConfig, rule *models.ForwardRule, rc RuleConfig, candidates []alertCandidate) {
	if stat == nil || cfg == nil || rule == nil {
		return
	}
	isEntry := rc.EntryNodeID != "" && stat.NodeID == rc.EntryNodeID
	active := map[string]struct{}{}
	for _, c := range candidates {
		if c.alertType != "" {
			active[c.alertType] = struct{}{}
		}
	}
	// 对本次评估涉及到的类型，如果当前没有触发，则视为已恢复（用于 ack 静默期内的再次触发放行）
	check := []string{}
	if cfg.NodeDownEnabled && !isEntry {
		check = append(check, "node_down")
	}
	if cfg.LinkDegradedEnabled && isEntry {
		check = append(check, "link_degraded")
	}
	if cfg.LinkFaultyEnabled && isEntry {
		check = append(check, "link_faulty")
	}
	if cfg.HighLatencyEnabled {
		check = append(check, "high_latency")
	}
	if cfg.TrafficSpikeEnabled && isEntry {
		check = append(check, "traffic_spike")
	}
	for _, t := range check {
		if _, ok := active[t]; ok {
			continue
		}
		setAlertClearedAt(stat.RuleID, t, time.Now().UTC())
	}
}

func buildAlertCandidates(stat *models.ForwardStat, cfg *models.ForwardAlertConfig, rule *models.ForwardRule, rc RuleConfig) []alertCandidate {
	candidates := make([]alertCandidate, 0, 4)
	isEntry := rc.EntryNodeID != "" && stat.NodeID == rc.EntryNodeID

	if isEntry {
		if strings.ToLower(stat.LinkStatus) == "faulty" && cfg.LinkFaultyEnabled {
			candidates = append(candidates, alertCandidate{
				alertType: "link_faulty",
				eventType: messageevent.ForwardLinkFaulty,
				severity:  "critical",
				message:   fmt.Sprintf("转发规则 [%s] 链路故障", rule.Name),
				details: map[string]interface{}{
					"node_id":   stat.NodeID,
					"rule_id":   stat.RuleID,
					"status":    stat.LinkStatus,
					"is_entry":  true,
					"timestamp": time.Now().UTC(),
				},
				emoji: "⛔",
			})
		}
		if strings.ToLower(stat.LinkStatus) == "degraded" && cfg.LinkDegradedEnabled {
			candidates = append(candidates, alertCandidate{
				alertType: "link_degraded",
				eventType: messageevent.ForwardLinkDegraded,
				severity:  "warning",
				message:   fmt.Sprintf("转发规则 [%s] 链路降级", rule.Name),
				details: map[string]interface{}{
					"node_id":   stat.NodeID,
					"rule_id":   stat.RuleID,
					"status":    stat.LinkStatus,
					"is_entry":  true,
					"timestamp": time.Now().UTC(),
				},
				emoji: "🟡",
			})
		}
	} else if strings.ToLower(stat.LinkStatus) == "faulty" && cfg.NodeDownEnabled {
		candidates = append(candidates, alertCandidate{
			alertType: "node_down",
			eventType: messageevent.ForwardNodeDown,
			severity:  "critical",
			message:   fmt.Sprintf("转发规则 [%s] 节点异常", rule.Name),
			details: map[string]interface{}{
				"node_id":   stat.NodeID,
				"rule_id":   stat.RuleID,
				"status":    stat.LinkStatus,
				"is_entry":  false,
				"timestamp": time.Now().UTC(),
			},
			emoji: "🔴",
		})
	}

	if cfg.HighLatencyEnabled {
		if latency, ok := parseLatency(stat.NodesLatency, latencyKeyDefault); ok && latency >= int64(cfg.HighLatencyThreshold) {
			candidates = append(candidates, alertCandidate{
				alertType: "high_latency",
				eventType: messageevent.ForwardHighLatency,
				severity:  "warning",
				message:   fmt.Sprintf("转发规则 [%s] 高延迟 (%dms)", rule.Name, latency),
				details: map[string]interface{}{
					"node_id":   stat.NodeID,
					"rule_id":   stat.RuleID,
					"latency":   latency,
					"threshold": cfg.HighLatencyThreshold,
					"timestamp": time.Now().UTC(),
				},
				emoji: "⏱️",
			})
		}
	}

	if cfg.TrafficSpikeEnabled {
		// 避免多节点重复触发：仅入口节点判断流量突增
		if isEntry {
			if spike := checkTrafficSpike(stat, cfg.TrafficSpikeThreshold); spike {
			candidates = append(candidates, alertCandidate{
				alertType: "traffic_spike",
				eventType: messageevent.ForwardTrafficSpike,
				severity:  "warning",
				message:   fmt.Sprintf("转发规则 [%s] 流量突增", rule.Name),
				details: map[string]interface{}{
					"node_id":   stat.NodeID,
					"rule_id":   stat.RuleID,
					"bytes":     stat.TrafficInBytes + stat.TrafficOutBytes,
					"threshold": cfg.TrafficSpikeThreshold,
					"timestamp": time.Now().UTC(),
				},
				emoji: "🚀",
			})
			}
		}
	}

	return candidates
}

func sendForwardAlert(rule *models.ForwardRule, cand alertCandidate) error {
	if rule == nil {
		return nil
	}
	detailsJSON, _ := json.Marshal(cand.details)
	eventType := cand.eventType
	if eventType == "" {
		eventType = cand.alertType
	}
	event := models.EventMessage{
		Event:   eventType,
		Time:    time.Now(),
		Message: cand.message,
		Emoji:   cand.emoji,
	}
	_ = messageSender.SendEvent(event)
	history := &models.ForwardAlertHistory{
		RuleID:    rule.ID,
		AlertType: cand.alertType,
		Severity:  cand.severity,
		Message:   cand.message,
		Details:   string(detailsJSON),
		CreatedAt: models.FromTime(time.Now()),
	}
	return dbforward.CreateAlertHistory(history)
}

func shouldSuppressAlert(ruleID uint, alertType string) bool {
	last, err := dbforward.GetLatestAlertByType(ruleID, alertType)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return false
		}
		return false
	}
	if !last.CreatedAt.ToTime().IsZero() && time.Since(last.CreatedAt.ToTime()) < alertDedupWindow {
		return true
	}
	if last.AcknowledgedAt != nil && !last.AcknowledgedAt.ToTime().IsZero() && time.Since(last.AcknowledgedAt.ToTime()) < alertAckSilence {
		// 若告警确认后已恢复过，则允许在静默期内再次触发
		if clearedAt, ok := getAlertClearedAt(ruleID, alertType); ok && clearedAt.After(last.AcknowledgedAt.ToTime()) {
			return false
		}
		return true
	}
	return false
}

func parseLatency(raw string, key string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	var data map[string]int64
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return 0, false
	}
	if v, ok := data[key]; ok {
		return v, true
	}
	for _, v := range data {
		return v, true
	}
	return 0, false
}

// 基于最近样本做简单倍数判断
func checkTrafficSpike(stat *models.ForwardStat, threshold float64) bool {
	if stat == nil {
		return false
	}
	if threshold <= 1 {
		threshold = 2.0
	}
	history, err := GetRecentTrafficHistory(stat.RuleID, stat.NodeID, 12)
	if err != nil || len(history) < 3 {
		return false
	}
	// history 表中存的是“桶内增量”，用最近桶的均值作为基线
	var sum int64
	var count int64
	for i := 0; i < len(history)-1; i++ { // 排除最后一个桶（可能是刚写入不完整或波动较大）
		v := history[i].TrafficInBytes + history[i].TrafficOutBytes
		if v <= 0 {
			continue
		}
		sum += v
		count++
	}
	if count == 0 {
		return false
	}
	avgBytesPerBucket := sum / count
	if avgBytesPerBucket <= 0 {
		return false
	}
	bucketSeconds := float64(historyBucketSeconds())
	if bucketSeconds <= 0 {
		bucketSeconds = 60
	}

	avgBps := float64(avgBytesPerBucket) * 8 / bucketSeconds
	currentBps := float64(stat.RealtimeBpsIn + stat.RealtimeBpsOut)
	if currentBps <= 0 {
		// fallback: 用最新桶的 bytes 与均值比较
		lastBytes := history[len(history)-1].TrafficInBytes + history[len(history)-1].TrafficOutBytes
		return float64(lastBytes) > float64(avgBytesPerBucket)*threshold
	}
	return currentBps > avgBps*threshold
}

func historyBucketSeconds() int64 {
	settings, err := dbforward.GetSystemSettings()
	if err != nil {
		return 60
	}
	switch strings.ToLower(strings.TrimSpace(settings.HistoryAggregatePeriod)) {
	case "10min":
		return int64((10 * time.Minute).Seconds())
	case "30min":
		return int64((30 * time.Minute).Seconds())
	case "1hour", "hour":
		return int64(time.Hour.Seconds())
	case "1day", "day":
		return int64((24 * time.Hour).Seconds())
	default:
		return int64(time.Hour.Seconds())
	}
}
