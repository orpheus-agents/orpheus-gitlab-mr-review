package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/connector"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/workflow"
	"github.com/stretchr/testify/require"
)

func TestApplicationUsesMountedWorkflowEnvironmentAndSkipsUnmatchedProject(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	requests := make(chan orpheus.CreateSessionRequest, 3)
	var skipBody string
	removed := false
	gitLabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 7 {
			http.NotFound(w, r)
			return
		}
		projectID, err := strconv.ParseInt(parts[4], 10, 64)
		require.NoError(t, err)
		if len(parts) == 7 && r.Method == http.MethodGet {
			reviewers := []any{map[string]any{"id": 42, "username": "reviewer"}, map[string]any{"id": 7, "username": "human"}}
			if removed && projectID == 15 {
				reviewers = reviewers[1:]
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": 1, "iid": 1, "project_id": projectID, "state": "opened", "source_branch": "feature", "target_branch": "main", "reviewers": reviewers, "diff_refs": map[string]string{"base_sha": "base", "start_sha": "start", "head_sha": "head"}}))
		} else if len(parts) == 8 && parts[7] == "discussions" {
			if skipBody == "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			require.NoError(t, json.NewEncoder(w).Encode([]any{map[string]any{"id": "skip", "individual_note": true, "notes": []any{map[string]any{"id": 99, "body": skipBody, "author": map[string]any{"id": 42, "username": "reviewer"}}}}}))
		} else if len(parts) == 8 && parts[7] == "notes" && r.Method == http.MethodPost {
			var body struct {
				Body string `json:"body"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			skipBody = body.Body
			_, _ = w.Write([]byte(`{"id":99}`))
		} else if len(parts) == 7 && r.Method == http.MethodPut {
			var body struct {
				ReviewerIDs []int64 `json:"reviewer_ids"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, []int64{7}, body.ReviewerIDs, "the human reviewer must remain assigned")
			removed = true
			_, _ = w.Write([]byte(`{}`))
		} else {
			http.NotFound(w, r)
		}
	}))
	defer gitLabServer.Close()
	orpheusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/runs":
			require.Equal(t, workflow.Namespace, r.URL.Query().Get("namespace"))
			_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
		case "POST /api/v1/sessions":
			var request orpheus.CreateSessionRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			requests <- request
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"session_id":"00000000-0000-4000-8000-000000000001","run_id":"00000000-0000-4000-8000-000000000002","message_id":"00000000-0000-4000-8000-000000000003"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer orpheusServer.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects.md"), []byte("---\nid: special\nproject_ids: [10]\nprofile: project-profile\nsandbox_template: project-template\nservices: [gitlab]\nlanguage: ru\nnotes:\n  success: 'Frozen success: {{ .FindingsCount }}'\n  fail: 'Frozen failure: {{ .Reason }}'\n---\nProject instructions\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "default.md"), []byte("---\nid: fallback\nprofile: fallback-profile\nsandbox_template: fallback-template\nservices: []\n---\nFallback instructions\n"), 0o600))
	cfg := config.Config{LogLevel: "error", HTTPTimeout: time.Second, PollInterval: time.Minute, ShutdownTimeout: time.Second, RunTimeout: time.Hour, HookTimeout: time.Minute, MaxSessionRequestBytes: 1 << 20, WorkflowsDir: dir, GitLab: config.GitLab{BaseURL: gitLabServer.URL, Token: "token"}, Orpheus: config.Orpheus{BaseURL: orpheusServer.URL, APIKey: "key"}}
	application, err := New(cfg)
	require.NoError(t, err)
	defer application.Close()
	reconciler := application.services[0].service.(*connector.Reconciler)
	reconcile := func(reconciler *connector.Reconciler, projectID int64) {
		source := gitlab.ReviewInput{Project: gitlab.Project{ID: projectID, PathWithNamespace: "team/project", HTTPURLToRepo: "https://gitlab.example.test/team/project.git"}, MergeRequest: gitlab.MergeRequest{ID: 1, IID: 1, ProjectID: projectID, State: "opened", SourceBranch: "feature", TargetBranch: "main", WebURL: "https://gitlab.example.test/team/project/-/merge_requests/1", Reviewers: []gitlab.User{{ID: 42, Username: "reviewer"}, {ID: 7, Username: "human"}}, DiffRefs: gitlab.DiffRefs{BaseSHA: "base", StartSHA: "start", HeadSHA: "head"}}}
		input, err := review.NewInput(gitLabServer.URL, gitlab.User{ID: 42, Username: "reviewer"}, source)
		require.NoError(t, err)
		require.NoError(t, reconciler.Reconcile(t.Context(), input))
	}
	// Editing a mounted file has no effect on an already initialized application.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects.md"), []byte("invalid replacement"), 0o600))
	for i, projectID := range []int64{10, 14} {
		reconcile(reconciler, projectID)
		var request orpheus.CreateSessionRequest
		select {
		case request = <-requests:
		case <-time.After(time.Second):
			t.Fatal("expected a workflow session request")
		}
		require.Equal(t, []string{"project-profile", "fallback-profile"}[i], request.Configuration.Agent.Profile)
		require.Equal(t, []string{"project-template", "fallback-template"}[i], request.Configuration.Sandbox.Template)
		require.Equal(t, 3600, *request.Configuration.Limits.RunTimeoutSeconds)
		require.Equal(t, 60, *request.Configuration.Hooks.TimeoutSeconds)
		var metadata workflow.MetadataV1
		require.NoError(t, json.Unmarshal(*request.Messages[0].Metadata, &metadata))
		require.Equal(t, []string{"special", "fallback"}[i], metadata.WorkflowID)
		if i == 0 {
			require.Equal(t, &review.NoteTemplates{Success: "Frozen success: {{ .FindingsCount }}", Fail: "Frozen failure: {{ .Reason }}"}, metadata.Notes)
			require.Contains(t, *request.Configuration.Agent.Instructions, "Review output language: ru")
		} else {
			require.Nil(t, metadata.Notes)
			require.Equal(t, "Fallback instructions\n", *request.Configuration.Agent.Instructions)
			require.Empty(t, *request.Configuration.Sandbox.Services)
		}
	}
	_, err = New(cfg)
	require.ErrorContains(t, err, "projects.md")
	// A restarted application with only the project-specific workflow skips other projects.
	require.NoError(t, os.Remove(filepath.Join(dir, "default.md")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects.md"), []byte("---\nid: special\nproject_ids: [10]\nprofile: project-profile\nsandbox_template: project-template\nservices: []\n---\nInstructions\n"), 0o600))
	restarted, err := New(cfg)
	require.NoError(t, err)
	defer restarted.Close()
	reconcile(restarted.services[0].service.(*connector.Reconciler), 15)
	require.Empty(t, requests)
	mu.Lock()
	defer mu.Unlock()
	require.True(t, removed)
	require.Contains(t, skipBody, "⏭️")
	require.Contains(t, skipBody, "no workflow is configured for this project")
	require.True(t, strings.HasSuffix(skipBody, " -->"), "missing durable marker: %s", skipBody)
}
