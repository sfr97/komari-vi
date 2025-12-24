package connectionlog

import (
	"log"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

// RecordConnect 在 agent WebSocket 建立时写入一条会话日志（断开时间/在线时长为空）。
func RecordConnect(clientUUID string, connectionID int64) {
	db := dbcore.GetDBInstance()
	now := models.FromTime(time.Now())

	entry := models.AgentConnectionLog{
		Client:       clientUUID,
		ConnectionID: connectionID,
		ConnectedAt:  now,
	}
	if err := db.Create(&entry).Error; err != nil {
		// 唯一键冲突或其他错误都不应影响主流程
		log.Printf("connectionlog.RecordConnect failed: client=%s connID=%d err=%v", clientUUID, connectionID, err)
	}
}

// RecordDisconnect 在 agent WebSocket 断开时补齐断开时间与在线时长。
func RecordDisconnect(clientUUID string, connectionID int64) {
	db := dbcore.GetDBInstance()
	now := models.FromTime(time.Now())

	var entry models.AgentConnectionLog
	err := db.
		Where("client = ? AND connection_id = ? AND disconnected_at IS NULL", clientUUID, connectionID).
		First(&entry).Error
	if err != nil {
		// 找不到（或已补齐）都属于正常情况
		return
	}

	connectedAt := entry.ConnectedAt.ToTime()
	onlineSeconds := int64(now.ToTime().Sub(connectedAt).Seconds())
	if onlineSeconds < 0 {
		onlineSeconds = 0
	}

	dis := now
	if err := db.Model(&models.AgentConnectionLog{}).
		Where("id = ? AND disconnected_at IS NULL", entry.ID).
		Updates(map[string]any{
			"disconnected_at": &dis,
			"online_seconds":  &onlineSeconds,
		}).Error; err != nil {
		log.Printf("connectionlog.RecordDisconnect failed: client=%s connID=%d err=%v", clientUUID, connectionID, err)
	}
}

// CloseAllOpenOnStartup 用于服务端重启时补齐遗留的“仍在线”记录。
// 断开时间取启动时间，在线时长按 connected_at -> 启动时间 计算。
func CloseAllOpenOnStartup(now time.Time) {
	db := dbcore.GetDBInstance()
	var open []models.AgentConnectionLog
	if err := db.Where("disconnected_at IS NULL").Find(&open).Error; err != nil {
		log.Printf("connectionlog.CloseAllOpenOnStartup query failed: %v", err)
		return
	}
	if len(open) == 0 {
		return
	}

	nowLT := models.FromTime(now)
	for _, entry := range open {
		connectedAt := entry.ConnectedAt.ToTime()
		onlineSeconds := int64(now.Sub(connectedAt).Seconds())
		if onlineSeconds < 0 {
			onlineSeconds = 0
		}
		dis := nowLT
		if err := db.Model(&models.AgentConnectionLog{}).
			Where("id = ? AND disconnected_at IS NULL", entry.ID).
			Updates(map[string]any{
				"disconnected_at": &dis,
				"online_seconds":  &onlineSeconds,
			}).Error; err != nil && err != gorm.ErrRecordNotFound {
			log.Printf("connectionlog.CloseAllOpenOnStartup update failed: id=%d err=%v", entry.ID, err)
		}
	}
}
