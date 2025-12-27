package forward

import (
	"sync"
	"time"
)

type alertState struct {
	clearedAt time.Time
}

var (
	alertStateMu sync.Mutex
	alertStates  = map[uint]map[string]alertState{} // ruleID -> alertType -> state
)

func setAlertClearedAt(ruleID uint, alertType string, at time.Time) {
	if ruleID == 0 || alertType == "" {
		return
	}
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	m := alertStates[ruleID]
	if m == nil {
		m = map[string]alertState{}
		alertStates[ruleID] = m
	}
	m[alertType] = alertState{clearedAt: at}
}

func getAlertClearedAt(ruleID uint, alertType string) (time.Time, bool) {
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	m := alertStates[ruleID]
	if m == nil {
		return time.Time{}, false
	}
	st, ok := m[alertType]
	if !ok || st.clearedAt.IsZero() {
		return time.Time{}, false
	}
	return st.clearedAt, true
}
