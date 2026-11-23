package agent

import (
	"context"
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

func WithAdmittedBatchRoleContracts(ctx context.Context) context.Context {
	return batchinst.WithAdmittedBatchRoleContracts(ctx)
}

func configForBatchInvocation(inv BatchInvocation) (*config.Config, error) {
	return batchinst.ConfigForBatchInvocation(inv.roleConfig)
}

func admitBatchRoleContracts(cfg *config.Config, inherited *session.RoleContract, spec jobbatch.Spec) (jobbatch.Spec, error) {
	return batchinst.AdmitRoleContracts(cfg, inherited, spec, batchinst.RoleAdmissionPolicy{
		ValidateBuiltin: func(cfg *config.Config, role RequestedModelRole) error {
			_, _, err := requestedRoleSlot(cfg, session.Session{}, role)
			return err
		},
		ResolveCustom: ResolveRoleContract,
	})
}

func validateBatchRolePayloads(spec jobbatch.Spec) error {
	return batchinst.ValidateRolePayloads(spec, func(raw json.RawMessage) (batchinst.RolePayload, error) {
		var p batchLeafPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return batchinst.RolePayload{}, err
		}
		out := batchinst.RolePayload{Tool: p.Tool, Input: p.Input, SelectedRoleContract: p.SelectedRoleContract}
		if p.Policy != nil {
			out.Parent = p.Policy.RoleContract
		}
		return out, nil
	})
}

func BatchRoleSnapshotHash(spec jobbatch.Spec) string { return batchinst.BatchRoleSnapshotHash(spec) }
