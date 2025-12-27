package forward

import (
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

func UpsertForwardInstanceStat(stat *models.ForwardInstanceStat) error {
	if stat == nil || stat.RuleID == 0 || stat.NodeID == "" || stat.InstanceID == "" {
		return nil
	}
	db := dbcore.GetDBInstance()
	if time.Time(stat.LastUpdatedAt).IsZero() {
		stat.LastUpdatedAt = models.FromTime(time.Now().UTC())
	}
	return db.Where("rule_id = ? AND node_id = ? AND instance_id = ?", stat.RuleID, stat.NodeID, stat.InstanceID).
		Assign(stat).
		FirstOrCreate(stat).Error
}

func GetForwardInstanceStat(ruleID uint, nodeID string, instanceID string) (*models.ForwardInstanceStat, error) {
	if ruleID == 0 || nodeID == "" || instanceID == "" {
		return nil, nil
	}
	var stat models.ForwardInstanceStat
	err := dbcore.GetDBInstance().Where("rule_id = ? AND node_id = ? AND instance_id = ?", ruleID, nodeID, instanceID).First(&stat).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &stat, nil
}

func ListForwardInstanceStats(ruleID uint) ([]models.ForwardInstanceStat, error) {
	if ruleID == 0 {
		return nil, nil
	}
	var stats []models.ForwardInstanceStat
	if err := dbcore.GetDBInstance().Where("rule_id = ?", ruleID).Find(&stats).Error; err != nil {
		return nil, err
	}
	return stats, nil
}

func ListForwardInstanceStatsByNode(ruleID uint, nodeID string) ([]models.ForwardInstanceStat, error) {
	if ruleID == 0 || nodeID == "" {
		return nil, nil
	}
	var stats []models.ForwardInstanceStat
	if err := dbcore.GetDBInstance().Where("rule_id = ? AND node_id = ?", ruleID, nodeID).Find(&stats).Error; err != nil {
		return nil, err
	}
	return stats, nil
}

func DeleteForwardInstanceStatsByRule(ruleID uint) error {
	if ruleID == 0 {
		return nil
	}
	return dbcore.GetDBInstance().Where("rule_id = ?", ruleID).Delete(&models.ForwardInstanceStat{}).Error
}
