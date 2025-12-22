package forward

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	pkg_flags "github.com/komari-monitor/komari-agent/cmd/flags"
	"github.com/komari-monitor/komari-agent/dnsresolver"
	"github.com/komari-monitor/komari-agent/ws"
)

type Manager struct {
	process  *ProcessManager
	firewall *FirewallManager
	health   *HealthChecker
	switcher *PrioritySwitcher
}

func NewManager() *Manager {
	health := NewHealthChecker()
	process := NewProcessManager(health)
	return &Manager{
		process:  process,
		firewall: NewFirewallManager(),
		health:   health,
		switcher: NewPrioritySwitcher(health, process),
	}
}

// RebindConn 绑定/更新当前 WS 连接，用于统计上报与 priority 策略的自动切换。
// 断线时传入 nil，可停止统计循环避免持续报错日志；Realm 进程本身不会被停止。
func (m *Manager) RebindConn(conn *ws.SafeConn) {
	if m == nil || m.process == nil {
		return
	}
	procs := m.process.Snapshot()
	m.process.RebindConn(conn)

	if m.switcher == nil {
		return
	}

	// 断线时停止所有 priority 监控（避免使用旧连接/重复 goroutine）。
	if conn == nil {
		for _, p := range procs {
			if p == nil {
				continue
			}
			m.switcher.Stop(p.RuleID)
		}
		return
	}

	// 重连后：为入口节点的 priority 规则恢复监控。
	for _, p := range procs {
		if p == nil {
			continue
		}
		req := p.startReq
		if req.EntryNodeID == "" || req.NodeID != req.EntryNodeID {
			continue
		}
		if len(req.PriorityConfigs) == 0 || len(req.PriorityRelays) == 0 {
			continue
		}
		m.switcher.Stop(req.RuleID)
		go m.switcher.MonitorAndSwitch(conn, req.RuleID, req.EntryNodeID, req.PriorityRelays, req.PriorityConfigs, req.ActiveRelayNodeID, p.port, p.statsIntv, req.Protocol, req.HealthCheckInterval, req.HealthCheckTarget)
	}
}

func (m *Manager) HandleTask(conn *ws.SafeConn, env TaskEnvelope) (interface{}, error) {
	switch env.TaskType {
	case TaskCheckPort:
		var req CheckPortRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleCheckPort(req), nil
	case TaskPrepareForwardEnv:
		var req PrepareForwardEnvRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handlePrepareEnv(req), nil
	case TaskStartRealm:
		var req StartRealmRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleStartRealm(conn, req), nil
	case TaskStopRealm:
		var req StopRealmRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleStopRealm(req), nil
	case TaskUpdateRealm:
		var req UpdateRealmRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleUpdateRealm(conn, req), nil
	case TaskGetRealmLog:
		var req GetRealmLogRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleGetRealmLog(req), nil
	case TaskClearRealmLog:
		var req ClearRealmLogRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleClearRealmLog(req), nil
	case TaskDeleteRealmLog:
		var req DeleteRealmLogRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleDeleteRealmLog(req), nil
	case TaskTestConnectivity:
		var req TestConnectivityRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return m.handleTestConnectivity(req), nil
	default:
		return nil, fmt.Errorf("unknown forward task type: %s", env.TaskType)
	}
}

func (m *Manager) handleCheckPort(req CheckPortRequest) CheckPortResponse {
	port, err := findAvailablePort(req.PortSpec, req.ExcludedPorts)
	if err != nil {
		return CheckPortResponse{Success: false, Message: err.Error()}
	}
	return CheckPortResponse{
		Success:       true,
		AvailablePort: &port,
		Message:       fmt.Sprintf("Port %d is available", port),
	}
}

func (m *Manager) handlePrepareEnv(req PrepareForwardEnvRequest) PrepareForwardEnvResponse {
	tool := m.firewall.Detect()

	if err := ensureTrafficStatsTools(); err != nil {
		return PrepareForwardEnvResponse{
			Success:      false,
			FirewallTool: string(tool),
			Message:      err.Error(),
		}
	}

	realmPath, version, err := ensureRealmBinary(req.RealmDownloadURL, req.ForceReinstall)
	if err != nil {
		return PrepareForwardEnvResponse{
			Success:      false,
			FirewallTool: string(tool),
			Message:      err.Error(),
		}
	}
	if err := os.MkdirAll(realmConfigDir, 0o755); err != nil {
		return PrepareForwardEnvResponse{
			Success:      false,
			FirewallTool: string(tool),
			Message:      fmt.Sprintf("prepare config dir: %v", err),
		}
	}
	if err := os.MkdirAll(realmLogDir, 0o755); err != nil {
		return PrepareForwardEnvResponse{
			Success:      false,
			FirewallTool: string(tool),
			Message:      fmt.Sprintf("prepare log dir: %v", err),
		}
	}
	return PrepareForwardEnvResponse{
		Success:      true,
		FirewallTool: string(tool),
		RealmVersion: version,
		Message:      fmt.Sprintf("realm ready at %s", realmPath),
	}
}

func ensureTrafficStatsTools() error {
	// 统计优先支持 nftables（需实际存在可用的 input/output hook 链）；否则安装/使用 iptables 作为保底。
	if _, ok := detectNftStatsBase(); ok {
		return nil
	}
	// 允许 IPv4-only / IPv6-only 场景；至少需要一种 iptables 家族存在。
	if _, err := exec.LookPath("iptables"); err == nil {
		return nil
	}
	if _, err := exec.LookPath("ip6tables"); err == nil {
		return nil
	}

	installers := []struct {
		name string
		args []string
	}{
		{name: "apt-get", args: []string{"update"}},
		{name: "apt-get", args: []string{"install", "-y", "iptables"}},
		{name: "dnf", args: []string{"install", "-y", "iptables"}},
		{name: "yum", args: []string{"install", "-y", "iptables"}},
		{name: "apk", args: []string{"add", "--no-cache", "iptables"}},
		{name: "pacman", args: []string{"-Sy", "--noconfirm", "iptables"}},
		{name: "zypper", args: []string{"--non-interactive", "install", "iptables"}},
	}

	var attempted []string
	for _, inst := range installers {
		if _, err := exec.LookPath(inst.name); err != nil {
			continue
		}
		attempted = append(attempted, inst.name+" "+strings.Join(inst.args, " "))
		cmd := exec.Command(inst.name, inst.args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// 尝试 sudo（在 Agent 非 root 场景下）
			if _, sudoErr := exec.LookPath("sudo"); sudoErr == nil {
				sudoArgs := append([]string{inst.name}, inst.args...)
				sudoCmd := exec.Command("sudo", sudoArgs...)
				out2, err2 := sudoCmd.CombinedOutput()
				if err2 != nil {
					attempted = append(attempted, "sudo "+inst.name+" "+strings.Join(inst.args, " "))
					_ = out
					_ = out2
					continue
				}
			} else {
				_ = out
				continue
			}
		}
		if _, err := exec.LookPath("iptables"); err == nil {
			return nil
		}
		if _, err := exec.LookPath("ip6tables"); err == nil {
			return nil
		}
	}

	if len(attempted) == 0 {
		return fmt.Errorf("nft/iptables/ip6tables not found and no package manager available")
	}
	return fmt.Errorf("nft/iptables/ip6tables not found; install attempts failed: %s", strings.Join(attempted, " | "))
}

func (m *Manager) handleStartRealm(conn *ws.SafeConn, req StartRealmRequest) StartRealmResponse {
	// 已存在（含 Adopt）则直接返回，保证 start 幂等
	if p, ok := m.process.Get(req.RuleID, req.NodeID); ok && p != nil {
		if conn != nil {
			go m.process.startStatsLoop(conn, p, req.RuleID, req.NodeID)
		}
		return StartRealmResponse{
			Success:    true,
			Pid:        0,
			ConfigPath: p.ConfigPath,
			LogPath:    p.LogPath,
			Message:    "Realm process already running",
		}
	}

	// Agent 重启/异常场景：若同一 configPath 的 realm 仍在运行，先对比配置：
	// - 配置一致：直接 Adopt（不 flush 统计链，避免计数清零）
	// - 配置不一致：先 Stop 遗留进程，再按新配置启动（保证与主控一致）
	configPath, _ := buildPaths(req.RuleID, req.NodeID)
	if pids := findRealmPIDsByConfigPath(configPath); len(pids) > 0 {
		want := rewriteRealmListen(req.Config, req.Port)
		if b, err := os.ReadFile(configPath); err == nil && strings.ReplaceAll(string(b), "\r\n", "\n") == strings.ReplaceAll(want, "\r\n", "\n") {
			if p, adopted, err := m.process.AdoptExisting(req, conn); err != nil {
				return StartRealmResponse{Success: false, Message: err.Error()}
			} else if adopted && p != nil {
				pid := 0
				if len(pids) > 0 {
					pid = pids[0]
				}
				sendForwardStats(conn, req.RuleID, req.NodeID, req.Port, "healthy", 0, 0, 0, 0, nil, req.ActiveRelayNodeID, 0)
				m.health.RecordStatus(req.RuleID, req.NodeID, true, 0)
				return StartRealmResponse{
					Success:    true,
					Pid:        pid,
					ConfigPath: p.ConfigPath,
					LogPath:    p.LogPath,
					Message:    "Realm process adopted successfully",
				}
			}
		} else {
			timeout := stopDuration(req.StopTimeout)
			_ = m.process.Stop(req.RuleID, req.NodeID, timeout, req.Protocol, req.Port)
		}
	}

	// 防火墙放行
	if err := openPortByProtocol(m.firewall, req.Port, req.Protocol); err != nil {
		return StartRealmResponse{Success: false, Message: fmt.Sprintf("open port failed: %v", err)}
	}

	proc, err := m.process.Start(req, conn)
	if err != nil {
		return StartRealmResponse{Success: false, Message: err.Error()}
	}
	sendForwardStats(conn, req.RuleID, req.NodeID, req.Port, "healthy", 0, 0, 0, 0, nil, req.ActiveRelayNodeID, 0)
	m.health.RecordStatus(req.RuleID, req.NodeID, true, 0)

	// priority 策略：仅入口节点需要监控切换
	if req.EntryNodeID != "" && req.NodeID == req.EntryNodeID && len(req.PriorityConfigs) > 0 && len(req.PriorityRelays) > 0 {
		go m.switcher.MonitorAndSwitch(conn, req.RuleID, req.EntryNodeID, req.PriorityRelays, req.PriorityConfigs, req.ActiveRelayNodeID, req.Port, time.Duration(req.StatsInterval)*time.Second, req.Protocol, req.HealthCheckInterval, req.HealthCheckTarget)
	}
	return StartRealmResponse{
		Success:    true,
		Pid:        proc.Cmd.Process.Pid,
		ConfigPath: proc.ConfigPath,
		LogPath:    proc.LogPath,
		Message:    "Realm process started successfully",
	}
}

func (m *Manager) handleStopRealm(req StopRealmRequest) StopRealmResponse {
	if m.switcher != nil {
		m.switcher.Stop(req.RuleID)
	}
	timeout := 5 * time.Second
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	if err := m.process.Stop(req.RuleID, req.NodeID, timeout, req.Protocol, req.Port); err != nil {
		return StopRealmResponse{Success: false, Message: err.Error()}
	}
	// 关闭放行
	if req.Port > 0 {
		_ = closePortByProtocol(m.firewall, req.Port, req.Protocol)
	}
	return StopRealmResponse{Success: true, Message: "Realm process stopped successfully"}
}

func (m *Manager) handleUpdateRealm(conn *ws.SafeConn, req UpdateRealmRequest) UpdateRealmResponse {
	proc, err := m.process.Update(req, conn)
	if err != nil {
		return UpdateRealmResponse{Success: false, Message: err.Error()}
	}
	sendForwardStats(conn, req.RuleID, req.NodeID, req.NewPort, "healthy", 0, 0, 0, 0, nil, req.ActiveRelayNodeID, 0)
	m.health.RecordStatus(req.RuleID, req.NodeID, true, 0)
	if req.EntryNodeID != "" && req.NodeID == req.EntryNodeID && len(req.PriorityConfigs) > 0 && len(req.PriorityRelays) > 0 {
		if m.switcher != nil {
			m.switcher.Stop(req.RuleID)
			go m.switcher.MonitorAndSwitch(conn, req.RuleID, req.EntryNodeID, req.PriorityRelays, req.PriorityConfigs, req.ActiveRelayNodeID, req.NewPort, time.Duration(req.StatsInterval)*time.Second, req.Protocol, req.HealthCheckInterval, req.HealthCheckTarget)
		}
	}
	return UpdateRealmResponse{
		Success: true,
		Pid:     proc.Cmd.Process.Pid,
		Message: "Realm process updated successfully",
	}
}

func (m *Manager) handleGetRealmLog(req GetRealmLogRequest) GetRealmLogResponse {
	if req.Lines <= 0 {
		req.Lines = 100
	}
	_, logPath := buildPaths(req.RuleID, req.NodeID)
	content, err := readLastLines(logPath, req.Lines)
	if err != nil {
		return GetRealmLogResponse{Success: false, Message: err.Error()}
	}
	return GetRealmLogResponse{
		Success:       true,
		LogContent:    content,
		LinesReturned: req.Lines,
	}
}

func (m *Manager) handleClearRealmLog(req ClearRealmLogRequest) ClearRealmLogResponse {
	_, logPath := buildPaths(req.RuleID, req.NodeID)
	if err := os.Truncate(logPath, 0); err != nil {
		return ClearRealmLogResponse{Success: false, Message: err.Error()}
	}
	return ClearRealmLogResponse{Success: true, Message: "Log file cleared successfully"}
}

func (m *Manager) handleDeleteRealmLog(req DeleteRealmLogRequest) DeleteRealmLogResponse {
	_, logPath := buildPaths(req.RuleID, req.NodeID)
	matches, _ := filepath.Glob(logPath + "*")
	if len(matches) == 0 {
		return DeleteRealmLogResponse{Success: true, Message: "No log file to delete"}
	}
	for _, file := range matches {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return DeleteRealmLogResponse{Success: false, Message: err.Error()}
		}
	}
	return DeleteRealmLogResponse{Success: true, Message: "Log file deleted successfully"}
}

func (m *Manager) handleTestConnectivity(req TestConnectivityRequest) TestConnectivityResponse {
	address := net.JoinHostPort(req.TargetHost, strconv.Itoa(req.TargetPort))
	timeout := time.Duration(req.Timeout) * time.Second
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return TestConnectivityResponse{
			Success:   false,
			Reachable: false,
			Message:   err.Error(),
		}
	}
	_ = conn.Close()
	latency := time.Since(start).Milliseconds()
	return TestConnectivityResponse{
		Success:   true,
		Reachable: true,
		LatencyMs: &latency,
		Message:   "Target is reachable",
	}
}

func openPortByProtocol(firewall *FirewallManager, port int, protocol string) error {
	for _, proto := range normalizeProtocols(protocol) {
		if err := firewall.OpenPort(port, proto); err != nil {
			return err
		}
	}
	return nil
}

func closePortByProtocol(firewall *FirewallManager, port int, protocol string) error {
	for _, proto := range normalizeProtocols(protocol) {
		if err := firewall.ClosePort(port, proto); err != nil {
			return err
		}
	}
	return nil
}

func ensureRealmBinary(downloadURL string, force bool) (string, string, error) {
	candidates := []string{"/usr/local/bin/realm", "/usr/bin/realm"}

	if !force {
		for _, p := range candidates {
			if fileExists(p) {
				version := getRealmVersion(p)
				return p, version, nil
			}
		}
		if path, err := exec.LookPath("realm"); err == nil {
			version := getRealmVersion(path)
			return path, version, nil
		}
	}

	if downloadURL == "" {
		downloadURL = defaultRealmDownloadURL()
	}
	if downloadURL == "" {
		return "", "", fmt.Errorf("realm binary not found and no download url provided")
	}

	target := candidates[0]
	if err := downloadTo(downloadURL, target); err != nil {
		return "", "", err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return "", "", err
	}
	version := getRealmVersion(target)
	return target, version, nil
}

func mapRealmOS(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	case "linux":
		return "linux"
	default:
		return ""
	}
}

func mapRealmArch(goarch string) string {
	switch strings.ToLower(strings.TrimSpace(goarch)) {
	case "amd64":
		return "x86_64"
	case "386":
		return "i686"
	case "arm64":
		return "arm64"
	case "arm":
		return "armv7"
	default:
		return ""
	}
}

func defaultRealmDownloadURL() string {
	flags := pkg_flags.GlobalConfig
	endpoint := strings.TrimSuffix(strings.TrimSpace(flags.Endpoint), "/")
	token := strings.TrimSpace(flags.Token)
	if endpoint == "" || token == "" {
		return ""
	}
	osName := mapRealmOS(runtime.GOOS)
	arch := mapRealmArch(runtime.GOARCH)
	if osName == "" || arch == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	u.Path = "/api/v1/realm/binaries/download"
	q := u.Query()
	q.Set("token", token)
	q.Set("os", osName)
	q.Set("arch", arch)
	u.RawQuery = q.Encode()
	return u.String()
}

func downloadTo(url, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	flags := pkg_flags.GlobalConfig
	if flags.CFAccessClientID != "" && flags.CFAccessClientSecret != "" {
		req.Header.Set("CF-Access-Client-Id", flags.CFAccessClientID)
		req.Header.Set("CF-Access-Client-Secret", flags.CFAccessClientSecret)
	}

	client := dnsresolver.GetHTTPClient(60 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download realm failed: %s", resp.Status)
	}
	tmpPath := target + ".tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return err
	}
	return os.Rename(tmpPath, target)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func getRealmVersion(path string) string {
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readLastLines(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	lines := make([]string, 0, n)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}
