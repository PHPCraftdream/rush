package installwire

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// ModelControl keeps the install contract independent of the owner of the
// invocation and model input types.
type ModelControl[I, S, M any] interface {
	Run(context.Context, I, *LeafRegistry, S) (jobbatch.ID, error)
	Add(context.Context, string, string, string, I, *LeafRegistry, []M) ([]jobbatch.ID, error)
	StatusNode(context.Context, string, string, string) (any, error)
	Output(context.Context, string, string, string, uint64) (any, error)
	Stop(context.Context, string, string, string) (any, error)
	InstallBatchModelControls(*LeafRegistry, I) ([]fantasy.AgentTool, error)
}
