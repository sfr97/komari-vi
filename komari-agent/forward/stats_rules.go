package forward

import (
	"log"
	"os/exec"
)

type StatsMethod string

const (
	StatsMethodUnknown  StatsMethod = "unknown"
	StatsMethodIptables StatsMethod = "iptables"
	StatsMethodNFTables StatsMethod = "nftables"
)

// StatsInfo 描述某条规则在本机的流量统计方式与必要上下文。
// 说明：统计方式与端口放行（FirewallManager）独立。
type StatsInfo struct {
	Method StatsMethod

	// nftables 上下文（仅 Method=nftables 时有效）
	NftFamily          string
	NftTable           string
	NftInputBaseChain  string
	NftOutputBaseChain string
}

func setupStatsRules(ruleID uint, port int, protocol string, outTargets []string) (StatsInfo, error) {
	// 优先 nftables（若系统已启用 nft 且存在 input/output hook 链），否则使用 iptables。
	if ctx, ok := detectNftStatsBase(); ok {
		info := StatsInfo{
			Method:             StatsMethodNFTables,
			NftFamily:          ctx.Family,
			NftTable:           ctx.Table,
			NftInputBaseChain:  ctx.InputChain,
			NftOutputBaseChain: ctx.OutputChain,
		}
		if err := setupNftStatsRules(ctx, ruleID, port, protocol, outTargets); err == nil {
			return info, nil
		} else {
			// nft 失败则回退 iptables（保底），不影响 realm 启动。
			log.Printf("setup nft stats failed, fallback to iptables: %v", err)
		}
	}
	return StatsInfo{Method: StatsMethodIptables}, setupIptablesRules(ruleID, port, protocol, outTargets)
}

func detectExistingStatsInfo(ruleID uint, port int) StatsInfo {
	inChain, _ := chainNames(ruleID, port)

	// 优先判断是否存在 nft 的统计链（存在则用 nft 读数，避免误选导致全 0）。
	if ctx, ok := detectNftStatsBase(); ok {
		if err := exec.Command("nft", "list", "chain", ctx.Family, ctx.Table, inChain).Run(); err == nil {
			return StatsInfo{
				Method:             StatsMethodNFTables,
				NftFamily:          ctx.Family,
				NftTable:           ctx.Table,
				NftInputBaseChain:  ctx.InputChain,
				NftOutputBaseChain: ctx.OutputChain,
			}
		}
	}

	// 兜底：iptables（只要链存在即可读 counters）
	for _, cmd := range availableIptablesCmds() {
		if err := exec.Command(cmd, "-nvxL", inChain).Run(); err == nil {
			return StatsInfo{Method: StatsMethodIptables}
		}
	}

	// 若都判断不出来，则按当前环境优先级选择（nft base 可用则 nft，否则 iptables）。
	if ctx, ok := detectNftStatsBase(); ok {
		return StatsInfo{
			Method:             StatsMethodNFTables,
			NftFamily:          ctx.Family,
			NftTable:           ctx.Table,
			NftInputBaseChain:  ctx.InputChain,
			NftOutputBaseChain: ctx.OutputChain,
		}
	}
	return StatsInfo{Method: StatsMethodIptables}
}

func cleanupStatsRules(info StatsInfo, ruleID uint, port int, protocol string, outTargets []string) {
	switch info.Method {
	case StatsMethodNFTables:
		cleanupNftStatsRules(nftStatsBase{
			Family:      info.NftFamily,
			Table:       info.NftTable,
			InputChain:  info.NftInputBaseChain,
			OutputChain: info.NftOutputBaseChain,
		}, ruleID, port, protocol, outTargets)
	default:
		cleanupIptablesRules(ruleID, port, protocol, outTargets)
	}
}

func ReadPortCounters(info StatsInfo, ruleID uint, port int, protocol string) (in iptCounters, out iptCounters, err error) {
	switch info.Method {
	case StatsMethodNFTables:
		return readNftPortCounters(nftStatsBase{
			Family:      info.NftFamily,
			Table:       info.NftTable,
			InputChain:  info.NftInputBaseChain,
			OutputChain: info.NftOutputBaseChain,
		}, ruleID, port)
	default:
		return ReadIptablesPortCounters(ruleID, port)
	}
}
