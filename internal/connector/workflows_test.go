package connector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/publication"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/workflow"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestReconcilerSkipsUnmatchedProjectsWithoutBuildingOrCreatingSession(t *testing.T) {
	t.Parallel()
	input := eligibleReviewInput()
	publisher := &reviewPublisherStub{}
	client := &orpheusMock{}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("an unmatched project must not build an agent environment")
		return workflow.SessionContract{}, nil
	}, ReconcilerConfig{Publisher: publisher, MatchesWorkflow: func(projectID int64) bool { require.Equal(t, input.MergeRequest.ProjectID, projectID); return false }})
	require.NoError(t, reconciler.Reconcile(t.Context(), input))
	require.Equal(t, 1, publisher.skippedCalls)
	require.Equal(t, input, publisher.input)
	require.Zero(t, publisher.calls)
	require.Zero(t, publisher.errorCalls)
	require.Zero(t, client.createCalls)
	publisher.err = &publication.Error{Code: "note_marker_missing", Retryable: true, Err: context.DeadlineExceeded}
	require.True(t, IsRetryable(reconciler.Reconcile(t.Context(), input)))
	require.Zero(t, client.createCalls)
	publisher.err, publisher.finalized = nil, true
	require.NoError(t, reconciler.Reconcile(t.Context(), input))
	require.Equal(t, 2, publisher.skippedCalls, "terminal skip is finalized without re-publishing")
}

func TestReconcilerRecoversActiveSessionWithoutSelectingCurrentWorkflow(t *testing.T) {
	t.Parallel()
	input := eligibleReviewInput()
	client := &orpheusMock{
		active: func(_ context.Context, namespace string) ([]orpheus.Session, error) {
			require.Equal(t, workflow.Namespace, namespace)
			return []orpheus.Session{{ID: "accepted", RunID: "run", MRKey: input.MRKey, InputFingerprint: input.ReviewFingerprint, Status: "running"}}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) {
			return recoveredMetadata(t, input), nil
		},
	}
	publisher := &reviewPublisherStub{}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("recovery must not build a contract from current configuration")
		return workflow.SessionContract{}, nil
	}, ReconcilerConfig{Publisher: publisher, MatchesWorkflow: func(int64) bool { t.Fatal("recovery precedes routing"); return false }})
	require.NoError(t, reconciler.reconcileSnapshot(t.Context(), reviewSnapshot(input)))
	require.Equal(t, 1, client.metadataCalls)
	require.Zero(t, client.findCalls)
	require.Zero(t, client.createCalls)
	require.Zero(t, client.cancelCalls)
	require.Zero(t, publisher.skippedCalls)
}

func TestMultipleWorkflowsShareAdmissionLimitAndNamespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "special.md"), []byte("---\nid: special\nproject_ids: [74]\nprofile: special-profile\nsandbox_template: special-template\nservices: [gitlab, redmine]\nmodel: special-model\nlanguage: ru\n---\nSpecial project rules\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "default.md"), []byte("---\nid: fallback\nprofile: default-profile\nsandbox_template: default-template\nservices: []\n---\nFallback rules\n"), 0o600))
	definitions, err := workflow.Load(dir, 4096)
	require.NoError(t, err)
	first, second := eligibleReviewInput(), anotherEligibleReviewInput()
	// The second input normally shares the project; route it to a different project.
	second.Project.ID, second.MergeRequest.ProjectID = 75, 75
	second.MRKey = "https://gitlab.example.com:75!2990"
	client := &orpheusMock{create: func(_ context.Context, key orpheus.ReviewSessionKey, _ int64, request orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
		require.Equal(t, workflow.Namespace, key.Namespace)
		var metadata workflow.MetadataV1
		require.NoError(t, json.Unmarshal(*request.Messages[0].Metadata, &metadata))
		if key.MRKey == first.MRKey {
			require.Equal(t, "special", metadata.WorkflowID)
			require.Equal(t, "special-profile", request.Configuration.Agent.Profile)
			require.Equal(t, "special-template", request.Configuration.Sandbox.Template)
			require.Equal(t, []string{"gitlab", "redmine"}, *request.Configuration.Sandbox.Services)
			require.Contains(t, *request.Configuration.Agent.Instructions, "Review output language: ru")
			require.Equal(t, "special-model", *request.Configuration.Agent.Model)
		} else {
			require.Equal(t, "fallback", metadata.WorkflowID)
			require.Equal(t, "default-profile", request.Configuration.Agent.Profile)
			require.Equal(t, "default-template", request.Configuration.Sandbox.Template)
			require.Empty(t, *request.Configuration.Sandbox.Services)
			require.Equal(t, "Fallback rules\n", *request.Configuration.Agent.Instructions)
			require.Nil(t, request.Configuration.Agent.Model)
		}
		require.Equal(t, 3600, *request.Configuration.Limits.RunTimeoutSeconds)
		return orpheus.Accepted{SessionID: "accepted", RunID: "run"}, nil
	}}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(input review.Input) (workflow.SessionContract, error) {
		selected, ok := definitions.Select(input.MergeRequest.ProjectID)
		require.True(t, ok)
		input.MergeRequest.WebURL = "https://gitlab.example.com/team/project/-/merge_requests/2990"
		return workflow.BuildSessionContract(input, selected.Options(workflow.Options{GitLabHost: "https://gitlab.example.com", RunTimeoutSeconds: 3600, HookTimeoutSeconds: 120, MaxRequestBytes: 1 << 20}))
	}, ReconcilerConfig{MaxConcurrent: 1, MatchesWorkflow: func(id int64) bool { _, ok := definitions.Select(id); return ok }})
	require.NoError(t, reconciler.reconcileSnapshot(t.Context(), reviewSnapshot(first, second)))
	require.Equal(t, 1, client.createCalls, "capacity is shared across workflows")
	require.Equal(t, first.MRKey, client.lastKey.MRKey)
	require.NoError(t, reconciler.reconcileSnapshot(t.Context(), reviewSnapshot(second)))
	require.Equal(t, 2, client.createCalls)
	require.Equal(t, second.MRKey, client.lastKey.MRKey)
}
