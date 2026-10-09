package connector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/publication"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/workflow"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestFailedSessionUsesSavedNotesAfterWorkflowChanges(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved custom template", true: "legacy session defaults"}[legacy], func(t *testing.T) {
			input := eligibleReviewInput()
			metadata, err := workflow.DecodeMetadata(recoveredMetadata(t, input))
			require.NoError(t, err)
			saved := review.NoteTemplates{Fail: "Saved failure message: {{ .Reason }}"}
			if !legacy {
				metadata.Notes = &saved
			}
			raw, err := json.Marshal(metadata)
			require.NoError(t, err)
			client := &orpheusMock{
				find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
					return []orpheus.Session{{ID: "accepted-session", RunID: "run", Status: "failed"}}, nil
				},
				metadata: func(_ context.Context, _, _, externalKey string) (json.RawMessage, error) {
					require.Equal(t, workflow.MessageExternalKey(input.ReviewFingerprint), externalKey)
					return raw, nil
				},
			}
			publisher := &reviewPublisherStub{}
			reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
				t.Fatal("existing session must not create a new contract")
				return workflow.SessionContract{}, nil
			}, ReconcilerConfig{Publisher: publisher, NotesForProject: func(int64) review.NoteTemplates {
				t.Fatal("accepted session must not use current workflow templates")
				return review.NoteTemplates{}
			}})
			require.NoError(t, reconciler.Reconcile(t.Context(), input))
			require.Equal(t, 1, publisher.errorCalls)
			require.Equal(t, "run_failed", publisher.failure.Code)
			if legacy {
				require.True(t, publisher.failure.Notes.Empty())
			} else {
				require.Equal(t, saved, publisher.failure.Notes)
			}
			require.Zero(t, client.createCalls)
		})
	}
}

func TestFailureNotesRetryMetadataLookupBeforePublication(t *testing.T) {
	input := eligibleReviewInput()
	metadata, err := workflow.DecodeMetadata(recoveredMetadata(t, input))
	require.NoError(t, err)
	metadata.Notes = &review.NoteTemplates{Fail: "Frozen failure note"}
	raw, err := json.Marshal(metadata)
	require.NoError(t, err)
	lookupErr := error(context.DeadlineExceeded)
	client := &orpheusMock{
		find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
			return []orpheus.Session{{ID: "accepted-session", RunID: "run", Status: "failed"}}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) { return raw, lookupErr },
	}
	publisher := &reviewPublisherStub{}
	reconciler := NewReconciler(zap.NewNop(), nil, client, testSessionContractBuilder, ReconcilerConfig{Publisher: publisher})
	require.True(t, IsRetryable(reconciler.Reconcile(t.Context(), input)))
	require.Zero(t, publisher.errorCalls)
	lookupErr = nil
	require.NoError(t, reconciler.Reconcile(t.Context(), input))
	require.Equal(t, 1, publisher.errorCalls)
	require.Equal(t, "Frozen failure note", publisher.failure.Notes.Fail)
	require.Zero(t, client.createCalls)
}

func TestBuildFailureUsesSelectedWorkflowFailureNote(t *testing.T) {
	input := eligibleReviewInput()
	publisher := &reviewPublisherStub{}
	notes := review.NoteTemplates{Fail: "Environment could not be prepared"}
	reconciler := NewReconciler(zap.NewNop(), nil, &orpheusMock{}, func(review.Input) (workflow.SessionContract, error) {
		return workflow.SessionContract{}, errors.New("private build error")
	}, ReconcilerConfig{Publisher: publisher, NotesForProject: func(projectID int64) review.NoteTemplates {
		require.Equal(t, input.MergeRequest.ProjectID, projectID)
		return notes
	}})
	require.NoError(t, reconciler.Reconcile(t.Context(), input))
	require.Equal(t, publication.Failure{Code: "invalid_session_contract", Notes: notes}, publisher.failure)
}

func testSessionContractBuilder(input review.Input) (workflow.SessionContract, error) {
	return testSessionContract(input), nil
}
