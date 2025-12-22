package forward

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type FirewallTool string

const (
	FirewallUnknown   FirewallTool = "unknown"
	FirewallNone      FirewallTool = "none"
	FirewallUFW       FirewallTool = "ufw"
	FirewallFirewalld FirewallTool = "firewalld"
	FirewallIptables  FirewallTool = "iptables"
	FirewallNFT       FirewallTool = "nftables"
)

type FirewallManager struct {
	mu sync.Mutex

	Tool FirewallTool

	// firewalld 的端口开放是全局状态，避免关闭用户本来就打开的端口：仅关闭本进程打开过的。
	firewalldAdded map[string]struct{} // key: "port/proto"
	// 少数环境 comment 模块不可用时，iptables 只能退化为无注释规则：仅关闭本进程打开过的。
	legacyIptablesAdded map[string]struct{} // key: "port/proto"
}

type firewallState struct {
	FirewalldAdded []string `json:"firewalld_added"`
}

func firewallStatePath() string {
	return "/var/lib/komari-agent/forward/firewall_state.json"
}

func NewFirewallManager() *FirewallManager {
	m := &FirewallManager{
		Tool:                detectFirewallTool(),
		firewalldAdded:      make(map[string]struct{}),
		legacyIptablesAdded: make(map[string]struct{}),
	}
	m.loadState()
	return m
}

func detectFirewallTool() FirewallTool {
	// 1) firewalld（若运行中）：官方建议运行时不要直接用 iptables/nft 操作规则集
	if isFirewalldRunning() {
		return FirewallFirewalld
	}
	// 2) ufw（若 active）
	if isUFWActive() {
		return FirewallUFW
	}
	// 3) iptables（优先于 nft：很多发行版的 iptables 实际为 nft 后端封装，兼容性更强）
	if _, err := exec.LookPath("iptables"); err == nil {
		return FirewallIptables
	}
	if _, err := exec.LookPath("ip6tables"); err == nil {
		return FirewallIptables
	}
	// 4) nft（仅在没有 iptables 时才使用；关闭/去重需要更复杂的 handle 管理，这里仅做最小尝试）
	if _, err := exec.LookPath("nft"); err == nil {
		return FirewallNFT
	}
	return FirewallNone
}

func (f *FirewallManager) loadState() {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(firewallStatePath())
	if err != nil {
		return
	}
	var st firewallState
	if err := json.Unmarshal(b, &st); err != nil {
		return
	}
	for _, k := range st.FirewalldAdded {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		f.firewalldAdded[k] = struct{}{}
	}
}

func (f *FirewallManager) saveStateLocked() {
	st := firewallState{FirewalldAdded: make([]string, 0, len(f.firewalldAdded))}
	for k := range f.firewalldAdded {
		st.FirewalldAdded = append(st.FirewalldAdded, k)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	path := firewallStatePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func (f *FirewallManager) Detect() FirewallTool {
	if f.Tool == "" || f.Tool == FirewallUnknown {
		f.Tool = detectFirewallTool()
	}
	return f.Tool
}

func (f *FirewallManager) OpenPort(port int, protocol string) error {
	switch f.Detect() {
	case FirewallNone:
		return nil
	case FirewallFirewalld:
		return f.openPortFirewalld(port, protocol)
	case FirewallUFW:
		return f.openPortUFW(port, protocol)
	case FirewallNFT:
		return f.openPortNFT(port, protocol)
	case FirewallIptables:
		return f.openPortIptables(port, protocol)
	default:
		log.Printf("firewall tool unknown, skip open port %d/%s", port, protocol)
		return nil
	}
}

func (f *FirewallManager) ClosePort(port int, protocol string) error {
	switch f.Detect() {
	case FirewallNone:
		return nil
	case FirewallFirewalld:
		return f.closePortFirewalld(port, protocol)
	case FirewallUFW:
		return f.closePortUFW(port, protocol)
	case FirewallNFT:
		return f.closePortNFT(port, protocol)
	case FirewallIptables:
		return f.closePortIptables(port, protocol)
	default:
		log.Printf("firewall tool unknown, skip close port %d/%s", port, protocol)
		return nil
	}
}

func protoPort(protocol string, port int) string {
	proto := strings.ToLower(protocol)
	if proto == "udp" {
		return strconv.Itoa(port) + "/udp"
	}
	// default tcp
	return strconv.Itoa(port) + "/tcp"
}

func portProtoKey(port int, protocol string) string {
	return strconv.Itoa(port) + "/" + strings.ToLower(protocol)
}

func firewallComment(port int, protocol string) string {
	// 便于在 ufw status numbered 中精确定位
	return "komari-forward:" + strconv.Itoa(port) + ":" + strings.ToLower(protocol)
}

func isFirewalldRunning() bool {
	if _, err := exec.LookPath("firewall-cmd"); err != nil {
		return false
	}
	out, err := exec.Command("firewall-cmd", "--state").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(string(out))), "running")
}

func isUFWActive() bool {
	if _, err := exec.LookPath("ufw"); err != nil {
		return false
	}
	out, err := exec.Command("ufw", "status").CombinedOutput()
	if err != nil {
		return false
	}
	// 输出一般为：Status: active / inactive
	return strings.Contains(strings.ToLower(string(out)), "status: active")
}

func (f *FirewallManager) openPortFirewalld(port int, protocol string) error {
	key := portProtoKey(port, protocol)

	// firewalld 的 query 在未运行时会失败；此处 Detect 已判断 running。
	out, _ := exec.Command("firewall-cmd", "--query-port", key).CombinedOutput()
	if strings.TrimSpace(strings.ToLower(string(out))) == "yes" {
		return nil
	}
	if err := runCmd("firewall-cmd", "--add-port", key); err != nil {
		return err
	}
	f.mu.Lock()
	f.firewalldAdded[key] = struct{}{}
	f.saveStateLocked()
	f.mu.Unlock()
	return nil
}

func (f *FirewallManager) closePortFirewalld(port int, protocol string) error {
	key := portProtoKey(port, protocol)
	f.mu.Lock()
	_, ok := f.firewalldAdded[key]
	if ok {
		delete(f.firewalldAdded, key)
		f.saveStateLocked()
	}
	f.mu.Unlock()
	if !ok {
		return nil
	}
	_ = runCmd("firewall-cmd", "--remove-port", key)
	return nil
}

var ufwRuleNumRe = regexp.MustCompile(`\[\s*(\d+)\s*\]`)
var nftHandleRe = regexp.MustCompile(`\bhandle\s+(\d+)\b`)

func (f *FirewallManager) openPortUFW(port int, protocol string) error {
	// 即便 ufw 已经允许了该端口，我们也倾向不重复添加；但若无法准确判断，添加带 comment 的规则也不会影响关闭（只删自己的）。
	key := portProtoKey(port, protocol)
	comment := firewallComment(port, protocol)
	// 已存在我们自己的规则则直接返回
	if nums := ufwFindRuleNumbers(key, comment); len(nums) > 0 {
		return nil
	}
	return runCmd("ufw", "allow", key, "comment", comment)
}

func (f *FirewallManager) closePortUFW(port int, protocol string) error {
	key := portProtoKey(port, protocol)
	comment := firewallComment(port, protocol)
	nums := ufwFindRuleNumbers(key, comment)
	if len(nums) == 0 {
		return nil
	}
	// delete by number：从大到小删，避免编号变化
	for i := len(nums) - 1; i >= 0; i-- {
		_ = runCmd("ufw", "--force", "delete", strconv.Itoa(nums[i]))
	}
	return nil
}

func ufwFindRuleNumbers(portProto string, comment string) []int {
	out, err := exec.Command("ufw", "status", "numbered").CombinedOutput()
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	nums := make([]int, 0, 4)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, portProto) {
			continue
		}
		if comment != "" && !strings.Contains(line, comment) {
			continue
		}
		m := ufwRuleNumRe.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		nums = append(nums, n)
	}
	return nums
}

func (f *FirewallManager) openPortNFT(port int, protocol string) error {
	// nftables 删除需要 handle，因此统一以 comment 标识并用 `nft -a` 获取 handle。
	// 仅操作既有的 inet filter input，不在这里自动创建 base chain，避免改变防火墙语义。
	comment := firewallComment(port, protocol)
	family, table, chain, chainOut, ok := nftFindInputChain()
	if !ok {
		// 系统未启用 nft filter input 链：视为无需放行
		return nil
	}
	if strings.Contains(chainOut, comment) {
		return nil
	}
	return runCmd("nft", "add", "rule", family, table, chain, strings.ToLower(protocol), "dport", strconv.Itoa(port), "accept", "comment", comment)
}

func (f *FirewallManager) closePortNFT(port int, protocol string) error {
	comment := firewallComment(port, protocol)
	family, table, chain, chainOut, ok := nftFindInputChain()
	if !ok {
		return nil
	}
	lines := strings.Split(chainOut, "\n")
	handles := make([]int, 0, 4)
	for _, line := range lines {
		if !strings.Contains(line, comment) {
			continue
		}
		m := nftHandleRe.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		handles = append(handles, n)
	}
	for _, h := range handles {
		_ = runCmd("nft", "delete", "rule", family, table, chain, "handle", strconv.Itoa(h))
	}
	return nil
}

func nftFindInputChain() (family, table, chain, output string, ok bool) {
	// 常见组合：inet/filter/input（多数发行版）
	candidates := [][3]string{
		{"inet", "filter", "input"},
		{"ip", "filter", "input"},
		{"ip6", "filter", "input"},
	}
	for _, c := range candidates {
		out, err := exec.Command("nft", "-a", "list", "chain", c[0], c[1], c[2]).CombinedOutput()
		if err != nil {
			continue
		}
		return c[0], c[1], c[2], string(out), true
	}
	return "", "", "", "", false
}

func (f *FirewallManager) openPortIptables(port int, protocol string) error {
	proto := strings.ToLower(protocol)
	key := portProtoKey(port, protocol)
	comment := firewallComment(port, protocol)

	if _, err := exec.LookPath("iptables"); err == nil {
		// 若系统本就已放行该端口，则不重复添加。
		if exec.Command("iptables", "-C", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT").Run() != nil {
			// 优先带 comment，便于仅删除我们加的规则
			if exec.Command("iptables", "-C", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-m", "comment", "--comment", comment, "-j", "ACCEPT").Run() != nil {
				if err := runCmd("iptables", "-I", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-m", "comment", "--comment", comment, "-j", "ACCEPT"); err != nil {
					// comment 模块不可用：退化为无注释规则，并仅在本进程内记录以便关闭
					if err2 := runCmd("iptables", "-I", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT"); err2 != nil {
						return err
					}
					f.mu.Lock()
					f.legacyIptablesAdded[key] = struct{}{}
					f.mu.Unlock()
				}
			}
		}
	}
	if _, err := exec.LookPath("ip6tables"); err == nil {
		if exec.Command("ip6tables", "-C", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT").Run() != nil {
			if exec.Command("ip6tables", "-C", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-m", "comment", "--comment", comment, "-j", "ACCEPT").Run() != nil {
				if err := runCmd("ip6tables", "-I", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-m", "comment", "--comment", comment, "-j", "ACCEPT"); err != nil {
					if err2 := runCmd("ip6tables", "-I", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT"); err2 != nil {
						return err
					}
					f.mu.Lock()
					f.legacyIptablesAdded[key] = struct{}{}
					f.mu.Unlock()
				}
			}
		}
	}
	return nil
}

func (f *FirewallManager) closePortIptables(port int, protocol string) error {
	proto := strings.ToLower(protocol)
	key := portProtoKey(port, protocol)
	comment := firewallComment(port, protocol)

	if _, err := exec.LookPath("iptables"); err == nil {
		_ = runCmd("iptables", "-D", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-m", "comment", "--comment", comment, "-j", "ACCEPT")
	}
	if _, err := exec.LookPath("ip6tables"); err == nil {
		_ = runCmd("ip6tables", "-D", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-m", "comment", "--comment", comment, "-j", "ACCEPT")
	}

	f.mu.Lock()
	_, ok := f.legacyIptablesAdded[key]
	if ok {
		delete(f.legacyIptablesAdded, key)
	}
	f.mu.Unlock()
	if ok {
		if _, err := exec.LookPath("iptables"); err == nil {
			_ = runCmd("iptables", "-D", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT")
		}
		if _, err := exec.LookPath("ip6tables"); err == nil {
			_ = runCmd("ip6tables", "-D", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", "ACCEPT")
		}
	}
	return nil
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("firewall command failed: %s %v -> %v, output: %s", name, args, err, string(out))
		return err
	}
	return nil
}
