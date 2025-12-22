package forward

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

type nftStatsBase struct {
	Family      string
	Table       string
	InputChain  string
	OutputChain string
}

type nftChainDef struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
	Hook   string `json:"hook,omitempty"`
}

type nftRuleset struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

func detectNftStatsBase() (nftStatsBase, bool) {
	if _, err := exec.LookPath("nft"); err != nil {
		return nftStatsBase{}, false
	}
	out, err := exec.Command("nft", "-j", "list", "ruleset").CombinedOutput()
	if err != nil {
		return nftStatsBase{}, false
	}
	var rs nftRuleset
	if err := json.Unmarshal(out, &rs); err != nil {
		return nftStatsBase{}, false
	}

	type key struct {
		family string
		table  string
	}
	m := make(map[key]nftStatsBase)

	for _, item := range rs.Nftables {
		raw, ok := item["chain"]
		if !ok || len(raw) == 0 {
			continue
		}
		var c nftChainDef
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		hook := strings.ToLower(strings.TrimSpace(c.Hook))
		if hook != "input" && hook != "output" {
			continue
		}
		k := key{family: c.Family, table: c.Table}
		v := m[k]
		v.Family = c.Family
		v.Table = c.Table
		if hook == "input" {
			v.InputChain = c.Name
		} else {
			v.OutputChain = c.Name
		}
		m[k] = v
	}

	// 选择同时具有 input/output 的表；优先 inet，其次 ip，再其次 ip6；优先 table=filter。
	famRank := func(f string) int {
		switch f {
		case "inet":
			return 0
		case "ip":
			return 1
		case "ip6":
			return 2
		default:
			return 3
		}
	}
	bestOK := false
	var best nftStatsBase
	bestScore := 1 << 30
	for _, v := range m {
		if v.InputChain == "" || v.OutputChain == "" || v.Family == "" || v.Table == "" {
			continue
		}
		score := famRank(v.Family) * 10
		if v.Table != "filter" {
			score += 1
		}
		if !bestOK || score < bestScore {
			bestOK = true
			best = v
			bestScore = score
		}
	}
	return best, bestOK
}

func statsCommentChain(ruleID uint, port int, direction string) string {
	return fmt.Sprintf("komari_fwd_stats_%s:%d:%d", direction, ruleID, port)
}

func statsCommentJump(ruleID uint, port int, direction string, proto string, suffix string) string {
	if suffix != "" {
		return fmt.Sprintf("komari_fwd_jump_%s:%d:%d:%s:%s", direction, ruleID, port, strings.ToLower(proto), suffix)
	}
	return fmt.Sprintf("komari_fwd_jump_%s:%d:%d:%s", direction, ruleID, port, strings.ToLower(proto))
}

func setupNftStatsRules(base nftStatsBase, ruleID uint, port int, protocol string, outTargets []string) error {
	inChain, outChain := chainNames(ruleID, port)

	if err := ensureNftChain(base.Family, base.Table, inChain); err != nil {
		return err
	}
	if err := ensureNftChain(base.Family, base.Table, outChain); err != nil {
		return err
	}

	// chain 内只做计数并 RETURN，避免改变防火墙语义
	_ = exec.Command("nft", "flush", "chain", base.Family, base.Table, inChain).Run()
	_ = exec.Command("nft", "flush", "chain", base.Family, base.Table, outChain).Run()
	if err := runCmd("nft", "add", "rule", base.Family, base.Table, inChain, "counter", "return", "comment", statsCommentChain(ruleID, port, "in")); err != nil {
		return err
	}
	if err := runCmd("nft", "add", "rule", base.Family, base.Table, outChain, "counter", "return", "comment", statsCommentChain(ruleID, port, "out")); err != nil {
		return err
	}

	// base chain 插入 jump 规则（position 0），确保可计数但不改变最终 verdict
	for _, proto := range normalizeProtocols(protocol) {
		// INPUT：按 dport
		jcIn := statsCommentJump(ruleID, port, "in", proto, "")
		if !nftChainHasComment(base.Family, base.Table, base.InputChain, jcIn) {
			if err := runCmd("nft", "insert", "rule", base.Family, base.Table, base.InputChain, "index", "0", proto, "dport", strconv.Itoa(port), "jump", inChain, "comment", jcIn); err != nil {
				// 不阻塞：某些系统 input 链不存在/不可写，允许后续回退到 iptables
				return err
			}
		}

		// OUTPUT：优先按 remote/extra_remotes 的 daddr+dport；缺失则回退 sport=listenPort
		added := 0
		for _, dst := range uniqueTargets(outTargets) {
			host, dport, ok := splitHostPort(dst)
			if !ok {
				continue
			}
			host = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(host, "]"), "["))
			if i := strings.IndexByte(host, '%'); i > 0 {
				host = host[:i]
			}

			ip := net.ParseIP(host)
			if ip == nil {
				continue
			}
			isV4 := ip.To4() != nil
			if base.Family == "ip6" && isV4 {
				continue
			}
			if base.Family == "ip" && !isV4 {
				continue
			}

			suffix := host + ":" + strconv.Itoa(dport)
			jcOut := statsCommentJump(ruleID, port, "out", proto, suffix)
			if nftChainHasComment(base.Family, base.Table, base.OutputChain, jcOut) {
				continue
			}

			match := []string{}
			switch {
			case isV4 && (base.Family == "inet" || base.Family == "ip"):
				match = append(match, "ip", "daddr", host)
			case !isV4 && (base.Family == "inet" || base.Family == "ip6"):
				match = append(match, "ip6", "daddr", host)
			default:
				continue
			}
			match = append(match, proto, "dport", strconv.Itoa(dport))

			args := []string{"insert", "rule", base.Family, base.Table, base.OutputChain, "index", "0"}
			args = append(args, match...)
			args = append(args, "jump", outChain, "comment", jcOut)
			if err := runCmd("nft", args...); err != nil {
				return err
			}
			added++
		}
		if added == 0 {
			jcOut := statsCommentJump(ruleID, port, "out", proto, "sport")
			if !nftChainHasComment(base.Family, base.Table, base.OutputChain, jcOut) {
				if err := runCmd("nft", "insert", "rule", base.Family, base.Table, base.OutputChain, "index", "0", proto, "sport", strconv.Itoa(port), "jump", outChain, "comment", jcOut); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func cleanupNftStatsRules(base nftStatsBase, ruleID uint, port int, protocol string, outTargets []string) {
	inChain, outChain := chainNames(ruleID, port)
	for _, proto := range normalizeProtocols(protocol) {
		nftDeleteRulesByPrefix(base.Family, base.Table, base.InputChain, statsCommentJump(ruleID, port, "in", proto, ""))
		nftDeleteRulesByPrefix(base.Family, base.Table, base.OutputChain, statsCommentJump(ruleID, port, "out", proto, ""))
	}
	_ = exec.Command("nft", "flush", "chain", base.Family, base.Table, inChain).Run()
	_ = exec.Command("nft", "flush", "chain", base.Family, base.Table, outChain).Run()
	_ = exec.Command("nft", "delete", "chain", base.Family, base.Table, inChain).Run()
	_ = exec.Command("nft", "delete", "chain", base.Family, base.Table, outChain).Run()
}

func readNftPortCounters(base nftStatsBase, ruleID uint, port int) (in iptCounters, out iptCounters, err error) {
	inChain, outChain := chainNames(ruleID, port)
	in, err = readNftChainCounters(base.Family, base.Table, inChain)
	if err != nil {
		return
	}
	out, err = readNftChainCounters(base.Family, base.Table, outChain)
	return
}

func ensureNftChain(family, table, chain string) error {
	if err := exec.Command("nft", "list", "chain", family, table, chain).Run(); err == nil {
		return nil
	}
	// 不自动创建 base chain（hook），仅创建普通链；失败则返回错误让上层回退。
	return runCmd("nft", "add", "chain", family, table, chain)
}

func nftChainHasComment(family, table, chain, comment string) bool {
	out, err := exec.Command("nft", "-a", "list", "chain", family, table, chain).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), comment)
}

func nftDeleteRulesByPrefix(family, table, chain, commentPrefix string) {
	out, err := exec.Command("nft", "-a", "list", "chain", family, table, chain).CombinedOutput()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	handles := make([]int, 0, 8)
	for _, line := range lines {
		if !strings.Contains(line, commentPrefix) {
			continue
		}
		m := nftHandleRe.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		h, err := strconv.Atoi(m[1])
		if err != nil || h <= 0 {
			continue
		}
		handles = append(handles, h)
	}
	for _, h := range handles {
		_ = exec.Command("nft", "delete", "rule", family, table, chain, "handle", strconv.Itoa(h)).Run()
	}
}

type nftListChain struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

type nftRule struct {
	Expr []map[string]json.RawMessage `json:"expr"`
}

type nftCounter struct {
	Packets int64 `json:"packets"`
	Bytes   int64 `json:"bytes"`
}

func readNftChainCounters(family, table, chain string) (iptCounters, error) {
	out, err := exec.Command("nft", "-j", "list", "chain", family, table, chain).CombinedOutput()
	if err != nil {
		return iptCounters{}, err
	}
	var rs nftListChain
	if err := json.Unmarshal(out, &rs); err != nil {
		return iptCounters{}, err
	}
	var total iptCounters
	for _, item := range rs.Nftables {
		raw, ok := item["rule"]
		if !ok || len(raw) == 0 {
			continue
		}
		var r nftRule
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		for _, expr := range r.Expr {
			craw, ok := expr["counter"]
			if !ok || len(craw) == 0 {
				continue
			}
			var c nftCounter
			if err := json.Unmarshal(craw, &c); err != nil {
				continue
			}
			total.Pkts += c.Packets
			total.Bytes += c.Bytes
		}
	}
	return total, nil
}
