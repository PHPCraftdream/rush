package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

func validateScenario(scenario Scenario, receipts map[tasktree.RequestID]tasktree.Receipt, bound bool) error {
	if err := preflightScenario(scenario); err != nil {
		return err
	}
	if err := validateBinding(scenario.SchemaVersion, scenario.TreeKey, scenario.Limits); err != nil {
		return err
	}
	if len(scenario.Steps) > MaxScenarioSteps {
		return fmt.Errorf("lab scenario exceeds %d steps", MaxScenarioSteps)
	}
	if err := checkScenarioSize(scenario); err != nil {
		return err
	}
	for _, step := range scenario.Steps {
		if json.Valid(step.Payload) {
			if err := scanJSON(step.Payload); err != nil {
				return fmt.Errorf("step %q: payload: %w", step.Label, err)
			}
		}
	}
	labels := make(map[string]bool)
	requests := make(map[tasktree.RequestID]bool)
	for id := range receipts {
		requests[id] = true
	}
	for _, step := range scenario.Steps {
		if strings.TrimSpace(step.Label) == "" || labels[step.Label] {
			return fmt.Errorf("step label %q is blank or duplicated", step.Label)
		}
		labels[step.Label] = true
		if strings.TrimSpace(string(step.RequestID)) == "" {
			return fmt.Errorf("step %q: request_id must not be blank", step.Label)
		}
		if step.Expect.IsError == nil {
			return fmt.Errorf("step %q: expect.is_error is required", step.Label)
		}
		switch step.Mode {
		case "":
			if requests[step.RequestID] {
				return fmt.Errorf("step %q: duplicate request_id requires replay or reuse mode", step.Label)
			}
		case "replay", "reuse":
			if bound && !requests[step.RequestID] {
				return fmt.Errorf("step %q: %s requires a previous request_id", step.Label, step.Mode)
			}
		default:
			return fmt.Errorf("step %q: unknown mode %q", step.Label, step.Mode)
		}
		requests[step.RequestID] = true
	}
	return nil
}

func Run(ctx context.Context, scenario Scenario, loaded *Checkpoint) (Report, error) {
	var report Report
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := preflightScenario(scenario); err != nil {
		return report, err
	}
	var seeds map[tasktree.TreeKey]tasktree.Envelope
	var receipts map[tasktree.RequestID]tasktree.Receipt
	if loaded != nil {
		if err := validateCheckpoint(*loaded); err != nil {
			return report, err
		}
		if loaded.TreeKey != scenario.TreeKey || loaded.Limits != scenario.Limits {
			return report, fmt.Errorf("checkpoint tree_key and limits must match scenario")
		}
		seeds = map[tasktree.TreeKey]tasktree.Envelope{loaded.TreeKey: loaded.Envelope}
		receipts = loaded.Envelope.Receipts
	}
	if err := validateScenario(scenario, receipts, true); err != nil {
		return report, err
	}
	store, err := memory.NewStore(scenario.Limits, seeds)
	if err != nil {
		return report, err
	}
	api := protocol.New(tasktree.NewService(store, scenario.Limits))
	var budget reportBudget
	for _, step := range scenario.Steps {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		var before tasktree.Envelope
		if step.Mode != "" {
			before, err = store.Load(ctx, scenario.TreeKey)
			if err != nil {
				return report, fmt.Errorf("step %q: %w", step.Label, err)
			}
			if _, ok := before.Receipts[step.RequestID]; !ok {
				return report, fmt.Errorf("step %q: %s requires a durable mutation receipt", step.Label, step.Mode)
			}
		}
		result, err := api.Execute(ctx, protocol.Invocation{
			TreeKey: scenario.TreeKey, Actor: step.Actor, RequestID: step.RequestID,
		}, step.Payload)
		if err != nil {
			return report, fmt.Errorf("step %q: execute: %w", step.Label, err)
		}
		after, err := store.Load(ctx, scenario.TreeKey)
		if err != nil {
			return report, fmt.Errorf("step %q: load oracle: %w", step.Label, err)
		}
		if err := compareMode(step, result, before, after); err != nil {
			return report, fmt.Errorf("step %q: %w", step.Label, err)
		}
		if err := compareExpectation(step.Expect, result, after.Snapshot); err != nil {
			return report, fmt.Errorf("step %q: expectation: %w", step.Label, err)
		}
		if err := budget.retain(result); err != nil {
			return report, fmt.Errorf("step %q: %w", step.Label, err)
		}
		report.Steps = append(report.Steps, StepResult{Label: step.Label, Result: result})
	}
	envelope, err := store.Load(ctx, scenario.TreeKey)
	if err != nil {
		return report, err
	}
	report.Checkpoint = Checkpoint{SchemaVersion: SchemaVersion, TreeKey: scenario.TreeKey, Limits: scenario.Limits, Envelope: envelope}
	if err := validateCheckpoint(report.Checkpoint); err != nil {
		return report, err
	}
	return report, nil
}

func compareMode(step Step, result protocol.ToolResult, before, after tasktree.Envelope) error {
	if step.Mode == "" {
		if result.Details.Replayed {
			return fmt.Errorf("new request unexpectedly replayed")
		}
		return nil
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("%s changed the durable envelope", step.Mode)
	}
	if step.Mode == "reuse" {
		if !result.IsError || result.Details.Problem == nil || result.Details.Problem.Code != tasktree.CodeRequestReused {
			return fmt.Errorf("reuse must reject actor, canonical payload, or expected revision mismatch with request_reused")
		}
		return nil
	}
	receipt := before.Receipts[step.RequestID]
	if result.IsError || !result.Details.Replayed || result.Details.Receipt == nil || !reflect.DeepEqual(*result.Details.Receipt, receipt) {
		return fmt.Errorf("replay must match actor, canonical payload, and expected revision and return the original receipt")
	}
	return nil
}
