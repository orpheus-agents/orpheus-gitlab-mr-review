package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/publication"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/workflow"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type orpheusMock struct {
	mu            sync.Mutex
	find          func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error)
	latest        func(context.Context, string, string) (*orpheus.Session, error)
	active        func(context.Context, string) ([]orpheus.Session, error)
	cancel        func(context.Context, string, string) error
	create        func(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error)
	findCalls     int
	activeCalls   int
	cancelCalls   int
	createCalls   int
	lastKey       orpheus.ReviewSessionKey
	lastReviewer  int64
	lastRequest   orpheus.CreateSessionRequest
	metadata      func(context.Context, string, string, string) (json.RawMessage, error)
	metadataCalls int
}

type concurrentOrpheusStub struct {
	find   func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error)
	active func(context.Context, string) ([]orpheus.Session, error)
	cancel func(context.Context, string, string) error
}

func (m *orpheusMock) FindLatestSession(ctx context.Context, namespace, mrKey string) (*orpheus.Session, error) {
	m.mu.Lock()
	latest := m.latest
	m.mu.Unlock()
	if latest == nil {
		return nil, nil
	}
	return latest(ctx, namespace, mrKey)
}

func (concurrentOrpheusStub) FindLatestSession(context.Context, string, string) (*orpheus.Session, error) {
	return nil, nil
}

type gitLabReviewSourceStub struct {
	input gitlab.ReviewInput
	err   error
	calls int
}

type reviewPublisherStub struct {
	skippedCalls int
	calls        int
	input        review.Input
	bundle       protocol.Bundle
	notes        review.NoteTemplates
	err          error
	errorCalls   int
	failure      publication.Failure
	finalized    bool
	finalizeErr  error
	discardCalls int
}

func (s *reviewPublisherStub) Finalize(context.Context, review.Input) (bool, error) {
	return s.finalized, s.finalizeErr
}

func (s *reviewPublisherStub) PublishError(_ context.Context, input review.Input, failure publication.Failure) error {
	s.errorCalls++
	s.input = input
	s.failure = failure
	return s.err
}

func (s *reviewPublisherStub) Discard(context.Context, review.Input) error {
	s.discardCalls++
	return s.err
}

func (s *reviewPublisherStub) Publish(_ context.Context, input review.Input, bundle protocol.Bundle, notes ...review.NoteTemplates) error {
	s.calls++
	s.input = input
	s.bundle = bundle
	if len(notes) > 0 {
		s.notes = notes[0]
	}
	return s.err
}

func (s *gitLabReviewSourceStub) GetReviewInput(context.Context, int64, int64) (gitlab.ReviewInput, error) {
	s.calls++
	return s.input, s.err
}

func (s concurrentOrpheusStub) FindSessions(ctx context.Context, key orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
	return s.find(ctx, key)
}

func (s concurrentOrpheusStub) ListActiveSessions(ctx context.Context, namespace string) ([]orpheus.Session, error) {
	if s.active == nil {
		return nil, nil
	}
	return s.active(ctx, namespace)
}

func (s concurrentOrpheusStub) CancelRun(ctx context.Context, sessionID, runID string) error {
	if s.cancel == nil {
		return errors.New("unexpected cancel run")
	}
	return s.cancel(ctx, sessionID, runID)
}

func (concurrentOrpheusStub) CreateSession(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
	return orpheus.Accepted{}, errors.New("unexpected create session")
}

func (concurrentOrpheusStub) FindMessageMetadata(context.Context, string, string, string) (json.RawMessage, error) {
	return nil, errors.New("unexpected metadata request")
}

func (m *orpheusMock) FindMessageMetadata(ctx context.Context, sessionID, runID, externalKey string) (json.RawMessage, error) {
	m.mu.Lock()
	m.metadataCalls++
	metadata := m.metadata
	m.mu.Unlock()
	if metadata == nil {
		return nil, errors.New("unexpected metadata request")
	}
	return metadata(ctx, sessionID, runID, externalKey)
}

func (m *orpheusMock) FindSessions(ctx context.Context, key orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
	m.mu.Lock()
	m.findCalls++
	m.lastKey = key
	find := m.find
	m.mu.Unlock()
	if find == nil {
		return nil, nil
	}
	return find(ctx, key)
}

func (m *orpheusMock) ListActiveSessions(ctx context.Context, namespace string) ([]orpheus.Session, error) {
	m.mu.Lock()
	m.activeCalls++
	active := m.active
	m.mu.Unlock()
	if active == nil {
		return nil, nil
	}
	return active(ctx, namespace)
}

func (m *orpheusMock) CancelRun(ctx context.Context, sessionID, runID string) error {
	m.mu.Lock()
	m.cancelCalls++
	cancel := m.cancel
	m.mu.Unlock()
	if cancel == nil {
		return errors.New("unexpected cancel run")
	}
	return cancel(ctx, sessionID, runID)
}

func (m *orpheusMock) CreateSession(ctx context.Context, key orpheus.ReviewSessionKey, reviewerID int64, request orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
	m.mu.Lock()
	m.createCalls++
	m.lastKey = key
	m.lastReviewer = reviewerID
	m.lastRequest = request
	create := m.create
	m.mu.Unlock()
	if create == nil {
		return orpheus.Accepted{}, nil
	}
	return create(ctx, key, reviewerID, request)
}

func TestReconcilerCreatesSessionForNewEligibleInput(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.DebugLevel)
	client := &orpheusMock{create: func(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
		return orpheus.Accepted{SessionID: "session-1", RunID: "run-1"}, nil
	}}
	input := eligibleReviewInput()
	contract := testSessionContract(input)
	reconciler := NewReconciler(zap.New(core), nil, client, func(got review.Input) (workflow.SessionContract, error) {
		require.Equal(t, input, got)
		return contract, nil
	})

	err := reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Equal(t, 1, client.findCalls)
	require.Equal(t, 1, client.createCalls)
	require.Equal(t, contract.Key, client.lastKey)
	require.Equal(t, contract.ReviewerID, client.lastReviewer)
	require.Equal(t, contract.Request, client.lastRequest)
	require.Equal(t, 1, logs.FilterMessage("created Orpheus review session").Len())
}

func TestReconcilerDoesNotCreateSecondAnalysisForExistingSession(t *testing.T) {
	t.Parallel()

	statuses := []string{"accepted", "starting", "running", "cancelling", "finalizing", "failed", "cancelled"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
				return []orpheus.Session{{ID: "session-1", RunID: "run-1", Status: status}}, nil
			}}
			buildCalls := 0
			reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
				buildCalls++
				return workflow.SessionContract{}, nil
			})

			err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

			require.NoError(t, err)
			require.Equal(t, 1, client.findCalls)
			require.Zero(t, client.createCalls)
			require.Zero(t, buildCalls)
		})
	}
}

func TestReconcilerContinuesFailedSessionAfterReviewFingerprintChanges(t *testing.T) {
	oldInput := eligibleReviewInput()
	current := oldInput
	current.ReviewFingerprint = strings.Repeat("f", 64)
	metadata := recoveredMetadata(t, oldInput)
	client := &orpheusMock{
		find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) { return nil, nil },
		latest: func(context.Context, string, string) (*orpheus.Session, error) {
			return &orpheus.Session{
				ID: "session-1", RunID: "run-1", MRKey: oldInput.MRKey, Status: "failed",
				InputFingerprint: oldInput.ReviewFingerprint, CreatedAt: time.Now(), ErrorCode: "worker_failed",
			}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) { return metadata, nil },
	}
	publisher := &reviewPublisherStub{}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("an existing assignment must not start another analysis")
		return workflow.SessionContract{}, nil
	}, ReconcilerConfig{Publisher: publisher, MatchesWorkflow: func(int64) bool { t.Fatal("old assignment must not select a new workflow"); return false }})

	err := reconciler.Reconcile(t.Context(), current)

	require.NoError(t, err)
	require.Zero(t, client.createCalls)
	require.Equal(t, 1, publisher.errorCalls)
	require.Equal(t, "run_failed", publisher.failure.Code)
	require.Equal(t, "session-1", publisher.failure.SessionID)
	require.Equal(t, oldInput.ReviewFingerprint, publisher.input.ReviewFingerprint)
}

func TestReconcilerAllowsAnalysisAfterNewReviewerAssignment(t *testing.T) {
	input := eligibleReviewInput()
	assignmentTime := time.Now()
	input.Notes = []gitlab.Note{{
		ID: 99, Author: gitlab.User{ID: 7}, System: true, CreatedAt: assignmentTime,
		Body: "requested review from @reviewer",
	}}
	client := &orpheusMock{
		find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) { return nil, nil },
		latest: func(context.Context, string, string) (*orpheus.Session, error) {
			return &orpheus.Session{
				ID: "old-session", RunID: "old-run", MRKey: input.MRKey, Status: "failed",
				InputFingerprint: strings.Repeat("d", 64), CreatedAt: assignmentTime.Add(-time.Minute),
			}, nil
		},
		create: func(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
			return orpheus.Accepted{SessionID: "new-session", RunID: "new-run"}, nil
		},
	}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(input review.Input) (workflow.SessionContract, error) {
		return testSessionContract(input), nil
	})

	require.NoError(t, reconciler.Reconcile(t.Context(), input))
	require.Equal(t, 1, client.createCalls)
	require.Zero(t, client.metadataCalls)
}

func TestReconcilerPublishesTerminalErrorWithoutAnalysisRetry(t *testing.T) {
	input := eligibleReviewInput()
	client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		return []orpheus.Session{{ID: "session-1", RunID: "run-1", Status: "failed"}}, nil
	}}
	publisher := &reviewPublisherStub{}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("a failed run must not be recreated")
		return workflow.SessionContract{}, nil
	}, ReconcilerConfig{Publisher: publisher})

	require.NoError(t, reconciler.Reconcile(t.Context(), input))
	require.Equal(t, 1, publisher.errorCalls)
	require.Equal(t, publication.Failure{Code: "run_failed", SessionID: "session-1"}, publisher.failure)
	require.Zero(t, client.createCalls)
}

func TestReconcilerValidatesCompletedBundleAgainstImmutableMetadata(t *testing.T) {
	t.Parallel()

	input := eligibleReviewInput()
	input.MergeRequest.WebURL = "https://gitlab.example.com/team/project/-/merge_requests/2989"
	contract, err := workflow.BuildSessionContract(input, workflow.Options{
		WorkflowID:         "retired-workflow",
		Notes:              review.NoteTemplates{Success: "Saved workflow completion: {{ .FindingsCount }}"},
		Services:           []string{},
		GitLabHost:         "https://gitlab.example.com",
		Instructions:       "# Policy\n\nReview the pinned diff.\n",
		AgentProfile:       "review-profile",
		SandboxTemplate:    "review-sandbox",
		RunTimeoutSeconds:  3600,
		HookTimeoutSeconds: 120,
		MaxRequestBytes:    1 << 20,
	})
	require.NoError(t, err)
	bundle := protocol.Bundle{
		SchemaVersion:   protocol.SchemaVersion,
		Stage:           protocol.StageReady,
		Workflow:        workflow.ProtocolExpected(contract.Metadata).Workflow,
		Identity:        workflow.ProtocolExpected(contract.Metadata).Identity,
		Review:          workflow.ProtocolExpected(contract.Metadata).Review,
		Counts:          protocol.Counts{},
		Confirmed:       []protocol.Finding{},
		Recommendations: []protocol.Recommendation{},
		Resolutions:     []protocol.Resolution{},
	}
	output, err := protocol.Encode(bundle, protocol.DefaultLimits())
	require.NoError(t, err)
	metadata, err := json.Marshal(contract.Metadata)
	require.NoError(t, err)
	exitCode := 0
	client := &orpheusMock{
		find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
			return []orpheus.Session{{
				ID:          "session-1",
				RunID:       "run-1",
				Status:      "completed",
				AgentStatus: "completed",
				Hooks: []orpheus.HookResult{{
					Name:               "after_run",
					Status:             "completed",
					ExitCode:           &exitCode,
					OutputCompleteness: "complete",
					OutputType:         "text",
					Output:             string(output),
				}},
			}}, nil
		},
		metadata: func(_ context.Context, sessionID, runID, externalKey string) (json.RawMessage, error) {
			require.Equal(t, "session-1", sessionID)
			require.Equal(t, "run-1", runID)
			require.Equal(t, workflow.MessageExternalKey(input.ReviewFingerprint), externalKey)
			return metadata, nil
		},
	}
	core, logs := observer.New(zap.DebugLevel)
	publisher := &reviewPublisherStub{}
	reconciler := NewReconciler(zap.New(core), nil, client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("completed session must not build a new contract")
		return workflow.SessionContract{}, nil
	}, ReconcilerConfig{Publisher: publisher, MatchesWorkflow: func(int64) bool {
		t.Fatal("accepted session must be recovered even after workflow removal")
		return false
	}})

	err = reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Equal(t, 1, client.metadataCalls)
	require.Zero(t, client.createCalls)
	require.Equal(t, 1, publisher.calls)
	require.Equal(t, input, publisher.input)
	require.Equal(t, bundle, publisher.bundle)
	require.Equal(t, "Saved workflow completion: {{ .FindingsCount }}", publisher.notes.Success)
	require.Equal(t, 1, logs.FilterMessage("completed GitLab review publication").Len())
}

func TestReconcilerRejectsCompletedRunWithoutSuccessfulAgentAndHook(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		session orpheus.Session
		code    string
	}{
		"agent failed": {
			session: orpheus.Session{Status: "completed", AgentStatus: "failed"},
			code:    "agent_not_completed",
		},
		"hook missing": {
			session: orpheus.Session{Status: "completed", AgentStatus: "completed"},
			code:    "after_run_missing",
		},
		"output truncated": {
			session: orpheus.Session{
				Status:      "completed",
				AgentStatus: "completed",
				Hooks: []orpheus.HookResult{{
					Name:               "after_run",
					Status:             "completed",
					ExitCode:           new(0),
					OutputCompleteness: "truncated",
					OutputType:         "text",
				}},
			},
			code: "after_run_output_incomplete",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
				return []orpheus.Session{test.session}, nil
			}}
			reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
				return workflow.SessionContract{}, errors.New("must not build")
			})

			err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

			var lifecycleError *LifecycleError
			require.ErrorAs(t, err, &lifecycleError)
			require.Equal(t, test.code, lifecycleError.Code)
			require.False(t, lifecycleError.Retryable)
			require.Zero(t, client.metadataCalls)
			require.Zero(t, client.createCalls)
		})
	}
}

func TestReconcilerSkipsIneligibleInputBeforeOrpheus(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.DebugLevel)
	client := &orpheusMock{}
	reconciler := NewReconciler(zap.New(core), nil, client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("contract builder must not be called")
		return workflow.SessionContract{}, nil
	})
	input := eligibleReviewInput()
	input.MergeRequest.State = "closed"
	input.MergeRequest.Reviewers = nil

	err := reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Zero(t, client.findCalls)
	require.Zero(t, client.createCalls)
	entries := logs.FilterMessage("GitLab merge request is not eligible for review").All()
	require.Len(t, entries, 1)
	require.Equal(t, []any{"mr_not_open", "reviewer_not_assigned"}, entries[0].ContextMap()["reasons"])
}

func TestReconcilerClassifiesRetryableLifecycleErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err       error
		retryable bool
		code      string
	}{
		"rate limit": {err: &orpheus.Error{Status: 429, Code: "rate_limited"}, retryable: true, code: "rate_limited"},
		"server":     {err: &orpheus.Error{Status: 503, Code: "unavailable"}, retryable: true, code: "unavailable"},
		"request":    {err: &orpheus.Error{Status: 400, Code: "invalid_request"}, retryable: false, code: "invalid_request"},
		"cancelled":  {err: context.Canceled, retryable: false, code: "request_cancelled"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
				return nil, test.err
			}}
			reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
				return workflow.SessionContract{}, nil
			})

			err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

			var lifecycleError *LifecycleError
			require.ErrorAs(t, err, &lifecycleError)
			require.Equal(t, LifecyclePhaseFind, lifecycleError.Phase)
			require.Equal(t, test.code, lifecycleError.Code)
			require.Equal(t, test.retryable, lifecycleError.Retryable)
			require.Equal(t, test.retryable, IsRetryable(err))
		})
	}
}

func TestReconcilerRecoversUncertainCreateWithoutSecondAnalysis(t *testing.T) {
	t.Parallel()

	client := &orpheusMock{}
	client.find = func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		if client.findCalls == 1 {
			return nil, nil
		}
		return []orpheus.Session{{ID: "session-1", RunID: "run-1", Status: "running"}}, nil
	}
	client.create = func(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
		return orpheus.Accepted{}, &orpheus.Error{Status: 503, Code: "response_lost"}
	}
	input := eligibleReviewInput()
	contract := testSessionContract(input)
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
		return contract, nil
	})

	firstErr := reconciler.Reconcile(context.Background(), input)
	secondErr := reconciler.Reconcile(context.Background(), input)

	require.Error(t, firstErr)
	require.True(t, IsRetryable(firstErr))
	require.NoError(t, secondErr)
	require.Equal(t, 2, client.findCalls)
	require.Equal(t, 1, client.createCalls)
}

func TestReconcilerDoesNotRetryInvalidContractOrDuplicateSessions(t *testing.T) {
	t.Parallel()

	input := eligibleReviewInput()
	client := &orpheusMock{}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
		return workflow.SessionContract{}, &workflow.RequestTooLargeError{Size: 2, Limit: 1}
	})

	err := reconciler.Reconcile(context.Background(), input)

	var lifecycleError *LifecycleError
	require.ErrorAs(t, err, &lifecycleError)
	require.Equal(t, LifecyclePhaseBuild, lifecycleError.Phase)
	require.Equal(t, "session_request_too_large", lifecycleError.Code)
	require.False(t, IsRetryable(err))
	require.Zero(t, client.createCalls)

	client.find = func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		return []orpheus.Session{{Status: "running"}, {Status: "running"}}, nil
	}
	err = reconciler.Reconcile(context.Background(), input)
	require.ErrorAs(t, err, &lifecycleError)
	require.Equal(t, "multiple_matching_sessions", lifecycleError.Code)
}

func TestReconcilerReturnsPermanentErrorForUnknownStatus(t *testing.T) {
	t.Parallel()

	client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		return []orpheus.Session{{Status: "new-status"}}, nil
	}}
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(review.Input) (workflow.SessionContract, error) {
		return workflow.SessionContract{}, errors.New("unused")
	})

	err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

	var lifecycleError *LifecycleError
	require.ErrorAs(t, err, &lifecycleError)
	require.Equal(t, "unknown_run_status", lifecycleError.Code)
	require.False(t, lifecycleError.Retryable)
}

func TestReconcilerAndAdapterHTTPContract(t *testing.T) {
	t.Parallel()

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount++
		require.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		response.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case http.MethodGet + " /api/v1/runs":
			require.Equal(t, workflow.Namespace, request.URL.Query().Get("namespace"))
			require.Equal(t, "https://gitlab.example.com:74!2989", request.URL.Query().Get("external_key"))
			if request.URL.Query().Get("input_fingerprint") == "" {
				require.Equal(t, "desc", request.URL.Query().Get("order"))
				require.Equal(t, "1", request.URL.Query().Get("limit"))
			} else {
				require.Equal(t, strings.Repeat("e", 64), request.URL.Query().Get("input_fingerprint"))
			}
			_, _ = response.Write([]byte(`{"items":[],"next_cursor":null}`))
		case http.MethodPost + " /api/v1/sessions":
			require.NotEmpty(t, request.Header.Get("Idempotency-Key"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, workflow.Namespace, body["namespace"])
			require.Equal(t, false, body["allow_multiple_runs"])
			configuration := body["configuration"].(map[string]any)
			sandbox := configuration["sandbox"].(map[string]any)
			require.Equal(t, []any{"gitlab", "redmine"}, sandbox["services"])
			env := sandbox["env"].(map[string]any)
			require.Equal(t, "https://gitlab.example.com/team/project.git", env["ORPHEUS_GITLAB_PROJECT_CLONE_URL"])
			require.NotContains(t, body, "services")
			hooks := configuration["hooks"].(map[string]any)
			require.Contains(t, hooks["before_run"], "git checkout --detach")
			require.Contains(t, hooks["after_run"], "exec python3")
			messages := body["messages"].([]any)
			message := messages[0].(map[string]any)
			metadata := message["metadata"].(map[string]any)
			require.Equal(t, float64(1), metadata["schema_version"])
			response.WriteHeader(http.StatusAccepted)
			_, _ = response.Write([]byte(`{
				"session_id":"00000000-0000-4000-8000-000000000001",
				"run_id":"00000000-0000-4000-8000-000000000002",
				"message_id":"00000000-0000-4000-8000-000000000003"
			}`))
		default:
			http.Error(response, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := orpheus.New(server.URL, "secret", time.Second)
	require.NoError(t, err)
	input := eligibleReviewInput()
	input.MergeRequest.WebURL = "https://gitlab.example.com/team/project/-/merge_requests/2989"
	reconciler := NewReconciler(zap.NewNop(), nil, client, func(input review.Input) (workflow.SessionContract, error) {
		return workflow.BuildSessionContract(input, workflow.Options{
			WorkflowID:         "retired-workflow",
			GitLabHost:         "https://gitlab.example.com",
			Instructions:       "# Project policy\n\nReview the pinned diff.\n",
			AgentProfile:       "review-profile",
			SandboxTemplate:    "review-sandbox",
			Services:           []string{"gitlab", "redmine"},
			RunTimeoutSeconds:  3600,
			HookTimeoutSeconds: 120,
			MaxRequestBytes:    1 << 20,
		})
	})

	err = reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Equal(t, 3, requestCount)
}

func TestAdmissionTransientLookupPreventsAllCreatesInTick(t *testing.T) {
	first := eligibleReviewInput()
	second := anotherEligibleReviewInput()
	client := &orpheusMock{
		find: func(_ context.Context, key orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
			if key.MRKey == first.MRKey {
				return nil, &orpheus.Error{Status: http.StatusServiceUnavailable, Code: "unavailable"}
			}
			return nil, nil
		},
	}
	reconciler := NewReconciler(
		zap.NewNop(),
		nil,
		client,
		func(input review.Input) (workflow.SessionContract, error) { return testSessionContract(input), nil },
		ReconcilerConfig{WorkerCount: 2, QueueCapacity: 1, MaxConcurrent: 2},
	)

	reconciler.processSnapshot(context.Background(), reviewSnapshot(first, second))

	require.Equal(t, 2, client.findCalls)
	require.Zero(t, client.createCalls)
}

func TestAdmissionCreatesOnlyUpToAvailableCapacity(t *testing.T) {
	client := &orpheusMock{}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		nil,
		client,
		func(input review.Input) (workflow.SessionContract, error) { return testSessionContract(input), nil },
		ReconcilerConfig{WorkerCount: 2, QueueCapacity: 1, MaxConcurrent: 1},
	)

	reconciler.processSnapshot(context.Background(), reviewSnapshot(eligibleReviewInput(), anotherEligibleReviewInput()))

	require.Equal(t, 2, client.findCalls)
	require.Equal(t, 1, client.createCalls)
	require.Equal(t, 1, logs.FilterMessage("review admission capacity is exhausted; deferring until the next poll").Len())
}

func TestAdmissionTransientActiveSessionReadPreventsCandidateLookup(t *testing.T) {
	client := &orpheusMock{active: func(context.Context, string) ([]orpheus.Session, error) {
		return nil, &orpheus.Error{Status: http.StatusServiceUnavailable, Code: "unavailable"}
	}}
	reconciler := NewReconciler(
		zap.NewNop(),
		nil,
		client,
		func(input review.Input) (workflow.SessionContract, error) { return testSessionContract(input), nil },
	)

	reconciler.processSnapshot(context.Background(), reviewSnapshot(eligibleReviewInput()))

	require.Zero(t, client.findCalls)
	require.Zero(t, client.createCalls)
}

func TestReconcilerCancelsActiveSessionWhenDiffChanges(t *testing.T) {
	input := eligibleReviewInput()
	metadata := recoveredMetadata(t, input)
	client := &orpheusMock{
		active: func(context.Context, string) ([]orpheus.Session, error) {
			return []orpheus.Session{{
				ID:               "session-1",
				RunID:            "run-1",
				MRKey:            input.MRKey,
				InputFingerprint: input.ReviewFingerprint,
				Status:           "running",
			}}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) {
			return metadata, nil
		},
		cancel: func(context.Context, string, string) error { return nil },
	}
	changed := input
	changed.MergeRequest.DiffRefs.HeadSHA = "changed-head"
	changed.DiffFingerprint = strings.Repeat("c", 64)
	changed.ReviewFingerprint = strings.Repeat("f", 64)
	core, logs := observer.New(zap.DebugLevel)
	publisher := &reviewPublisherStub{}
	reconciler := NewReconciler(
		zap.New(core),
		nil,
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{Publisher: publisher},
	)

	reconciler.processSnapshot(context.Background(), reviewSnapshot(changed))

	require.Equal(t, 1, client.cancelCalls)
	require.Equal(t, 1, publisher.discardCalls)
	require.Zero(t, client.findCalls)
	entries := logs.FilterMessage("cancelled stale Orpheus review session").All()
	require.Len(t, entries, 1)
	require.Equal(t, "diff_changed", entries[0].ContextMap()["cancellation_reason"])
}

func TestReconcilerReadsAndKeepsCurrentActiveSession(t *testing.T) {
	input := eligibleReviewInput()
	metadata := recoveredMetadata(t, input)
	client := &orpheusMock{
		active: func(context.Context, string) ([]orpheus.Session, error) {
			return []orpheus.Session{{
				ID:               "session-1",
				RunID:            "run-1",
				MRKey:            input.MRKey,
				InputFingerprint: input.ReviewFingerprint,
				Status:           "starting",
			}}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) {
			return metadata, nil
		},
	}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		nil,
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
	)

	reconciler.processSnapshot(context.Background(), reviewSnapshot(input))

	require.Zero(t, client.cancelCalls)
	require.Zero(t, client.findCalls)
	require.Zero(t, client.createCalls)
	entries := logs.FilterMessage("reconciled active Orpheus review session").All()
	require.Len(t, entries, 1)
	require.Equal(t, "starting", entries[0].ContextMap()["run_status"])
}

func TestReconcilerWorkersProcessInputsConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	client := concurrentOrpheusStub{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		started <- struct{}{}
		<-release
		return []orpheus.Session{{Status: "running"}}, nil
	}}
	reconciler := NewReconciler(
		zap.NewNop(),
		nil,
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 2, QueueCapacity: 2},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- reconciler.Run(ctx) }()
	<-reconciler.started
	first := eligibleReviewInput()
	second := anotherEligibleReviewInput()
	require.NoError(t, reconciler.Submit(context.Background(), reviewSnapshot(first, second)))

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("reconcile inputs did not start concurrently")
		}
	}
	close(release)
	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	require.NoError(t, reconciler.Stop(shutdownCtx))
	require.NoError(t, <-runDone)
}

func TestReconcilerRecoversAndCancelsRunWhoseReviewerWasRemoved(t *testing.T) {
	input := eligibleReviewInput()
	metadata := recoveredMetadata(t, input)
	client := &orpheusMock{
		active: func(context.Context, string) ([]orpheus.Session, error) {
			return []orpheus.Session{{
				ID:               "session-1",
				RunID:            "run-1",
				MRKey:            input.MRKey,
				InputFingerprint: input.ReviewFingerprint,
				Status:           "running",
			}}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) {
			return metadata, nil
		},
		cancel: func(_ context.Context, sessionID, runID string) error {
			require.Equal(t, "session-1", sessionID)
			require.Equal(t, "run-1", runID)
			return nil
		},
	}
	removed := input
	removed.MergeRequest.Reviewers = nil
	source := &gitLabReviewSourceStub{input: gitlab.ReviewInput{
		MergeRequest: removed.MergeRequest,
		Project:      removed.Project,
		Notes:        removed.Notes,
	}}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		source,
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 2, QueueCapacity: 2},
	)

	reconciler.processSnapshot(context.Background(), reviewSnapshot())

	require.Equal(t, 1, source.calls)
	require.Equal(t, 1, client.cancelCalls)
	require.Equal(t, 1, logs.FilterMessage("cancelled stale Orpheus review session").Len())
}

func TestReconcilerRecoversAndCancelsRunForClosedMergeRequest(t *testing.T) {
	input := eligibleReviewInput()
	metadata := recoveredMetadata(t, input)
	client := &orpheusMock{
		active: func(context.Context, string) ([]orpheus.Session, error) {
			return []orpheus.Session{{ID: "session-1", RunID: "run-1", MRKey: input.MRKey, InputFingerprint: input.ReviewFingerprint, Status: "running"}}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) { return metadata, nil },
		cancel:   func(context.Context, string, string) error { return nil },
	}
	closed := input
	closed.MergeRequest.State = "closed"
	source := &gitLabReviewSourceStub{input: gitlab.ReviewInput{MergeRequest: closed.MergeRequest, Project: closed.Project}}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		source,
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
	)

	reconciler.processSnapshot(context.Background(), reviewSnapshot())

	require.Equal(t, 1, client.cancelCalls)
	entries := logs.FilterMessage("cancelled stale Orpheus review session").All()
	require.Len(t, entries, 1)
	require.Equal(t, "mr_not_open", entries[0].ContextMap()["cancellation_reason"])
}

func TestReconcilerRetriesTransientCancellationOnNextSnapshot(t *testing.T) {
	input := eligibleReviewInput()
	metadata := recoveredMetadata(t, input)
	cancelCalls := 0
	client := &orpheusMock{
		active: func(context.Context, string) ([]orpheus.Session, error) {
			return []orpheus.Session{{ID: "session-1", RunID: "run-1", MRKey: input.MRKey, InputFingerprint: input.ReviewFingerprint, Status: "running"}}, nil
		},
		metadata: func(context.Context, string, string, string) (json.RawMessage, error) {
			return metadata, nil
		},
		cancel: func(context.Context, string, string) error {
			cancelCalls++
			if cancelCalls == 1 {
				return &orpheus.Error{Status: http.StatusServiceUnavailable, Code: "unavailable"}
			}
			return nil
		},
	}
	removed := input
	removed.MergeRequest.Reviewers = nil
	source := &gitLabReviewSourceStub{input: gitlab.ReviewInput{MergeRequest: removed.MergeRequest, Project: removed.Project}}
	reconciler := NewReconciler(zap.NewNop(), source, client, func(review.Input) (workflow.SessionContract, error) {
		return workflow.SessionContract{}, nil
	})

	reconciler.processSnapshot(context.Background(), reviewSnapshot())
	require.Equal(t, 1, cancelCalls)
	reconciler.processSnapshot(context.Background(), reviewSnapshot())

	require.Equal(t, 2, cancelCalls)
}

func TestReconcilerSubmitRejectsDuplicateKeysAndDoesNotBlockWhenFull(t *testing.T) {
	reconciler := NewReconciler(
		zap.NewNop(),
		nil,
		concurrentOrpheusStub{},
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 1, QueueCapacity: 1},
	)
	input := eligibleReviewInput()

	require.Error(t, reconciler.Submit(context.Background(), reviewSnapshot(input, input)))
	require.NoError(t, reconciler.Submit(context.Background(), reviewSnapshot(input)))
	require.Len(t, reconciler.queue, 1)
	require.ErrorIs(t, reconciler.Submit(context.Background(), reviewSnapshot(anotherEligibleReviewInput())), ErrReconcileQueueFull)
	require.Len(t, reconciler.queue, 1)
}

func TestReconcilerGracefulShutdownDrainsAcceptedInputs(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := concurrentOrpheusStub{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		close(started)
		<-release
		return []orpheus.Session{{Status: "running"}}, nil
	}}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		nil,
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 1, QueueCapacity: 1},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- reconciler.Run(ctx) }()
	<-reconciler.started
	require.NoError(t, reconciler.Submit(context.Background(), reviewSnapshot(eligibleReviewInput())))
	<-started
	cancel()
	stopDone := make(chan error, 1)
	go func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
		defer cancelShutdown()
		stopDone <- reconciler.Stop(shutdownCtx)
	}()
	select {
	case <-stopDone:
		t.Fatal("reconciler stopped before the accepted input completed")
	default:
	}
	close(release)

	require.NoError(t, <-stopDone)
	require.NoError(t, <-runDone)
	require.Equal(t, 1, logs.FilterMessage("stopping review reconciler").Len())
	require.Equal(t, 1, logs.FilterMessage("review reconciler stopped").Len())
	require.ErrorIs(t, reconciler.Submit(context.Background(), reviewSnapshot(anotherEligibleReviewInput())), ErrReconcilerStopping)
}

func TestReconcilerCancelsActiveRequestsAfterShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	requestCancelled := make(chan struct{})
	client := concurrentOrpheusStub{find: func(ctx context.Context, _ orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		close(started)
		<-ctx.Done()
		close(requestCancelled)
		return nil, ctx.Err()
	}}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		nil,
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 1, QueueCapacity: 1},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- reconciler.Run(ctx) }()
	<-reconciler.started
	require.NoError(t, reconciler.Submit(context.Background(), reviewSnapshot(eligibleReviewInput())))
	<-started
	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelShutdown()
	require.ErrorIs(t, reconciler.Stop(shutdownCtx), context.DeadlineExceeded)

	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		t.Fatal("active Orpheus request was not cancelled")
	}
	require.NoError(t, <-runDone)
	require.Equal(t, 1, logs.FilterMessage("review reconciler shutdown timed out; cancelling active requests").Len())
}

func testSessionContract(input review.Input) workflow.SessionContract {
	allowMultiple := false
	key := orpheus.ReviewSessionKey{
		Namespace:         workflow.Namespace,
		MRKey:             input.MRKey,
		ReviewFingerprint: input.ReviewFingerprint,
	}
	return workflow.SessionContract{
		Key:        key,
		ReviewerID: input.Reviewer.ID,
		Request: orpheus.CreateSessionRequest{
			AllowMultipleRuns: &allowMultiple,
			Messages:          []orpheus.TextMessage{{Text: "review"}},
		},
	}
}

func eligibleReviewInput() review.Input {
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	return review.Input{
		MRKey:             "https://gitlab.example.com:74!2989",
		DiffFingerprint:   strings.Repeat("d", 64),
		ReviewFingerprint: strings.Repeat("e", 64),
		Reviewer:          reviewer,
		Project: gitlab.Project{
			ID:                74,
			PathWithNamespace: "team/project",
			HTTPURLToRepo:     "https://gitlab.example.com/team/project.git",
			SSHURLToRepo:      "git@gitlab.example.com:team/project.git",
		},
		MergeRequest: gitlab.MergeRequest{
			ProjectID: 74,
			IID:       2989,
			State:     "opened",
			Reviewers: []gitlab.User{reviewer},
			DiffRefs: gitlab.DiffRefs{
				BaseSHA:  "base",
				StartSHA: "start",
				HeadSHA:  "head",
			},
		},
	}
}

func anotherEligibleReviewInput() review.Input {
	input := eligibleReviewInput()
	input.MRKey = "https://gitlab.example.com:74!2990"
	input.ReviewFingerprint = strings.Repeat("f", 64)
	input.MergeRequest.IID = 2990
	return input
}

func reviewSnapshot(inputs ...review.Input) review.Snapshot {
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	return review.Snapshot{Reviewer: reviewer, Inputs: inputs}
}

func recoveredMetadata(t *testing.T, input review.Input) json.RawMessage {
	t.Helper()
	metadata := workflow.MetadataV1{
		SchemaVersion:    workflow.MetadataSchemaVersion,
		WorkflowID:       "retired-workflow",
		WorkflowRevision: "sha256:" + strings.Repeat("a", 64),
		GitLab: workflow.GitLabMetadata{
			Host:             "https://gitlab.example.com",
			ProjectID:        input.MergeRequest.ProjectID,
			ProjectPath:      input.Project.PathWithNamespace,
			MergeRequestIID:  input.MergeRequest.IID,
			ReviewerUserID:   input.Reviewer.ID,
			ReviewerUsername: input.Reviewer.Username,
		},
		Review: workflow.ReviewMetadata{
			DiffFingerprint:   input.DiffFingerprint,
			ReviewFingerprint: input.ReviewFingerprint,
			ArtifactsPath:     ".orpheus/reviews/" + input.DiffFingerprint,
		},
		DiffRefs: workflow.DiffRefsMetadata{
			BaseSHA:  input.MergeRequest.DiffRefs.BaseSHA,
			StartSHA: input.MergeRequest.DiffRefs.StartSHA,
			HeadSHA:  input.MergeRequest.DiffRefs.HeadSHA,
		},
		Protocol: workflow.ProtocolMetadata{
			ArtifactSchemaVersion: workflow.ArtifactSchemaVersion,
			BundleSchemaVersion:   workflow.BundleSchemaVersion,
			HelperSHA256:          "sha256:" + strings.Repeat("b", 64),
		},
	}
	raw, err := json.Marshal(metadata)
	require.NoError(t, err)
	return raw
}

func (s *reviewPublisherStub) PublishSkipped(_ context.Context, input review.Input) error {
	s.skippedCalls++
	s.input = input
	return s.err
}
