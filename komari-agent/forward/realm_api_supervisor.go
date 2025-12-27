package forward

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const realmApiLogPath = "/var/log/komari-agent/realm-api.log"

type RealmApiStatus struct {
	Pid     int
	Port    int
	Version string
}

type RealmApiSupervisor struct {
	mu sync.Mutex

	realmPath string
	version   string

	cmd  *exec.Cmd
	port int
	gen  uint64

	logFile *os.File

	// restart control
	stopCh chan struct{}
}

func NewRealmApiSupervisor() *RealmApiSupervisor {
	return &RealmApiSupervisor{
		stopCh: make(chan struct{}),
	}
}

func (s *RealmApiSupervisor) Ensure(req RealmApiEnsureRequest) (RealmApiStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	realmPath, version, err := ensureRealmBinary(req.RealmDownloadURL, req.ForceReinstall)
	if err != nil {
		return RealmApiStatus{}, err
	}
	s.realmPath = realmPath
	s.version = version

	if !req.Restart && s.isHealthyLocked() {
		return s.statusLocked(), nil
	}

	if err := s.restartLocked(); err != nil {
		return RealmApiStatus{}, err
	}
	return s.statusLocked(), nil
}

func (s *RealmApiSupervisor) BaseURL() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.port <= 0 {
		return "", false
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.port), true
}

func (s *RealmApiSupervisor) statusLocked() RealmApiStatus {
	pid := 0
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	return RealmApiStatus{
		Pid:     pid,
		Port:    s.port,
		Version: s.version,
	}
}

func (s *RealmApiSupervisor) isHealthyLocked() bool {
	if s.cmd == nil || s.cmd.Process == nil || s.port <= 0 {
		return false
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/instances", s.port)
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (s *RealmApiSupervisor) restartLocked() error {
	if s.realmPath == "" {
		return errors.New("realm binary path is empty")
	}

	s.killLocked()

	for attempt := 0; attempt < 10; attempt++ {
		port, err := pickAvailableLoopbackPort()
		if err != nil {
			return err
		}

		cmd := exec.Command(
			s.realmPath,
			"api",
			"--bind",
			"127.0.0.1",
			"--port",
			strconv.Itoa(port),
		)

		var logFile *os.File
		if err := os.MkdirAll(filepath.Dir(realmApiLogPath), 0o755); err == nil {
			if f, err := os.OpenFile(realmApiLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				logFile = f
				cmd.Stdout = f
				cmd.Stderr = f
			}
		}

		if err := cmd.Start(); err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			continue
		}

		s.cmd = cmd
		s.port = port
		s.gen++
		s.logFile = logFile
		gen := s.gen

		go s.waitAndRestart(cmd, gen)

		// wait for healthy
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if s.isHealthyLocked() {
				return nil
			}
			time.Sleep(150 * time.Millisecond)
		}

		s.killLocked()
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("failed to start realm api after retries")
}

func (s *RealmApiSupervisor) killLocked() {
	if s.cmd == nil || s.cmd.Process == nil {
		s.cmd = nil
		s.port = 0
		if s.logFile != nil {
			_ = s.logFile.Close()
			s.logFile = nil
		}
		return
	}

	_ = s.cmd.Process.Kill()

	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}

	s.cmd = nil
	s.port = 0
}

func (s *RealmApiSupervisor) waitAndRestart(cmd *exec.Cmd, gen uint64) {
	_ = cmd.Wait()

	select {
	case <-s.stopCh:
		return
	default:
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A newer process is already running (or an intentional stop happened).
	if s.cmd != cmd || s.gen != gen {
		return
	}

	s.cmd = nil
	s.port = 0
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}

	// Best-effort restart; if it fails, the next Ensure() will retry.
	_ = s.restartLocked()
}

func pickAvailableLoopbackPort() (int, error) {
	// Prefer "pick a free port" by probing, to avoid relying on a single OS-picked port.
	// Still not race-free; restartLocked() will retry if the port becomes unavailable.
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 50; i++ {
		port := 20000 + rnd.Intn(40000) // 20000-59999
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	// fallback: ask OS for an ephemeral port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr == nil {
		_ = ln.Close()
		return 0, errors.New("unexpected listener addr type")
	}
	port := addr.Port
	_ = ln.Close()
	if port <= 0 {
		return 0, errors.New("picked port is invalid")
	}
	return port, nil
}
