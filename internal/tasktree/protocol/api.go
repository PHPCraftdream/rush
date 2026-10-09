package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

type API struct{ backend Backend }

func New(backend Backend) *API { return &API{backend: backend} }

func infrastructure(err error) bool {
	var storage *tasktree.InfrastructureError
	var uncertain *tasktree.CommitUnknownError
	return errors.As(err, &storage) || errors.As(err, &uncertain)
}

func (a *API) problem(ctx context.Context, invocation Invocation, err error) (ToolResult, error) {
	if infrastructure(err) {
		return ToolResult{}, err
	}
	var p *tasktree.Problem
	if !errors.As(err, &p) {
		return ToolResult{}, err
	}
	summary, readErr := a.backend.Summary(ctx, invocation.TreeKey)
	if readErr != nil {
		return ToolResult{}, &tasktree.InfrastructureError{Operation: "protocol summary", Err: readErr}
	}
	return ToolResult{IsError: true, Details: Details{Summary: summary, Problem: p}, Text: renderProblem(summary, p)}, nil
}

func (a *API) Execute(ctx context.Context, invocation Invocation, payload json.RawMessage) (ToolResult, error) {
	if a == nil || a.backend == nil {
		return ToolResult{}, fmt.Errorf("task protocol requires a backend")
	}
	invalid := func(message string) (ToolResult, error) {
		return a.problem(ctx, invocation, &tasktree.Problem{Code: tasktree.CodeInvalidInput, Message: message})
	}
	if strings.TrimSpace(string(invocation.TreeKey)) == "" || strings.TrimSpace(invocation.Actor.ID) == "" {
		return invalid("trusted tree key and actor ID must be nonblank; host must repair the binding")
	}
	if invocation.Actor.Kind != tasktree.ActorAgent && invocation.Actor.Kind != tasktree.ActorOperator {
		return invalid("trusted actor kind must be agent or operator; host must repair the binding")
	}
	command, expected, err := decode(payload)
	if err != nil {
		return invalid(err.Error())
	}
	if command.Op == tasktree.OpView {
		view, err := a.backend.View(ctx, invocation.TreeKey, command.Target)
		if err != nil {
			return a.problem(ctx, invocation, err)
		}
		return ToolResult{Details: Details{Summary: view.Summary, View: &view}, Text: renderView(view)}, nil
	}
	if strings.TrimSpace(string(invocation.RequestID)) == "" {
		return invalid("trusted mutation request ID must be nonblank; host must supply a stable invocation ID")
	}
	reply, err := a.backend.Mutate(ctx, invocation.TreeKey, invocation.Actor, invocation.RequestID, expected, command)
	if err != nil {
		return a.problem(ctx, invocation, err)
	}
	details := Details{Summary: reply.Summary, Receipt: &reply.Receipt, Replayed: reply.Replayed, Created: reply.Created}
	return ToolResult{Details: details, Text: renderMutation(reply)}, nil
}
