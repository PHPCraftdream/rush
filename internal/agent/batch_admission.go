package agent

import (
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Temporary integration bridges for callers still owned by package agent.
func batchAdmissionPolicy() batchinst.AdmissionPolicy {
	return batchinst.AdmissionPolicy{
		ControlTarget: batchControlTarget, DecodeTool: batchAdmissionTool,
		IOGroup: batchGroupIO, AgentGroup: batchGroupAgentDriver,
		IORank: batchGroupRanks[batchGroupIO], AgentRank: batchGroupRanks[batchGroupAgentDriver],
	}
}

func batchAdmissionTool(raw json.RawMessage) (string, error) {
	var payload batchLeafPayload
	err := json.Unmarshal(raw, &payload)
	return payload.Tool, err
}

func batchAdmissionProfile(profile *BatchProfile) batchinst.AdmissionProfile {
	if profile == nil || profile.caps == nil {
		return batchinst.AdmissionProfile{}
	}
	return batchinst.AdmissionProfile{GroupCap: profile.GroupCap, GroupRank: profile.GroupRank}
}

func ValidateSubmit(profile *BatchProfile, waiter batchinst.RankContext, edges []batchinst.DependencyEdge, spec jobbatch.Spec, maxDepth, maxNodes int, resolver batchinst.MCPCapabilityResolver) error {
	return batchinst.ValidateSubmit(batchAdmissionProfile(profile), waiter, edges, spec, maxDepth, maxNodes, resolver, batchAdmissionPolicy())
}

func ValidateAdd(profile *BatchProfile, waiter batchinst.RankContext, edges []batchinst.DependencyEdge, spec jobbatch.Spec, existingDepth, existingNodes, maxDepth, maxNodes int, resolver batchinst.MCPCapabilityResolver) error {
	return batchinst.ValidateAdd(batchAdmissionProfile(profile), waiter, edges, spec, existingDepth, existingNodes, maxDepth, maxNodes, resolver, batchAdmissionPolicy())
}

func validateBatch(profile *BatchProfile, waiter batchinst.RankContext, edges []batchinst.DependencyEdge, specs []jobbatch.Spec, depthBudget, nodeBudget int, resolver batchinst.MCPCapabilityResolver) error {
	return batchinst.ValidateBatch(batchAdmissionProfile(profile), waiter, edges, specs, depthBudget, nodeBudget, resolver, batchAdmissionPolicy())
}
