//go:build !linux
// +build !linux

package forward

import "time"

func pidAlive(pid int) bool { return false }

func waitPIDsExit(pids []int, timeout time.Duration) {}

func findRealmPIDsByConfigPath(configPath string) []int { return nil }
