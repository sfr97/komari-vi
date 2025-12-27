package forward

import "sort"

// SortRelays returns a stable-ordered copy of relays by SortOrder.
// Tie-break: node_id, then original index (to keep duplicates stable).
func SortRelays(relays []RelayNode) []RelayNode {
	type relayRef struct {
		idx   int
		relay *RelayNode
	}
	refs := make([]relayRef, 0, len(relays))
	for i := range relays {
		refs = append(refs, relayRef{idx: i, relay: &relays[i]})
	}
	sort.Slice(refs, func(i, j int) bool {
		a := refs[i].relay
		b := refs[j].relay
		if a.SortOrder == b.SortOrder {
			if a.NodeID == b.NodeID {
				return refs[i].idx < refs[j].idx
			}
			return a.NodeID < b.NodeID
		}
		return a.SortOrder < b.SortOrder
	})
	out := make([]RelayNode, 0, len(refs))
	for _, r := range refs {
		out = append(out, *r.relay)
	}
	return out
}

