package forward

import (
	"log"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	dbforward "github.com/komari-monitor/komari/database/forward"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

var (
	historyMaintMu       sync.Mutex
	historyMaintLastDate string
	historyMaintRunning  bool
)

// MaybeRunHistoryMaintenance 按天执行历史数据聚合/清理（满足方案的 30天/1年/3年 规则）。
// 该函数可被高频调用（例如每30分钟一次），内部会保证每天只运行一次且不会并发重入。
func MaybeRunHistoryMaintenance(now time.Time) {
	now = now.UTC()
	dateKey := now.Format("2006-01-02")

	historyMaintMu.Lock()
	if historyMaintRunning || historyMaintLastDate == dateKey {
		historyMaintMu.Unlock()
		return
	}
	historyMaintRunning = true
	historyMaintLastDate = dateKey
	historyMaintMu.Unlock()

	go func() {
		defer func() {
			historyMaintMu.Lock()
			historyMaintRunning = false
			historyMaintMu.Unlock()
		}()
		if err := RunHistoryMaintenance(now, 30); err != nil {
			log.Printf("forward history maintenance failed: %v", err)
		}
	}()
}

// RunHistoryMaintenance 执行历史聚合与清理。
// maxCatchupDays 用于补偿停机/禁用期间的欠账，避免一次任务处理过久。
func RunHistoryMaintenance(now time.Time, maxCatchupDays int) error {
	now = now.UTC()
	if maxCatchupDays <= 0 {
		maxCatchupDays = 1
	}
	if maxCatchupDays > 30 {
		maxCatchupDays = 30
	}
	db := dbcore.GetDBInstance()

	// 1) 30天以上：聚合到小时（仅处理刚刚跨过 30天 门槛的最近 maxCatchupDays 天）
	cutoff30d := startOfDayUTC(now.AddDate(0, 0, -30))
	cutoff1y := startOfDayUTC(now.AddDate(-1, 0, 0))
	for i := 0; i < maxCatchupDays; i++ {
		end := cutoff30d.Add(-time.Duration(i) * 24 * time.Hour)
		start := end.Add(-24 * time.Hour)
		if end.Before(cutoff1y) || end.Equal(cutoff1y) {
			break
		}
		if start.Before(cutoff1y) {
			start = cutoff1y
		}
		if !start.Before(end) {
			continue
		}
		if err := aggregateWindow(db, start, end, time.Hour); err != nil {
			return err
		}
	}

	// 2) 1年以上：聚合到天（仅处理刚刚跨过 1年 门槛的最近 maxCatchupDays 天）
	cutoff3y := startOfDayUTC(now.AddDate(-3, 0, 0))
	for i := 0; i < maxCatchupDays; i++ {
		end := cutoff1y.Add(-time.Duration(i) * 24 * time.Hour)
		start := end.Add(-24 * time.Hour)
		if end.Before(cutoff3y) || end.Equal(cutoff3y) {
			break
		}
		if start.Before(cutoff3y) {
			start = cutoff3y
		}
		if !start.Before(end) {
			continue
		}
		if err := aggregateWindow(db, start, end, 24*time.Hour); err != nil {
			return err
		}
	}

	// 3) 3年以上：清理
	if err := db.Where("timestamp < ?", models.FromTime(cutoff3y)).Delete(&models.ForwardTrafficHistory{}).Error; err != nil {
		return err
	}

	return nil
}

func startOfDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// aggregateWindow 将 [start, end) 的数据聚合到 bucketSize 对齐的桶，并删除桶内非桶起始时间的原始点。
func aggregateWindow(db *gorm.DB, start time.Time, end time.Time, bucketSize time.Duration) error {
	if db == nil {
		return nil
	}
	start = start.UTC()
	end = end.UTC()
	if !start.Before(end) {
		return nil
	}
	if bucketSize <= 0 {
		return nil
	}

	// 按桶逐段处理，避免一次加载过多数据
	cur := alignBucketStart(start, bucketSize)
	for cur.Before(end) {
		next := cur.Add(bucketSize)
		if next.After(end) {
			next = end
		}
		if err := aggregateOneBucket(db, cur, next); err != nil {
			return err
		}
		cur = cur.Add(bucketSize)
	}
	return nil
}

func alignBucketStart(t time.Time, bucketSize time.Duration) time.Time {
	if bucketSize == 24*time.Hour {
		return startOfDayUTC(t)
	}
	return t.Truncate(bucketSize)
}

func aggregateOneBucket(db *gorm.DB, bucketStart time.Time, bucketEnd time.Time) error {
	var rows []models.ForwardTrafficHistory
	if err := db.Where("timestamp >= ? AND timestamp < ?", models.FromTime(bucketStart), models.FromTime(bucketEnd)).
		Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	type agg struct {
		ruleID      uint
		nodeID      string
		inSum       int64
		outSum      int64
		connSum     int64
		latSum      int64
		sampleCount int64
	}
	type key struct {
		ruleID uint
		nodeID string
	}
	acc := make(map[key]*agg)
	for _, r := range rows {
		k := key{ruleID: r.RuleID, nodeID: r.NodeID}
		a := acc[k]
		if a == nil {
			a = &agg{ruleID: r.RuleID, nodeID: r.NodeID}
			acc[k] = a
		}
		a.inSum += r.TrafficInBytes
		a.outSum += r.TrafficOutBytes
		a.connSum += int64(r.Connections)
		a.latSum += int64(r.AvgLatencyMs)
		a.sampleCount++
	}

	for _, a := range acc {
		connAvg := 0
		latAvg := 0
		if a.sampleCount > 0 {
			connAvg = int(a.connSum / a.sampleCount)
			latAvg = int(a.latSum / a.sampleCount)
		}
		entry := &models.ForwardTrafficHistory{
			RuleID:          a.ruleID,
			NodeID:          a.nodeID,
			Timestamp:       models.FromTime(bucketStart),
			Connections:     connAvg,
			TrafficInBytes:  a.inSum,
			TrafficOutBytes: a.outSum,
			AvgLatencyMs:    latAvg,
		}
		if err := dbforward.UpsertTrafficHistory(entry); err != nil {
			return err
		}
	}

	// 删除桶内除 bucketStart 之外的点（保留 bucketStart 作为聚合后的唯一点）
	return db.Where("timestamp > ? AND timestamp < ?", models.FromTime(bucketStart), models.FromTime(bucketEnd)).
		Delete(&models.ForwardTrafficHistory{}).Error
}
