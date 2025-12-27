package forward

// SortHops returns a stable-ordered copy of hops by SortOrder (tie-break by original index).
// It is used to keep chain hop order consistent across planning/UI.
func SortHops(hops []ChainHop) []ChainHop {
	refs := sortedHopRefs(hops)
	out := make([]ChainHop, 0, len(refs))
	for _, r := range refs {
		out = append(out, *r.hop)
	}
	return out
}
