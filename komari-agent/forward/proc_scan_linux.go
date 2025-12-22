//go:build linux
// +build linux

package forward

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func waitPIDsExit(pids []int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allDead := true
		for _, pid := range pids {
			if pidAlive(pid) {
				allDead = false
				break
			}
		}
		if allDead {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func findRealmPIDsByConfigPath(configPath string) []int {
	if configPath == "" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	needle := []byte(configPath)
	out := make([]int, 0, 4)
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 1 {
			continue
		}
		cmdlinePath := filepath.Join("/proc", ent.Name(), "cmdline")
		cmdline, err := os.ReadFile(cmdlinePath)
		if err != nil || len(cmdline) == 0 {
			continue
		}
		if !bytes.Contains(cmdline, []byte("realm")) {
			continue
		}
		if !bytes.Contains(cmdline, needle) {
			continue
		}
		out = append(out, pid)
	}
	return out
}
