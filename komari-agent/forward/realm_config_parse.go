package forward

import (
	"net"
	"regexp"
	"strconv"
	"strings"
)

var (
	realmRemoteRe = regexp.MustCompile(`(?m)^\s*remote\s*=\s*"(.*?)"\s*(?:#.*)?$`)
	realmListenRe = regexp.MustCompile(`(?m)^\s*listen\s*=\s*"(.*?)"\s*(?:#.*)?$`)
	realmExtraRe  = regexp.MustCompile(`(?ms)^\s*extra_remotes\s*=\s*\[(.*?)\]\s*(?:#.*)?$`)
	realmQuotedRe = regexp.MustCompile(`"([^"]+)"`)
)

// parseRealmOutTargets 从 realm TOML 文本中提取出站目标（remote + extra_remotes）。
// 仅做轻量提取用于 iptables 统计，不做完整 TOML 语义解析。
func parseRealmOutTargets(config string) []string {
	config = strings.ReplaceAll(config, "\r\n", "\n")
	if strings.TrimSpace(config) == "" {
		return nil
	}

	out := make([]string, 0, 8)

	for _, m := range realmRemoteRe.FindAllStringSubmatch(config, -1) {
		if len(m) < 2 {
			continue
		}
		v := strings.TrimSpace(m[1])
		if v != "" {
			out = append(out, v)
		}
	}

	for _, m := range realmExtraRe.FindAllStringSubmatch(config, -1) {
		if len(m) < 2 {
			continue
		}
		body := m[1]
		for _, qm := range realmQuotedRe.FindAllStringSubmatch(body, -1) {
			if len(qm) < 2 {
				continue
			}
			v := strings.TrimSpace(qm[1])
			if v != "" {
				out = append(out, v)
			}
		}
	}

	return uniqueTargets(out)
}

func rewriteRealmListen(config string, port int) string {
	if port <= 0 {
		return config
	}
	config = strings.ReplaceAll(config, "\r\n", "\n")
	host := chooseListenHost()
	want := net.JoinHostPort(host, strconv.Itoa(port))
	return realmListenRe.ReplaceAllString(config, "listen = \""+want+"\"")
}

func parseRealmListenPort(config string) (int, bool) {
	config = strings.ReplaceAll(config, "\r\n", "\n")
	m := realmListenRe.FindStringSubmatch(config)
	if len(m) < 2 {
		return 0, false
	}
	addr := strings.TrimSpace(m[1])
	if addr == "" {
		return 0, false
	}
	host, portStr, err := net.SplitHostPort(strings.Trim(addr, `"`))
	_ = host
	if err != nil {
		// 兼容 listen=":1234" 这种情况
		if strings.HasPrefix(addr, ":") {
			portStr = strings.TrimPrefix(addr, ":")
		} else {
			return 0, false
		}
	}
	p, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || p <= 0 {
		return 0, false
	}
	return p, true
}
