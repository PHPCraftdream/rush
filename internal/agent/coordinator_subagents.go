// Sub-agent delegation and skill discovery: bodies moved to
// subagents_component.go (the subagentsComponent host pattern); this file
// keeps the thin coordinator delegates and the concrete host adapter.
package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/safety/notify"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	core "github.com/PHPCraftdream/rush/internal/session/service"
)

func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	return newSubagentsComponent(c).runSubAgent(ctx, params)
}

func (c *coordinator) inheritBatchQuestionState(ctx context.Context, childSessionID string, call *SessionAgentCall) error {
	return newSubagentsComponent(c).inheritBatchQuestionState(ctx, childSessionID, call)
}

func (c *coordinator) agentFor(sessionID string) SessionAgent {
	return newSubagentsComponent(c).agentFor(sessionID)
}

func (c *coordinator) releaseDriverIfScopeClosed(childSessionID string) {
	newSubagentsComponent(c).releaseDriverIfScopeClosed(childSessionID)
}

func (c *coordinator) releaseDriverGeneration(childSessionID string, generation uint64) {
	newSubagentsComponent(c).releaseDriverGeneration(childSessionID, generation)
}

type coordinatorSubAgentHost struct{ coordinator *coordinator }

func newSubagentsComponent(c *coordinator) *subagentsComponent {
	return &subagentsComponent{host: coordinatorSubAgentHost{coordinator: c}}
}

func (h coordinatorSubAgentHost) readyWg() *readyGate { return &h.coordinator.readyWg }
func (h coordinatorSubAgentHost) sessions() core.Service {
	return h.coordinator.sessions
}
func (h coordinatorSubAgentHost) cfg() *config.ConfigStore { return h.coordinator.cfg }
func (h coordinatorSubAgentHost) permissions() permission.Service {
	return h.coordinator.permissions
}

func (h coordinatorSubAgentHost) notify() pubsub.Publisher[notify.Notification] {
	return h.coordinator.notify
}

func (h coordinatorSubAgentHost) subAgentDrivers() *subAgentDriverRegistry {
	return h.coordinator.subAgentDrivers
}

func (h coordinatorSubAgentHost) currentAgent() SessionAgent {
	return h.coordinator.currentAgent
}

func (h coordinatorSubAgentHost) resolveRequestedDelegationModel(ctx context.Context, cfg *config.Config, parentID string, role RequestedModelRole) (delegationRoleModel, error) {
	return h.coordinator.resolveRequestedDelegationModel(ctx, cfg, parentID, role)
}

func (h coordinatorSubAgentHost) batchHeldQuestionResumeHint(ctx context.Context, owner string) string {
	return h.coordinator.batchHeldQuestionResumeHint(ctx, owner)
}

func (h coordinatorSubAgentHost) batchHeldQuestionResumeRefusal(ctx context.Context, owner, child string) string {
	return h.coordinator.batchHeldQuestionResumeRefusal(ctx, owner, child)
}

func (h coordinatorSubAgentHost) resetConsecutiveResume(sessionID string) {
	h.coordinator.resetConsecutiveResume(sessionID)
}

func (h coordinatorSubAgentHost) buildModelsFromCfg(ctx context.Context, cfg *config.Config, smartModelCfg, fastModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	return h.coordinator.buildModelsFromCfg(ctx, cfg, smartModelCfg, fastModelCfg, isSubAgent)
}

func (h coordinatorSubAgentHost) buildCredentialModel(ctx context.Context, cfg *config.Config, creds *CredentialSet, choice ModelChoice) (Model, config.ProviderConfig, error) {
	return h.coordinator.buildCredentialModel(ctx, cfg, creds, choice)
}

func (h coordinatorSubAgentHost) resolveSubAgentModelOverride(ctx context.Context, sessionID string) (*Model, error) {
	return h.coordinator.resolveSubAgentModelOverride(ctx, sessionID)
}

func (h coordinatorSubAgentHost) workerSubAgentActiveForCall(ctx context.Context, cfg *config.Config) bool {
	return h.coordinator.workerSubAgentActiveForCall(ctx, cfg)
}

func (h coordinatorSubAgentHost) refreshSubAgentSlotModel(ctx context.Context, cfg *config.Config, role config.SelectedModelType, model Model) (Model, bool) {
	return h.coordinator.refreshSubAgentSlotModel(ctx, cfg, role, model)
}

func (h coordinatorSubAgentHost) rebuildInputs(providerID string) (*config.Config, config.ProviderConfig, error) {
	return h.coordinator.rebuildInputs(providerID)
}

func (h coordinatorSubAgentHost) rebuildPinnedModel(ctx context.Context, cfg *config.Config, model Model, providerCfg config.ProviderConfig, isSubAgent bool) (Model, error) {
	return h.coordinator.rebuildPinnedModel(ctx, cfg, model, providerCfg, isSubAgent)
}

func (h coordinatorSubAgentHost) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	return h.coordinator.refreshOAuth2Token(ctx, providerCfg)
}

func (h coordinatorSubAgentHost) runAwaitingAdmission(ctx context.Context, agent SessionAgent, call SessionAgentCall) (result *fantasy.AgentResult, err error, queued bool) {
	return h.coordinator.runAwaitingAdmission(ctx, agent, call)
}

func (h coordinatorSubAgentHost) runWithUnauthorizedRetry(ctx context.Context, providerCfg config.ProviderConfig, fn func() error, rebuildCall func() error) error {
	return h.coordinator.runWithUnauthorizedRetry(ctx, providerCfg, fn, rebuildCall)
}

func (h coordinatorSubAgentHost) isUnauthorized(err error) bool {
	return modelkit.IsUnauthorized(err)
}

func (h coordinatorSubAgentHost) childQuestionPreamble(childSessionID string) string {
	return h.coordinator.childQuestionPreamble(childSessionID)
}

func (h coordinatorSubAgentHost) rebuiltChildQuestionState(ctx context.Context, sessionID string) (*batchChildQuestionState, error) {
	return h.coordinator.rebuiltChildQuestionState(ctx, sessionID)
}
