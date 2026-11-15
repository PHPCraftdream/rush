package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func finalizationTestMessage() Message {
	return Message{ID: "candidate", SessionID: "owner", Role: Assistant, Parts: []ContentPart{TextContent{Text: "result"}}}
}

func TestFinalizationCandidateStaysPartialUntilAccepted(t *testing.T) {
	t.Parallel()
	candidate := finalizationTestMessage()
	id := FinalizationIdentity{Key: "logical-owner", Generation: 1, Revision: 3, Serial: 2}
	hash, err := candidate.FinalizationContentHash()
	require.NoError(t, err)
	require.NoError(t, candidate.MarkFinalizationCandidate(id))
	require.False(t, candidate.IsFinished())
	require.True(t, candidate.FinishPart().Partial)
	require.Equal(t, FinishReasonUnknown, candidate.FinishReason())
	accepted := candidate.Clone()
	require.NoError(t, accepted.AcceptFinalizationCandidate(id, hash))
	require.True(t, accepted.IsFinished())
	require.Equal(t, FinishReasonEndTurn, accepted.FinishReason())
	require.Equal(t, hash, accepted.FinishPart().FinalizationHash)
	require.False(t, candidate.IsFinished(), "accepting a clone cannot mutate a provisional snapshot")
	require.Empty(t, candidate.FinishPart().FinalizationHash)
	after, err := accepted.FinalizationContentHash()
	require.NoError(t, err)
	require.Equal(t, hash, after, "only footer state changes at acceptance")
	require.ErrorIs(t, accepted.AcceptFinalizationCandidate(id, hash), ErrFinalizationAlreadyAccepted)
}

func TestFinalizationCandidateMetadataSurvivesPartsRoundTrip(t *testing.T) {
	t.Parallel()
	candidate := finalizationTestMessage()
	id := FinalizationIdentity{Key: "logical-owner", Generation: 2, Revision: 7, Serial: 4}
	require.NoError(t, candidate.MarkFinalizationCandidate(id))
	hash, err := candidate.FinalizationContentHash()
	require.NoError(t, err)
	raw, err := marshalParts(candidate.Parts)
	require.NoError(t, err)
	parts, err := unmarshalParts(raw)
	require.NoError(t, err)
	candidate.Parts = parts
	require.False(t, candidate.IsFinished())
	require.NoError(t, candidate.AcceptFinalizationCandidate(id, hash))
	require.Equal(t, id.Key, candidate.FinishPart().FinalizationKey)
	require.Equal(t, id.Generation, candidate.FinishPart().FinalizationGeneration)
	require.Equal(t, id.Revision, candidate.FinishPart().FinalizationRevision)
	require.Equal(t, id.Serial, candidate.FinishPart().FinalizationSerial)
}

func TestFinalizationChangedIdentityOrBodyCannotBecomeFinal(t *testing.T) {
	t.Parallel()
	original := finalizationTestMessage()
	id := FinalizationIdentity{Key: "owner", Generation: 1, Revision: 2, Serial: 3}
	require.NoError(t, original.MarkFinalizationCandidate(id))
	hash, err := original.FinalizationContentHash()
	require.NoError(t, err)
	for _, wrong := range []FinalizationIdentity{
		{Key: "other", Generation: 1, Revision: 2, Serial: 3},
		{Key: "owner", Generation: 2, Revision: 2, Serial: 3},
		{Key: "owner", Generation: 1, Revision: 3, Serial: 3},
		{Key: "owner", Generation: 1, Revision: 2, Serial: 4},
	} {
		candidate := original.Clone()
		require.ErrorIs(t, candidate.AcceptFinalizationCandidate(wrong, hash), ErrFinalizationCandidateChanged)
		require.False(t, candidate.IsFinished())
	}
	changed := original.Clone()
	changed.Parts[0] = TextContent{Text: "replaced after generation"}
	require.ErrorIs(t, changed.AcceptFinalizationCandidate(id, hash), ErrFinalizationCandidateChanged)
	require.False(t, changed.IsFinished())
}

func TestFinalizationDoesNotAdoptOldCompletedMessages(t *testing.T) {
	t.Parallel()
	old := finalizationTestMessage()
	old.AddFinish(FinishReasonEndTurn, "", "")
	before, err := marshalParts(old.Parts)
	require.NoError(t, err)
	id := FinalizationIdentity{Key: "new-owner", Generation: 1, Serial: 1}
	require.ErrorIs(t, old.MarkFinalizationCandidate(id), ErrFinalizationCandidateChanged)
	after, err := marshalParts(old.Parts)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NotContains(t, string(after), "finalization_")
}

func TestFinalizationDoesNotNormalizeMessageText(t *testing.T) {
	t.Parallel()
	text := " \n{\"value\":1}\n"
	candidate := finalizationTestMessage()
	candidate.Parts = []ContentPart{TextContent{Text: text}}
	id := FinalizationIdentity{Key: "owner", Generation: 1, Serial: 1}
	require.NoError(t, candidate.MarkFinalizationCandidate(id))
	hash, err := candidate.FinalizationContentHash()
	require.NoError(t, err)
	require.NoError(t, candidate.AcceptFinalizationCandidate(id, hash))
	require.Equal(t, text, candidate.Content().Text)
}
