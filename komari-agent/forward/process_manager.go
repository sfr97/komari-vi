package forward

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/komari-monitor/komari-agent/ws"
)

const (
	realmConfigDir = "/etc/komari-agent/realm"
	realmLogDir    = "/var/log/komari-agent"
)

type RealmProcess struct {
	RuleID      uint
	NodeID      string
	Cmd         *exec.Cmd
	ConfigPath  string
	LogPath     string
	StartTime   time.Time
	WaitDone    chan struct{}
	logFile     *os.File
	stopStats   chan struct{}
	statsIntv   time.Duration
	port        int
	protocol    string
	outTargets  []string
	collector   *StatsCollector
	statsInfo   StatsInfo
	linkMonitor *LinkHealthMonitor
	startReq    StartRealmRequest
	conn        *ws.SafeConn
	crashLimit  int
	crashCount  int
	stopping    bool
}

type ProcessManager struct {
	mu        sync.Mutex
	processes map[string]*RealmProcess
	health    *HealthChecker
}

func NewProcessManager(health *HealthChecker) *ProcessManager {
	return &ProcessManager{
		processes: make(map[string]*RealmProcess),
		health:    health,
	}
}

// Snapshot returns a point-in-time view of all running processes.
// Callers must treat the returned pointers as read-only.
func (m *ProcessManager) Snapshot() []*RealmProcess {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*RealmProcess, 0, len(m.processes))
	for _, p := range m.processes {
		out = append(out, p)
	}
	return out
}

// RebindConn updates the WS connection used for periodic stats reporting.
// If conn is nil, existing collectors are stopped to avoid log spam; processes keep running.
func (m *ProcessManager) RebindConn(conn *ws.SafeConn) {
	if m == nil {
		return
	}
	procs := m.Snapshot()

	// Update stored conn and stop old collectors (if any).
	m.mu.Lock()
	for _, p := range procs {
		if p == nil {
			continue
		}
		p.conn = conn
		if p.collector != nil {
			p.collector.Stop()
			p.collector = nil
		}
	}
	m.mu.Unlock()

	if conn == nil {
		return
	}
	for _, p := range procs {
		if p == nil {
			continue
		}
		// Adopt 模式下 Cmd 为空：通过 configPath 探测是否仍有 realm 在跑。
		if p.Cmd == nil || p.Cmd.Process == nil {
			if len(findRealmPIDsByConfigPath(p.ConfigPath)) == 0 {
				continue
			}
		}
		go m.startStatsLoop(conn, p, p.RuleID, p.NodeID)
	}
}

func statsDuration(sec int) time.Duration {
	if sec <= 0 {
		return 10 * time.Second
	}
	return time.Duration(sec) * time.Second
}

func buildPaths(ruleID uint, nodeID string) (configPath, logPath string) {
	configName := fmt.Sprintf("realm-rule-%d-node-%s.toml", ruleID, nodeID)
	logName := fmt.Sprintf("realm-rule-%d-node-%s.log", ruleID, nodeID)
	return filepath.Join(realmConfigDir, configName), filepath.Join(realmLogDir, logName)
}

func (m *ProcessManager) Start(req StartRealmRequest, conn *ws.SafeConn) (*RealmProcess, error) {
	key := m.key(req.RuleID, req.NodeID)

	if err := os.MkdirAll(realmConfigDir, 0o755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	if err := os.MkdirAll(realmLogDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}

	finalConfig := rewriteRealmListen(req.Config, req.Port)

	configPath, logPath := buildPaths(req.RuleID, req.NodeID)
	if err := os.WriteFile(configPath, []byte(finalConfig), 0o644); err != nil {
		return nil, fmt.Errorf("write realm config: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command(findRealmBinaryPath(), "-c", configPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	outTargets := parseRealmOutTargets(finalConfig)
	statsInfo, statsErr := setupStatsRules(req.RuleID, req.Port, req.Protocol, outTargets)
	if statsErr != nil {
		log.Printf("setup traffic stats failed but continue (method=%s): %v", statsInfo.Method, statsErr)
	}

	if err := cmd.Start(); err != nil {
		cleanupStatsRules(statsInfo, req.RuleID, req.Port, req.Protocol, outTargets)
		_ = logFile.Close()
		return nil, fmt.Errorf("start realm: %w", err)
	}

	proc := &RealmProcess{
		RuleID:     req.RuleID,
		NodeID:     req.NodeID,
		Cmd:        cmd,
		ConfigPath: configPath,
		LogPath:    logPath,
		StartTime:  time.Now(),
		WaitDone:   make(chan struct{}),
		logFile:    logFile,
		stopStats:  make(chan struct{}),
		statsIntv:  statsDuration(req.StatsInterval),
		port:       req.Port,
		protocol:   req.Protocol,
		outTargets: outTargets,
		statsInfo:  statsInfo,
		startReq:   req,
		conn:       conn,
		crashLimit: crashLimit(req.CrashRestartLimit),
	}
	if req.EntryNodeID != "" && req.NodeID == req.EntryNodeID && (req.HealthCheckNextHop != "" || req.HealthCheckTarget != "") {
		proc.linkMonitor = NewLinkHealthMonitor(req.Protocol, req.HealthCheckNextHop, req.HealthCheckTarget, req.HealthCheckInterval)
		proc.linkMonitor.Start()
	}

	m.mu.Lock()
	m.processes[key] = proc
	m.mu.Unlock()

	go m.waitForExit(key, proc)
	if conn != nil {
		go m.startStatsLoop(conn, proc, req.RuleID, req.NodeID)
	}
	return proc, nil
}

func (m *ProcessManager) Get(ruleID uint, nodeID string) (*RealmProcess, bool) {
	key := m.key(ruleID, nodeID)
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.processes[key]
	return p, ok
}

// AdoptExisting 尝试“接管”一个由旧 Agent 启动、但当前进程不是其父进程的 realm。
// 注意：此模式下不会对统计链做 flush/create，避免流量计数被重置；仅恢复 stats 上报与优先级监控能力。
func (m *ProcessManager) AdoptExisting(req StartRealmRequest, conn *ws.SafeConn) (*RealmProcess, bool, error) {
	if m == nil {
		return nil, false, nil
	}
	if _, ok := m.Get(req.RuleID, req.NodeID); ok {
		return nil, false, nil
	}
	configPath, logPath := buildPaths(req.RuleID, req.NodeID)
	pids := findRealmPIDsByConfigPath(configPath)
	if len(pids) == 0 {
		return nil, false, nil
	}
	if req.Port <= 0 {
		return nil, false, errors.New("invalid port")
	}

	outTargets := parseRealmOutTargets(req.Config)
	proc := &RealmProcess{
		RuleID:     req.RuleID,
		NodeID:     req.NodeID,
		Cmd:        nil, // 非当前进程启动的 child，Stop 时走 pid 扫描
		ConfigPath: configPath,
		LogPath:    logPath,
		StartTime:  time.Now(),
		WaitDone:   make(chan struct{}),
		stopStats:  make(chan struct{}),
		statsIntv:  statsDuration(req.StatsInterval),
		port:       req.Port,
		protocol:   req.Protocol,
		outTargets: outTargets,
		statsInfo:  detectExistingStatsInfo(req.RuleID, req.Port),
		startReq:   req,
		conn:       conn,
		crashLimit: crashLimit(req.CrashRestartLimit),
	}
	if req.EntryNodeID != "" && req.NodeID == req.EntryNodeID && (req.HealthCheckNextHop != "" || req.HealthCheckTarget != "") {
		proc.linkMonitor = NewLinkHealthMonitor(req.Protocol, req.HealthCheckNextHop, req.HealthCheckTarget, req.HealthCheckInterval)
		proc.linkMonitor.Start()
	}

	key := m.key(req.RuleID, req.NodeID)
	m.mu.Lock()
	m.processes[key] = proc
	m.mu.Unlock()

	if conn != nil {
		go m.startStatsLoop(conn, proc, req.RuleID, req.NodeID)
	}
	return proc, true, nil
}

func findRealmBinaryPath() string {
	// 首选 PATH，其次尝试常见安装位置（prepare env 会优先写入 /usr/local/bin/realm）
	if path, err := exec.LookPath("realm"); err == nil {
		return path
	}
	candidates := []string{"/usr/local/bin/realm", "/usr/bin/realm"}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "realm"
}

func (m *ProcessManager) waitForExit(key string, proc *RealmProcess) {
	err := proc.Cmd.Wait()
	if proc.logFile != nil {
		_ = proc.logFile.Close()
	}
	if proc.stopStats != nil {
		close(proc.stopStats)
		proc.stopStats = nil
	}
	if proc.collector != nil {
		proc.collector.Stop()
		proc.collector = nil
	}
	if proc.linkMonitor != nil {
		proc.linkMonitor.Stop()
		proc.linkMonitor = nil
	}

	if err != nil && !proc.stopping && proc.crashLimit > 0 && proc.crashCount < proc.crashLimit {
		proc.crashCount++
		log.Printf("realm process %s crashed (%d/%d), restarting...", key, proc.crashCount, proc.crashLimit)
		time.Sleep(5 * time.Second)
		m.mu.Lock()
		delete(m.processes, key)
		m.mu.Unlock()
		close(proc.WaitDone)
		if _, restartErr := m.Start(proc.startReq, proc.conn); restartErr == nil {
			return
		}
	}

	if err != nil {
		log.Printf("realm process %s exited with error: %v", key, err)
	}
	cleanupStatsRules(proc.statsInfo, proc.RuleID, proc.port, proc.protocol, proc.outTargets)

	m.mu.Lock()
	delete(m.processes, key)
	m.mu.Unlock()
	close(proc.WaitDone)
}

func (m *ProcessManager) Stop(ruleID uint, nodeID string, timeout time.Duration, protocol string, port int) error {
	key := m.key(ruleID, nodeID)

	m.mu.Lock()
	proc, ok := m.processes[key]
	m.mu.Unlock()
	if !ok {
		// 兼容：Agent 重启后 map 丢失，尝试按 config path 扫描并停止遗留进程
		configPath, _ := buildPaths(ruleID, nodeID)
		pids := findRealmPIDsByConfigPath(configPath)
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		waitPIDsExit(pids, timeout)
		for _, pid := range pids {
			if pidAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		// best-effort cleanup stats rules for this rule/port
		cfgBytes, _ := os.ReadFile(configPath)
		if port <= 0 {
			if p, ok := parseRealmListenPort(string(cfgBytes)); ok {
				port = p
			}
		}
		if port > 0 {
			outTargets := parseRealmOutTargets(string(cfgBytes))
			if strings.TrimSpace(protocol) == "" {
				protocol = "both"
			}
			cleanupStatsRules(detectExistingStatsInfo(ruleID, port), ruleID, port, protocol, outTargets)
		}
		return nil
	}

	// 无论是否 child，都优先停止采集循环
	if proc.stopStats != nil {
		select {
		case <-proc.stopStats:
		default:
			close(proc.stopStats)
		}
		proc.stopStats = nil
	}
	if proc.collector != nil {
		proc.collector.Stop()
		proc.collector = nil
	}
	if proc.linkMonitor != nil {
		proc.linkMonitor.Stop()
		proc.linkMonitor = nil
	}

	// 常规 child 进程停止路径
	if proc.Cmd != nil && proc.Cmd.Process != nil {
		proc.stopping = true
		_ = proc.Cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-proc.WaitDone:
		case <-time.After(timeout):
			_ = proc.Cmd.Process.Kill()
			<-proc.WaitDone
		}
		return nil
	}

	// 非 child（旧 Agent 遗留）：按 config path 找 PID 并 kill + 轮询退出
	proc.stopping = true
	pids := findRealmPIDsByConfigPath(proc.ConfigPath)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	waitPIDsExit(pids, timeout)
	for _, pid := range pids {
		if pidAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	cleanupStatsRules(proc.statsInfo, proc.RuleID, proc.port, proc.protocol, proc.outTargets)
	m.mu.Lock()
	delete(m.processes, key)
	m.mu.Unlock()
	if proc.WaitDone != nil {
		select {
		case <-proc.WaitDone:
		default:
			close(proc.WaitDone)
		}
	}
	return nil
}

func (m *ProcessManager) Update(req UpdateRealmRequest, conn *ws.SafeConn) (*RealmProcess, error) {
	timeout := stopDuration(req.StopTimeout)
	if err := m.Stop(req.RuleID, req.NodeID, timeout, req.Protocol, 0); err != nil {
		return nil, err
	}
	return m.Start(StartRealmRequest{
		RuleID:              req.RuleID,
		NodeID:              req.NodeID,
		EntryNodeID:         req.EntryNodeID,
		Protocol:            req.Protocol,
		Config:              req.NewConfig,
		Port:                req.NewPort,
		StatsInterval:       req.StatsInterval,
		HealthCheckInterval: req.HealthCheckInterval,
		HealthCheckNextHop:  req.HealthCheckNextHop,
		HealthCheckTarget:   req.HealthCheckTarget,
		CrashRestartLimit:   req.CrashRestartLimit,
		StopTimeout:         req.StopTimeout,
		PriorityConfigs:     req.PriorityConfigs,
		PriorityRelays:      req.PriorityRelays,
		ActiveRelayNodeID:   req.ActiveRelayNodeID,
		PriorityListenPort:  req.PriorityListenPort,
	}, conn)
}

func (m *ProcessManager) key(ruleID uint, nodeID string) string {
	return fmt.Sprintf("rule-%d-node-%s", ruleID, nodeID)
}

func (m *ProcessManager) startStatsLoop(conn *ws.SafeConn, proc *RealmProcess, ruleID uint, nodeID string) {
	if conn == nil || proc == nil {
		return
	}
	var relayIDs []string
	if len(proc.startReq.PriorityRelays) > 0 {
		relayIDs = make([]string, 0, len(proc.startReq.PriorityRelays))
		for _, r := range proc.startReq.PriorityRelays {
			if r.NodeID != "" {
				relayIDs = append(relayIDs, r.NodeID)
			}
		}
	}
	collector := NewStatsCollector(m.health, int(proc.statsIntv/time.Second), relayIDs, proc.startReq.ActiveRelayNodeID)
	proc.collector = collector
	collector.statsInfo = proc.statsInfo
	collector.linkMonitor = proc.linkMonitor
	collector.StartLoop(conn, ruleID, nodeID, proc.port, proc.protocol)
}

func crashLimit(val int) int {
	if val <= 0 {
		return 3
	}
	return val
}

func stopDuration(val int) time.Duration {
	if val <= 0 {
		return 5 * time.Second
	}
	return time.Duration(val) * time.Second
}
