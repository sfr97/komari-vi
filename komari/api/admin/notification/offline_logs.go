package notification

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/api"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
)

// GET /api/admin/notification/offline/logs?client=uuid&page=1&page_size=50
func ListOfflineConnectionLogs(c *gin.Context) {
	clientUUID := c.Query("client")
	if clientUUID == "" {
		api.RespondError(c, 400, "Missing client")
		return
	}

	pageStr := c.Query("page")
	if pageStr == "" {
		pageStr = "1"
	}
	pageSizeStr := c.Query("page_size")
	if pageSizeStr == "" {
		pageSizeStr = "50"
	}
	page, err := strconv.Atoi(pageStr)
	if err != nil || page <= 0 {
		api.RespondError(c, 400, "Invalid page: "+pageStr)
		return
	}
	pageSize, err := strconv.Atoi(pageSizeStr)
	if err != nil || pageSize <= 0 {
		api.RespondError(c, 400, "Invalid page_size: "+pageSizeStr)
		return
	}
	if pageSize > 200 {
		pageSize = 200
	}

	db := dbcore.GetDBInstance()
	var total int64
	if err := db.Model(&models.AgentConnectionLog{}).
		Where("client = ?", clientUUID).
		Count(&total).Error; err != nil {
		api.RespondError(c, 500, "Failed to count logs: "+err.Error())
		return
	}

	offset := (page - 1) * pageSize
	var logs []models.AgentConnectionLog
	if err := db.
		Where("client = ?", clientUUID).
		Order("connected_at desc").
		Limit(pageSize).
		Offset(offset).
		Find(&logs).Error; err != nil {
		api.RespondError(c, 500, "Failed to retrieve logs: "+err.Error())
		return
	}

	api.RespondSuccess(c, gin.H{
		"logs":      logs,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// GET /api/admin/notification/offline/logs/chart?client=uuid&limit=3000
// 用于统计图展示，最多返回 3000 条（不删除历史数据）。
func ListOfflineConnectionLogsChart(c *gin.Context) {
	clientUUID := c.Query("client")
	if clientUUID == "" {
		api.RespondError(c, 400, "Missing client")
		return
	}

	limitStr := c.Query("limit")
	if limitStr == "" {
		limitStr = "3000"
	}
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		api.RespondError(c, 400, "Invalid limit: "+limitStr)
		return
	}
	if limit > 3000 {
		limit = 3000
	}

	db := dbcore.GetDBInstance()
	var logs []models.AgentConnectionLog
	if err := db.
		Where("client = ?", clientUUID).
		Order("connected_at asc").
		Limit(limit).
		Find(&logs).Error; err != nil {
		api.RespondError(c, 500, "Failed to retrieve logs: "+err.Error())
		return
	}

	api.RespondSuccess(c, gin.H{
		"logs":  logs,
		"limit": limit,
	})
}
