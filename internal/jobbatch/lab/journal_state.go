package lab

import (
	"sort"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Unfinished identities come from the captured Engine prefix, descendants
// before their root, including nodes accepted by later Add requests.
func journalUnfinished(state journalDerived, inputs []JournalInput) []jobbatch.ID {
	ids := []jobbatch.ID{}
	var visit func(jobbatch.ID)
	visit = func(parent jobbatch.ID) {
		children := []jobbatch.NodeView{}
		for _, v := range state.nodes {
			if v.Parent == parent {
				children = append(children, v)
			}
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Ordinal < children[j].Ordinal })
		for _, v := range children {
			visit(v.ID)
			if !v.State.Terminal() {
				ids = append(ids, v.ID)
			}
		}
	}
	for _, in := range inputs {
		if in.Action != ActionSubmit || in.Result != InputAccepted || in.IDs == nil || len(*in.IDs) != 1 {
			continue
		}
		root := (*in.IDs)[0]
		visit(root)
		if v, ok := state.nodes[root]; ok && !v.State.Terminal() {
			ids = append(ids, root)
		}
	}
	return ids
}
