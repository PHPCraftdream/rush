package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestHandleInterruptTick_DurableReplacementCopiesActivePolicy(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "interrupt-policy-copy")
	require.NoError(t, err)

	model := &Model{ModelCfg: config.SelectedModel{
		Provider:        "tenant-provider",
		Model:           "tenant-model",
		ReasoningEffort: "high",
		MaxTokens:       321,
	}}
	fastModel := &Model{ModelCfg: config.SelectedModel{
		Provider: "tenant-provider",
		Model:    "tenant-fast",
	}}
	runSpec := &permission.RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"fs_read"},
		AllowBash:  []string{"git status"},
	}
	runAllowlist, err := permission.BuildRunAllowlist(*runSpec)
	require.NoError(t, err)
	folderSpec := &permission.FolderScopeSpec{
		WorkingDir: t.TempDir(),
		Entries: []permission.FolderScopeEntry{{
			Dir: ".",
			Ops: []permission.FileOp{permission.FileOpRead},
		}},
		KeepCommandTools: true,
	}
	folderScope, err := permission.BuildFolderScope(*folderSpec)
	require.NoError(t, err)
	active := SessionAgentCall{
		SessionID:   sess.ID,
		Prompt:      "active B",
		Attachments: []message.Attachment{{FileName: "stale.txt"}},
		ProviderOptions: fantasy.ProviderOptions{
			"openai": &openai.ProviderOptions{},
		},
		MaxOutputTokens:      777,
		NonInteractive:       true,
		SystemPromptOverride: "B prompt",
		MaxCost:              12.5,
		MaxTokens:            987,
		SmartModel:           model,
		FastModel:            fastModel,
		SystemPromptPrefix:   stringPtr("B prefix"),
		SystemPrompt:         stringPtr("B base"),
		Tools:                []fantasy.AgentTool{},
		RunAllowlist:         &runAllowlist,
		RunAllowlistSpec:     runSpec,
		FolderScopeSpec:      folderSpec,
		CallOptions: &CallOptions{
			ModelRole:                config.SelectedModelTypeFast,
			TimeoutOptionsSet:        true,
			TimeoutExtendsOnProgress: false,
			TimeoutHardCap:           19,
			DisableSubAgents:         true,
			FailIfSessionBusy:        true,
			FolderScope:              &folderScope,
		},
		Origin: message.OriginSDK,
	}

	current := newMockAgent("operator-provider", 4096, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("ok"), nil
	})
	current.activeCall = active
	current.hasActiveCall = true
	coord := &coordinator{sessions: env.sessions, messages: env.messages, currentAgent: current}

	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt B"}},
	})
	require.NoError(t, err)
	const injectID = "interrupt-policy-copy-inject"
	require.NoError(t, env.sessions.CreatePendingInject(t.Context(), session.PendingInject{
		ID: injectID, SessionID: sess.ID, MessageID: msg.ID, Content: msg.FullText(), Interrupt: true,
	}))

	ctx := WithCallOptions(t.Context(), &CallOptions{AllowPeakHours: true})
	fired, err := coord.handleInterruptTick(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, fired)
	require.Len(t, current.interruptAndReplaced, 1)
	replacement := current.interruptAndReplaced[0]

	require.Equal(t, msg.FullText(), replacement.Prompt)
	require.Nil(t, replacement.Attachments)
	require.Equal(t, msg.ID, replacement.ExistingMessageID)
	require.Equal(t, injectID, replacement.InjectID)
	require.NotEmpty(t, replacement.LogicalCallID)
	require.True(t, replacement.FromDurableQueue)
	require.Nil(t, replacement.OnUserMessageCreated)
	require.Equal(t, active.ProviderOptions, replacement.ProviderOptions)
	require.Equal(t, active.MaxOutputTokens, replacement.MaxOutputTokens)
	require.Equal(t, active.MaxCost, replacement.MaxCost)
	require.Equal(t, active.MaxTokens, replacement.MaxTokens)
	require.Equal(t, active.NonInteractive, replacement.NonInteractive)
	require.Equal(t, active.SystemPromptOverride, replacement.SystemPromptOverride)
	require.Same(t, active.SmartModel, replacement.SmartModel)
	require.Same(t, active.FastModel, replacement.FastModel)
	require.Equal(t, active.SystemPromptPrefix, replacement.SystemPromptPrefix)
	require.Equal(t, active.SystemPrompt, replacement.SystemPrompt)
	require.Equal(t, active.Tools, replacement.Tools)
	require.Same(t, active.RunAllowlist, replacement.RunAllowlist)
	require.Equal(t, active.RunAllowlistSpec, replacement.RunAllowlistSpec)
	require.Equal(t, active.FolderScopeSpec, replacement.FolderScopeSpec)
	require.Same(t, active.CallOptions, replacement.CallOptions)
	require.Equal(t, active.Origin, replacement.Origin)

	entries, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	var data session.SessionAgentCallData
	require.NoError(t, json.Unmarshal([]byte(entries[0].CallData), &data))
	require.Equal(t, replacement.LogicalCallID, data.LogicalCallID)
	require.Equal(t, replacement.Prompt, data.Prompt)
	require.Equal(t, replacement.RunAllowlistSpec.AllowTools, data.RunAllowlistSpec.AllowTools)
	require.Equal(t, replacement.RunAllowlistSpec.AllowBash, data.RunAllowlistSpec.AllowBash)
	require.Equal(t, replacement.FolderScopeSpec.WorkingDir, data.FolderScopeSpec.WorkingDir)
	require.Len(t, data.FolderScopeSpec.Entries, len(replacement.FolderScopeSpec.Entries))
	for i, entry := range replacement.FolderScopeSpec.Entries {
		require.Equal(t, entry.Dir, data.FolderScopeSpec.Entries[i].Dir)
		require.Equal(t, []string{string(entry.Ops[0])}, []string{string(data.FolderScopeSpec.Entries[i].Ops[0])})
	}
	require.Equal(t, string(replacement.CallOptions.ModelRole), data.CallOptionsSpec.ModelRole)
	require.True(t, data.CallOptionsSpec.DisableSubAgents)
	require.True(t, data.CallOptionsSpec.TimeoutOptionsSet)
	require.Equal(t, replacement.MaxCost, data.MaxCost)
	require.Equal(t, replacement.MaxTokens, data.MaxTokens)
	require.Equal(t, replacement.SmartModel.ModelCfg.Model, data.SmartModel.Model)
	require.Equal(t, replacement.FastModel.ModelCfg.Model, data.FastModel.Model)

	rebuilt, err := FromSessionAgentCallData(data)
	require.NoError(t, err)
	require.Equal(t, replacement.RunAllowlistSpec, rebuilt.RunAllowlistSpec)
	require.Equal(t, replacement.FolderScopeSpec, rebuilt.FolderScopeSpec)
	require.Equal(t, replacement.CallOptions.ModelRole, rebuilt.CallOptions.ModelRole)
	require.True(t, rebuilt.CallOptions.DisableSubAgents)
	require.Equal(t, replacement.CallOptions.TimeoutHardCap, rebuilt.CallOptions.TimeoutHardCap)
}

func TestHandleInterruptTick_ReconcileLostGenerationKeepsNewerQueueRow(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "interrupt-policy-reconcile")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	mb := sa.getMailbox(sess.ID)
	a := SessionAgentCall{SessionID: sess.ID, Prompt: "A"}
	b := SessionAgentCall{
		SessionID:        sess.ID,
		Prompt:           "B",
		RunAllowlistSpec: &permission.RunAllowlistSpec{Restrict: true, AllowBash: []string{"git status"}},
		CallOptions:      &CallOptions{ModelRole: config.SelectedModelTypeFast, DisableSubAgents: true},
	}
	_, epoch := mb.submit(a, func() {})
	require.NotZero(t, epoch)
	mb.setCurrentCall(a)
	mb.beginGeneration(func() {})

	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "interrupt"}},
	})
	require.NoError(t, err)
	const injectID = "interrupt-policy-reconcile-inject"
	require.NoError(t, env.sessions.CreatePendingInject(t.Context(), session.PendingInject{
		ID: injectID, SessionID: sess.ID, MessageID: msg.ID, Content: msg.FullText(), Interrupt: true,
	}))

	sessions := &interruptEnqueueBarrierSessions{
		Service: env.sessions,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	coord := &coordinator{sessions: sessions, messages: env.messages, currentAgent: sa}
	result := make(chan struct {
		fired bool
		err   error
	}, 1)
	go func() {
		fired, tickErr := coord.handleInterruptTick(t.Context(), sess.ID)
		result <- struct {
			fired bool
			err   error
		}{fired, tickErr}
	}()
	<-sessions.entered

	mb.setCurrentCall(b)
	bCanceled := false
	mb.beginGeneration(func() { bCanceled = true })
	require.NoError(t, env.sessions.EnqueueRunQueueEntry(t.Context(), "newer-generation-row", sess.ID, []byte(`{"prompt":"newer"}`)))
	close(sessions.release)

	out := <-result
	require.NoError(t, out.err)
	require.False(t, out.fired)
	require.False(t, bCanceled)

	pending, err := env.sessions.PeekInterruptInject(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, injectID, pending.ID)
	candidateA, err := env.sessions.GetRunQueueEntry(t.Context(), sessions.candidateKey)
	require.NoError(t, err)
	require.Nil(t, candidateA, "candidate A must be absent before an idempotent stale cleanup")
	require.NoError(t, env.sessions.ReconcileInterruptInjectEnqueue(
		t.Context(), *pending, sessions.candidateKey,
	))
	entries, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "newer-generation-row", entries[0].ID)
}

func TestInterruptReconcileDoesNotDeleteLaterAttemptForSameInject(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "interrupt-policy-aba")
	require.NoError(t, err)
	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "source"}},
	})
	require.NoError(t, err)
	inject := session.PendingInject{
		ID:        "interrupt-policy-aba-inject",
		SessionID: sess.ID,
		MessageID: msg.ID,
		Content:   "source",
		Interrupt: true,
		CreatedAt: 1234,
	}
	require.NoError(t, env.sessions.CreatePendingInject(t.Context(), inject))

	const (
		candidateA = "candidate-attempt-a"
		candidateB = "candidate-attempt-b"
	)
	callDataA := []byte(`{"logical_call_id":"A","inject_id":"interrupt-policy-aba-inject"}`)
	callDataB := []byte(`{"logical_call_id":"B","inject_id":"interrupt-policy-aba-inject"}`)
	consumed, err := env.sessions.ConsumeInterruptInjectAndEnqueue(
		t.Context(), sess.ID, inject.ID, candidateA, callDataA,
	)
	require.NoError(t, err)
	require.Equal(t, inject, *consumed)

	require.NoError(t, env.sessions.ReconcileInterruptInjectEnqueue(t.Context(), *consumed, candidateA))
	candidate, err := env.sessions.GetRunQueueEntry(t.Context(), candidateA)
	require.NoError(t, err)
	require.Nil(t, candidate)
	restored, err := env.sessions.PeekInterruptInject(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, &inject, restored)

	consumed, err = env.sessions.ConsumeInterruptInjectAndEnqueue(
		t.Context(), sess.ID, inject.ID, candidateB, callDataB,
	)
	require.NoError(t, err)
	require.Equal(t, inject, *consumed)
	queuedB, err := env.sessions.GetRunQueueEntry(t.Context(), candidateB)
	require.NoError(t, err)
	require.NotNil(t, queuedB)
	require.Equal(t, string(callDataB), queuedB.CallData)

	// A delayed retry for attempt A must be a no-op. It must not match B just
	// because both candidates originated from the same pending inject.
	require.NoError(t, env.sessions.ReconcileInterruptInjectEnqueue(t.Context(), inject, candidateA))
	queuedBAfter, err := env.sessions.GetRunQueueEntry(t.Context(), candidateB)
	require.NoError(t, err)
	require.NotNil(t, queuedBAfter)
	require.Equal(t, string(callDataB), queuedBAfter.CallData)
	remaining, err := env.sessions.PeekInterruptInject(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Nil(t, remaining, "stale attempt A must not restore the source after attempt B consumed it")
	entries, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, candidateB, entries[0].ID)
}

type interruptEnqueueBarrierSessions struct {
	session.Service
	entered      chan struct{}
	release      chan struct{}
	candidateKey string
}

func (s *interruptEnqueueBarrierSessions) ConsumeInterruptInjectAndEnqueue(ctx context.Context, sessionID, injectID, idempotencyKey string, callData []byte) (*session.PendingInject, error) {
	pi, err := s.Service.ConsumeInterruptInjectAndEnqueue(ctx, sessionID, injectID, idempotencyKey, callData)
	if err != nil || pi == nil {
		return pi, err
	}
	s.candidateKey = idempotencyKey
	close(s.entered)
	<-s.release
	return pi, nil
}

func stringPtr(value string) *string { return &value }
