package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/google/uuid"
)

// BatchCLITask is the untrusted external task form. Payloads, registry IDs,
// sessions and tool-call identities are deliberately not accepted from JSON.
type BatchCLITask struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

type batchCLIBinding struct {
	registry *policyLeafRegistry
	id       string
	session  string
	message  string
	images   bool
	model    string
	allow    *permission.RunAllowlistSpec
	scope    *permission.FolderScopeSpec
}

// BatchCLIBuilder binds trusted constructors to a concrete coordinator.
type BatchCLIBuilder struct {
	coord   *coordinator
	ingress BatchWorkIngress
}

// NewBatchCLIBuilder requires the installed ingress and the concrete Rush
// coordinator, ensuring trees execute through its pinned policy tools.
func NewBatchCLIBuilder(c Coordinator, in BatchWorkIngress) (*BatchCLIBuilder, error) {
	cc, ok := c.(*coordinator)
	if !ok || in == nil {
		return nil, errors.New("batch CLI: installed coordinator and runtime are required")
	}
	return &BatchCLIBuilder{coord: cc, ingress: in}, nil
}

func (b *BatchCLIBuilder) bind(ctx context.Context, sessionID, messageID string) (batchCLIBinding, error) {
	if b == nil || b.coord == nil || b.ingress == nil || sessionID == "" || messageID == "" {
		return batchCLIBinding{}, errors.New("batch CLI: missing installed runtime or owner identity")
	}
	cfg := b.coord.cfg.Config()
	coder, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return batchCLIBinding{}, errors.New("batch CLI: coder tool configuration unavailable")
	}
	// The normal top-level CLI path applies the same call options before
	// building coder tools; scoped options fail closed when not representable.
	if scope := folderScopeSpecFrom(ctx); scope != nil {
		ctx = WithFolderScopeSpec(ctx, scope)
	}
	built, err := b.coord.buildPolicyTools(ctx, cfg, coder, false)
	if err != nil {
		return batchCLIBinding{}, err
	}
	id, err := b.ingress.PinLeafRegistry((*BatchLeafRegistry)(built.leaves))
	if err != nil {
		return batchCLIBinding{}, err
	}
	var allow *permission.RunAllowlistSpec
	if v := runAllowlistSpecFrom(ctx); v != nil {
		copy := *v
		allow = &copy
	}
	var scopeSpec *permission.FolderScopeSpec
	if v := folderScopeSpecFrom(ctx); v != nil {
		copy := *v
		scopeSpec = &copy
	}
	return batchCLIBinding{
		registry: built.leaves, id: id, session: sessionID, message: messageID,
		allow: allow, scope: scopeSpec,
	}, nil
}

func (b *BatchCLIBuilder) translate(ctx context.Context, bind batchCLIBinding, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if spec.Task != nil {
		var task BatchCLITask
		if err := json.Unmarshal(spec.Task.Payload, &task); err != nil {
			return jobbatch.Spec{}, fmt.Errorf("batch CLI task payload must be {tool,input}: %w", err)
		}
		if task.Tool == "" || len(task.Input) == 0 || !json.Valid(task.Input) {
			return jobbatch.Spec{}, errors.New("batch CLI task requires tool and valid input")
		}
		if batchControlTarget(task.Tool) || !batchWorkTarget(task.Tool) {
			return jobbatch.Spec{}, fmt.Errorf("batch CLI: tool %q is unavailable or control-plane; use a supported work tool", task.Tool)
		}
		leaf, err := bind.registry.resolve(task.Tool)
		if err != nil {
			return jobbatch.Spec{}, fmt.Errorf("batch CLI: tool %q is not in configured pinned policy toolset: %w", task.Tool, err)
		}
		// Resolve the pinned decorated tool solely to ensure it exists. The
		// executor runs its policy-preserving leaf with the same run context.
		_ = leaf
		payload, err := json.Marshal(batchLeafPayload{
			Tool: task.Tool, ToolCallID: uuid.NewString(), SessionID: bind.session, Input: task.Input, RegistryID: bind.id,
			Policy: &batchLeafPolicyContext{RegistryID: bind.id, MessageID: bind.message, SupportsImages: bind.images, ModelName: bind.model, FolderScope: bind.scope, RunAllowlist: bind.allow, RestrictedToolset: bind.id},
		})
		if err != nil {
			return jobbatch.Spec{}, err
		}
		group, _, err := classifyBatchLeaf(batchLeafKind(task.Tool), task.Tool, batchMCPCapability{})
		if err != nil {
			return jobbatch.Spec{}, err
		}
		spec.Task.Kind = batchLeafKind(task.Tool)
		spec.Task.Group = group
		spec.Task.Payload = payload
		return spec, nil
	}
	if spec.Batch == nil {
		return jobbatch.Spec{}, errors.New("batch CLI: every item must be exactly a task or batch")
	}
	for i := range spec.Batch.Items {
		item, err := b.translate(ctx, bind, spec.Batch.Items[i])
		if err != nil {
			return jobbatch.Spec{}, err
		}
		spec.Batch.Items[i] = item
	}
	return spec, nil
}

// TranslateSpec turns an external structural tree into the engine tree with
// trusted session/message and pinned policy identity supplied by Rush.
func (b *BatchCLIBuilder) TranslateSpec(ctx context.Context, sessionID, messageID string, spec jobbatch.Spec) (jobbatch.Spec, error) {
	bind, err := b.bind(ctx, sessionID, messageID)
	if err != nil {
		return jobbatch.Spec{}, err
	}
	if spec.Batch == nil || spec.Task != nil {
		return jobbatch.Spec{}, errors.New("batch CLI run requires a batch root")
	}
	return b.translate(ctx, bind, spec)
}

// TranslateItems applies an already-pinned identity to Add children.
func (b *BatchCLIBuilder) TranslateItems(ctx context.Context, sessionID, messageID string, items []jobbatch.Spec) ([]jobbatch.Spec, error) {
	bind, err := b.bind(ctx, sessionID, messageID)
	if err != nil {
		return nil, err
	}
	out := make([]jobbatch.Spec, len(items))
	for i := range items {
		out[i], err = b.translate(ctx, bind, items[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
