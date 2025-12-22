package forward

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// 链命名：尽量短且稳定，避免触发 iptables 链名长度限制（不同版本/后端限制不完全一致）。
func chainNames(ruleID uint, port int) (string, string) {
	rid := uint32(ruleID)
	p := uint16(port)
	return fmt.Sprintf("KF_%08x_%04x_IN", rid, p), fmt.Sprintf("KF_%08x_%04x_OUT", rid, p)
}

func setupIptablesRules(ruleID uint, port int, protocol string, outTargets []string) error {
	inChain, outChain := chainNames(ruleID, port)

	cmds := availableIptablesCmds()
	if len(cmds) == 0 {
		return fmt.Errorf("iptables/ip6tables not found")
	}
	for _, cmd := range cmds {
		// 创建链（若已存在忽略）
		_ = exec.Command(cmd, "-N", inChain).Run()
		_ = exec.Command(cmd, "-N", outChain).Run()
		// 仅做计数：链内使用 RETURN，避免改变原有防火墙语义（特别是默认 DROP 的机器）
		_ = exec.Command(cmd, "-F", inChain).Run()
		_ = exec.Command(cmd, "-F", outChain).Run()
		_ = exec.Command(cmd, "-A", inChain, "-j", "RETURN").Run()
		_ = exec.Command(cmd, "-A", outChain, "-j", "RETURN").Run()
	}

	// 在 INPUT/OUTPUT 插入跳转规则（若不存在）
	for _, proto := range normalizeProtocols(protocol) {
		// INPUT：同时兼容 IPv4/IPv6
		for _, cmd := range cmds {
			addJumpIfMissingCmd(cmd, []string{"INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", inChain})
		}

		// OUTPUT：realm 的出站连接通常使用临时源端口，直接按 --sport listenPort 可能统计不准。
		// 优先按配置中的 remote/extra_remotes 做 OUTPUT 统计；若缺失则回退到旧规则。
		added := 0
		if len(outTargets) > 0 {
			for _, dst := range uniqueTargets(outTargets) {
				host, dport, ok := splitHostPort(dst)
				if !ok {
					continue
				}
				for _, cmd := range iptablesCmdsForHost(host, cmds) {
					added++
					addJumpIfMissingCmd(cmd, []string{"OUTPUT", "-p", proto, "-d", host, "--dport", strconv.Itoa(dport), "-j", outChain})
				}
			}
		}
		if added == 0 {
			for _, cmd := range cmds {
				addJumpIfMissingCmd(cmd, []string{"OUTPUT", "-p", proto, "--sport", strconv.Itoa(port), "-j", outChain})
			}
		}
	}
	return nil
}

func cleanupIptablesRules(ruleID uint, port int, protocol string, outTargets []string) {
	inChain, outChain := chainNames(ruleID, port)
	cmds := availableIptablesCmds()
	// 删除跳转规则
	for _, proto := range normalizeProtocols(protocol) {
		for _, cmd := range cmds {
			_ = exec.Command(cmd, "-D", "INPUT", "-p", proto, "--dport", strconv.Itoa(port), "-j", inChain).Run()
		}
		if len(outTargets) > 0 {
			for _, dst := range uniqueTargets(outTargets) {
				host, dport, ok := splitHostPort(dst)
				if !ok {
					continue
				}
				for _, cmd := range iptablesCmdsForHost(host, cmds) {
					_ = exec.Command(cmd, "-D", "OUTPUT", "-p", proto, "-d", host, "--dport", strconv.Itoa(dport), "-j", outChain).Run()
				}
			}
		}
		// 兼容清理旧规则（即便当前使用新规则）
		for _, cmd := range cmds {
			_ = exec.Command(cmd, "-D", "OUTPUT", "-p", proto, "--sport", strconv.Itoa(port), "-j", outChain).Run()
		}
	}
	// 清空并删除链
	for _, cmd := range cmds {
		_ = exec.Command(cmd, "-F", inChain).Run()
		_ = exec.Command(cmd, "-X", inChain).Run()
		_ = exec.Command(cmd, "-F", outChain).Run()
		_ = exec.Command(cmd, "-X", outChain).Run()
	}
}

func availableIptablesCmds() []string {
	out := make([]string, 0, 2)
	if _, err := exec.LookPath("iptables"); err == nil {
		out = append(out, "iptables")
	}
	if _, err := exec.LookPath("ip6tables"); err == nil {
		out = append(out, "ip6tables")
	}
	return out
}

func iptablesCmdsForHost(host string, available []string) []string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if i := strings.IndexByte(host, '%'); i > 0 {
		host = host[:i]
	}

	ip := net.ParseIP(host)
	if ip != nil {
		if ip.To4() != nil {
			return filterCmds(available, "iptables")
		}
		return filterCmds(available, "ip6tables")
	}
	// hostname：无法确定，尽量同时统计 IPv4/IPv6（可用哪个就用哪个）
	return available
}

func filterCmds(available []string, want string) []string {
	out := make([]string, 0, 1)
	for _, v := range available {
		if v == want {
			out = append(out, v)
		}
	}
	return out
}

func addJumpIfMissingCmd(cmd string, args []string) {
	// 检查是否存在
	check := append([]string{"-C"}, args...)
	if err := exec.Command(cmd, check...).Run(); err == nil {
		return
	}
	_ = exec.Command(cmd, append([]string{"-I"}, args...)...).Run()
}

type iptCounters struct {
	Pkts  int64
	Bytes int64
}

func readChainCountersCmd(cmd string, chain string) (iptCounters, error) {
	c := exec.Command(cmd, "-nvxL", chain)
	out, err := c.Output()
	if err != nil {
		return iptCounters{}, err
	}
	var pkts, totalBytes int64
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Chain") || strings.HasPrefix(line, "pkts") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		p, _ := strconv.ParseInt(fields[0], 10, 64)
		b, _ := strconv.ParseInt(fields[1], 10, 64)
		pkts += p
		totalBytes += b
	}
	if err := scanner.Err(); err != nil {
		return iptCounters{}, err
	}
	return iptCounters{Pkts: pkts, Bytes: totalBytes}, nil
}

func readChainCountersMulti(chain string) (iptCounters, error) {
	cmds := availableIptablesCmds()
	if len(cmds) == 0 {
		return iptCounters{}, fmt.Errorf("iptables/ip6tables not found")
	}
	var sum iptCounters
	var lastErr error
	ok := 0
	for _, cmd := range cmds {
		v, err := readChainCountersCmd(cmd, chain)
		if err != nil {
			lastErr = err
			continue
		}
		ok++
		sum.Pkts += v.Pkts
		sum.Bytes += v.Bytes
	}
	if ok == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no counters available")
		}
		return iptCounters{}, lastErr
	}
	return sum, nil
}

// ReadPortCounters 返回入口/出口字节数
func ReadIptablesPortCounters(ruleID uint, port int) (in iptCounters, out iptCounters, err error) {
	inChain, outChain := chainNames(ruleID, port)
	in, err = readChainCountersMulti(inChain)
	if err != nil {
		return
	}
	out, err = readChainCountersMulti(outChain)
	return
}

func uniqueTargets(targets []string) []string {
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func splitHostPort(addr string) (string, int, bool) {
	addr = strings.TrimSpace(strings.Trim(addr, "\""))
	if addr == "" {
		return "", 0, false
	}
	if host, portStr, err := net.SplitHostPort(addr); err == nil {
		p, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil || p <= 0 {
			return "", 0, false
		}
		return strings.TrimSpace(host), p, true
	}

	// 兜底：兼容少量不标准写法（不支持 IPv6 无 [] 字面量）
	idx := strings.LastIndex(addr, ":")
	if idx <= 0 || idx >= len(addr)-1 {
		return "", 0, false
	}
	host := strings.TrimSpace(addr[:idx])
	p, err := strconv.Atoi(strings.TrimSpace(addr[idx+1:]))
	if err != nil || p <= 0 {
		return "", 0, false
	}
	return host, p, true
}
