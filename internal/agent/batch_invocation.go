package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/permission"
)

// Delivery modes of one invocation. R11D owns the elections; the ingress only
// validates the closed set and carries the choice.
const (
	batchDeliverySync     = "sync"     // The caller waits for the real completion.
	batchDeliveryInline   = "inline"   // Durable completion or started history.
	batchDeliveryDetached = "detached" // Started now; notices selected later.
)

// Background-escape policies. Escape switches delivery of the existing root,
// never its execution (R11D).
const (
	batchEscapeNever = "never" // The leaf waits for its real Done.
	batchEscapeAuto  = "auto"  // The host may switch delivery to detached.
)

// Typed invocation rejections. All are sentinels distinguishable via errors.Is.
var (
	errBatchMissingSessionID    = errors.New("batch invocation: missing session id")
	errBatchMissingMessageID    = errors.New("batch invocation: missing message id")
	errBatchMissingCallID       = errors.New("batch invocation: missing tool call id")
	errBatchMissingRegistryID   = errors.New("batch invocation: missing pinned registry identity")
	errBatchMissingOrigin       = errors.New("batch invocation: missing origin")
	errBatchMissingPolicy       = errors.New("batch invocation: missing trusted policy context")
	errBatchBadDeliveryMode     = errors.New("batch invocation: unknown delivery mode")
	errBatchBadEscapePolicy     = errors.New("batch invocation: unknown background-escape policy")
	errBatchBadWaiterRank       = errors.New("batch invocation: waiter rank outside the closed profile ranks")
	errBatchBadDiskBinding      = errors.New("batch invocation: malformed disk binding id")
	errBatchNilLeaf             = errors.New("batch invocation: nil leaf tool")
	errBatchNilInput            = errors.New("batch invocation: nil tool input")
	errBatchInvocationMismatch  = errors.New("batch invocation: input hash mismatch for a live tool call")
	errBatchDeliveryNotWired    = errors.New("batch delivery seam is not wired")
	errBatchRunnerNotWired      = errors.New("batch runner handle is not wired")
	errBatchAttachNotWired      = errors.New("batch attach store is not wired")
	errBatchLeavesNotWired      = errors.New("batch pinned-leaf resolver is not wired")
	errBatchProfileNotWired     = errors.New("batch runtime profile is not wired")
	errBatchSubmitBeforeInvalid = errors.New("batch runtime: admission failed before the runner was called")
)

// batchLeafPolicyContext is the immutable executor-side policy transport: the
// trusted restricted-run and execution context of ONE invocation, carried in
// the task payload so concurrent calls with different folder scopes, disks or
// restricted toolsets never collide on a session-keyed lookup. It carries
// everything batch_exec_generic.go rebuilds from the caller context today
// (message id, image support, model name) plus the values it loses (the
// pinned registry identity, the folder scope, the restricted-run allowlist
// and the custom disk identity).
type batchLeafPolicyContext struct {
	RegistryID     string                       `json:"registry_id"`
	MessageID      string                       `json:"message_id,omitempty"`
	SupportsImages bool                         `json:"supports_images,omitempty"`
	ModelName      string                       `json:"model_name,omitempty"`
	FolderScope    *permission.FolderScopeSpec  `json:"folder_scope,omitempty"`
	RunAllowlist   *permission.RunAllowlistSpec `json:"run_allowlist,omitempty"`
	CustomDisk     string                       `json:"custom_disk,omitempty"`
	DiskProvider   bool                         `json:"disk_provider,omitempty"`
	// DiskBindingID is the REQUIRED process-bound provider binding
	// (disk_provider_binding.go) when this invocation carries a custom
	// DiskProvider; empty means no custom disk. Liveness is the
	// executor's job; the ID flows into the canonical spec payload hash.
	DiskBindingID     string `json:"disk_binding_id,omitempty"`
	RestrictedToolset string `json:"restricted_toolset"`
}

// batchDeliveryChoice is the mode/window pair R11D elects on.
type batchDeliveryChoice struct {
	Mode   string `json:"mode"`
	Window string `json:"window,omitempty"` // R11D: deadline or notice window.
}

// BatchInvocation is the immutable ingress metadata of ONE tool invocation.
// It is validated fail-closed before any engine or store call and never
// derived from session-keyed lookups: the pinned registry identity travels
// with the invocation.
type BatchInvocation struct {
	// SessionID is the owner session (the live-attach owner fence).
	SessionID string `json:"session_id"`
	// MessageID is the enclosing model message.
	MessageID string `json:"message_id"`
	// RegistryID is the pinned policy-leaf registry identity (R1), NOT a
	// session key: two calls of one session with different restricted
	// toolsets carry different registries.
	RegistryID string `json:"registry_id"`
	// Origin names the ingress caller family (cli_bridge, agentic_fetch,
	// coordinator) for audit.
	Origin string `json:"origin"`
	CallID string `json:"call_id,omitempty"`

	Delivery         batchDeliveryChoice `json:"delivery"`
	BackgroundEscape string              `json:"background_escape,omitempty"`

	// WaiterNode is the awaiting node of an explicit await, empty for a
	// direct call; WaiterRank is the caller's closed-profile rank.
	WaiterNode jobbatch.ID `json:"waiter_node,omitempty"`
	WaiterRank int         `json:"waiter_rank"`

	// Policy is the trusted restricted-run/policy context the executor
	// needs to rebuild its run context. Never read from session state.
	Policy batchLeafPolicyContext `json:"policy"`
	// WorkerStopScope is trusted tool-build policy; it is never model input.
	WorkerStopScope bool `json:"-"`
}

// batchCallIdentity returns the live-attach key of an invocation: the owner
// session and the tool call id (call.ID), exactly the pair R11B reserves.
func batchCallIdentity(inv BatchInvocation, call fantasy.ToolCall) (string, string) {
	return inv.SessionID, call.ID
}

// batchCanonicalInput compacts the model's raw tool input once. Two retries
// of one logical call carry byte-identical canonical inputs.
func batchCanonicalInput(input string) (json.RawMessage, error) {
	if strings.TrimSpace(input) == "" {
		return nil, fmt.Errorf("%w", errBatchNilInput)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(input)); err != nil {
		return nil, fmt.Errorf("batch invocation: canonicalise input: %w", err)
	}
	return json.RawMessage(compact.Bytes()), nil
}

// batchInvocationHash derives the live-attach input hash: the canonical SHA-256
// of the accepted one-item spec, mirroring the R11B commit hook's spec hash so
// a reserved root and its ingress key always agree.
func batchInvocationHash(spec jobbatch.Spec) string {
	raw, err := json.Marshal(spec)
	if err != nil {
		raw = []byte("unmarshalable")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Validate fails closed on any missing identity (session, message or call id,
// pinned registry identity, origin, trusted policy context) or unknown
// delivery/escape choice, and canonicalises every identity value once.
func (inv *BatchInvocation) Validate() error {
	inv.SessionID = strings.TrimSpace(inv.SessionID)
	inv.MessageID = strings.TrimSpace(inv.MessageID)
	inv.RegistryID = strings.TrimSpace(inv.RegistryID)
	inv.Origin = strings.TrimSpace(inv.Origin)
	inv.Policy.RegistryID = strings.TrimSpace(inv.Policy.RegistryID)
	inv.Policy.RestrictedToolset = strings.TrimSpace(inv.Policy.RestrictedToolset)
	inv.Policy.ModelName = strings.TrimSpace(inv.Policy.ModelName)
	inv.Policy.CustomDisk = strings.TrimSpace(inv.Policy.CustomDisk)
	inv.Policy.DiskBindingID = strings.TrimSpace(inv.Policy.DiskBindingID)
	// A non-empty binding ID is a required process binding: Validate checks
	// well-formedness only; liveness is resolved by the executor before any
	// leaf side effect (batch_exec.go).
	if inv.Policy.DiskBindingID != "" && !batchWellFormedBindingID(inv.Policy.DiskBindingID) {
		return fmt.Errorf("%w: binding id %q", errBatchBadDiskBinding, inv.Policy.DiskBindingID)
	}
	if inv.SessionID == "" {
		return fmt.Errorf("%w", errBatchMissingSessionID)
	}
	if inv.MessageID == "" {
		return fmt.Errorf("%w", errBatchMissingMessageID)
	}
	if inv.RegistryID == "" {
		return fmt.Errorf("%w", errBatchMissingRegistryID)
	}
	if inv.Origin == "" {
		return fmt.Errorf("%w", errBatchMissingOrigin)
	}
	if inv.Policy.RegistryID == "" || inv.Policy.RestrictedToolset == "" {
		return fmt.Errorf("%w", errBatchMissingPolicy)
	}
	switch inv.Delivery.Mode {
	case "":
		inv.Delivery.Mode = batchDeliverySync
	case batchDeliverySync, batchDeliveryInline, batchDeliveryDetached:
	default:
		return fmt.Errorf("%w: %q", errBatchBadDeliveryMode, inv.Delivery.Mode)
	}
	switch inv.BackgroundEscape {
	case "":
		inv.BackgroundEscape = batchEscapeNever
	case batchEscapeNever, batchEscapeAuto:
	default:
		return fmt.Errorf("%w: %q", errBatchBadEscapePolicy, inv.BackgroundEscape)
	}
	rankOK := false
	for _, rank := range batchGroupRanks {
		if inv.WaiterRank == rank {
			rankOK = true
		}
	}
	if !rankOK {
		return fmt.Errorf("%w: %d", errBatchBadWaiterRank, inv.WaiterRank)
	}
	return nil
}

// batchWellFormedBindingID checks binding-ID well-formedness only: opaque,
// bounded, whitespace-free. Liveness is the executor's job.
func batchWellFormedBindingID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == 0 {
			return false
		}
	}
	return true
}

// batchLeafRunContext rebuilds the executor run context from the invocation's
// immutable policy transport: every value batch_exec_generic.go copies today,
// disk provider closure pinned at caller tool-build time. Executor resolves
// and validates the binding; it never reconstructs these ordinary leaves.
func batchLeafRunContext(base context.Context, p batchLeafPayload) context.Context {
	ctx := context.WithValue(base, tools.SessionIDContextKey, p.SessionID)
	if p.Policy == nil {
		return ctx
	}
	if p.diskProvider != nil {
		ctx = context.WithValue(ctx, batchDiskProviderContextKey{}, p.diskProvider)
	}
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, p.Policy.MessageID)
	ctx = context.WithValue(ctx, tools.SupportsImagesContextKey, p.Policy.SupportsImages)
	ctx = context.WithValue(ctx, tools.ModelNameContextKey, p.Policy.ModelName)
	if p.Policy.FolderScope != nil {
		ctx = context.WithValue(ctx, batchFolderScopeContextKey{}, p.Policy.FolderScope)
	}
	if p.Policy.RunAllowlist != nil {
		ctx = context.WithValue(ctx, batchRunAllowlistContextKey{}, p.Policy.RunAllowlist)
	}
	if p.Policy.CustomDisk != "" {
		ctx = context.WithValue(ctx, batchCustomDiskContextKey{}, p.Policy.CustomDisk)
	}
	return ctx
}

type (
	batchFolderScopeContextKey  struct{}
	batchRunAllowlistContextKey struct{}
	batchCustomDiskContextKey   struct{}
	// batchDiskProviderContextKey carries the resolved process-bound
	// DiskProvider for a pinned leaf run.
	batchDiskProviderContextKey struct{}
)

// batchDiskProviderFrom returns the resolved binding provider carried by a
// pinned leaf run context, or nil (no binding required).
func batchDiskProviderFrom(ctx context.Context) tools.DiskProvider {
	p, _ := ctx.Value(batchDiskProviderContextKey{}).(tools.DiskProvider)
	return p
}
