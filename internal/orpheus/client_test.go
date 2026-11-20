package orpheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestFindSessionsUsesExactIdentityAndPaginates(t *testing.T) {
	t.Parallel()

	sessionIDs := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000004",
	}
	runIDs := []string{
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "/api/v1/runs", request.URL.Path)
		require.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		require.Equal(t, "gitlab/mr-review", request.URL.Query().Get("namespace"))
		require.Equal(t, "https://gitlab.example.com:42!17", request.URL.Query().Get("external_key"))
		require.Equal(t, "review-fingerprint", request.URL.Query().Get("input_fingerprint"))
		require.Equal(t, "asc", request.URL.Query().Get("order"))
		require.Equal(t, strconv.Itoa(pageSize), request.URL.Query().Get("limit"))

		response.Header().Set("Content-Type", "application/json")
		page := map[string]any{
			"items": []any{map[string]any{
				"id":                runIDs[requests-1],
				"session_id":        sessionIDs[requests-1],
				"status":            "running",
				"input_fingerprint": "review-fingerprint",
				"created_at":        "2026-10-02T10:00:00Z",
			}},
		}
		if requests == 1 {
			require.Empty(t, request.URL.Query().Get("cursor"))
			page["next_cursor"] = "opaque+/cursor"
		} else {
			require.Equal(t, "opaque+/cursor", request.URL.Query().Get("cursor"))
			page["next_cursor"] = nil
		}
		require.NoError(t, json.NewEncoder(response).Encode(page))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	sessions, err := client.FindSessions(context.Background(), testReviewSessionKey())

	require.NoError(t, err)
	require.Equal(t, 2, requests)
	require.Equal(t, []Session{
		{
			ID:               sessionIDs[0],
			RunID:            runIDs[0],
			Status:           "running",
			InputFingerprint: "review-fingerprint",
			CreatedAt:        time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
		},
		{
			ID:               sessionIDs[1],
			RunID:            runIDs[1],
			Status:           "running",
			InputFingerprint: "review-fingerprint",
			CreatedAt:        time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
		},
	}, sessions)
}

func TestListActiveSessionsReadsLatestRunAndProjectsRecoveryIdentity(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		require.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/sessions":
			require.Equal(t, http.MethodGet, request.Method)
			require.Equal(t, "active", request.URL.Query().Get("activity"))
			require.Equal(t, "gitlab/mr-review", request.URL.Query().Get("namespace"))
			require.Equal(t, "asc", request.URL.Query().Get("order"))
			_, _ = io.WriteString(response, `{
				"items":[{
					"id":"00000000-0000-4000-8000-000000000001",
					"configuration":{"sandbox":{"services":["gitlab"],"env_names":[],"env_from":[]}},
                    "last_run_id":"00000000-0000-4000-8000-000000000002",
					"external_key":"https://gitlab.example.com:42!17",
					"allow_multiple_runs":false,
					"status":"running",
					"created_at":"2026-10-02T10:00:00Z",
					"last_run_created_at":"2026-10-02T10:00:00Z"
				}],
				"next_cursor":null
			}`)
		case "/api/v1/sessions/00000000-0000-4000-8000-000000000001/runs/00000000-0000-4000-8000-000000000002":
			require.Equal(t, http.MethodGet, request.Method)
			_, _ = io.WriteString(response, `{
				"id":"00000000-0000-4000-8000-000000000002",
				"session_id":"00000000-0000-4000-8000-000000000001",
				"status":"running",
				"services":["gitlab-registry"],"env_from":[],"env_names":[],"harness_env_names":["GITLAB_TOKEN"],
                "input_fingerprint":"review-fingerprint",
				"created_at":"2026-10-02T10:00:00Z"
			}`)
		default:
			http.Error(response, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	sessions, err := client.ListActiveSessions(context.Background(), "gitlab/mr-review")

	require.NoError(t, err)
	require.Equal(t, 2, requests)
	require.Equal(t, []Session{{
		ID:               "00000000-0000-4000-8000-000000000001",
		RunID:            "00000000-0000-4000-8000-000000000002",
		MRKey:            "https://gitlab.example.com:42!17",
		Status:           "running",
		InputFingerprint: "review-fingerprint",
		CreatedAt:        time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
	}}, sessions)
}

func TestFindSessionsRejectsRepeatedCursor(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"items":[],"next_cursor":"same"}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	_, err := client.FindSessions(context.Background(), testReviewSessionKey())

	require.EqualError(t, err, "find Orpheus sessions: repeated pagination cursor")
}

func TestFindSessionsProjectsAgentAndHookResults(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"items":[{
				"id":"00000000-0000-4000-8000-000000000002",
				"session_id":"00000000-0000-4000-8000-000000000001",
				"status":"completed",
				"agent_status":"completed",
                "services":["gitlab-registry"],"env_names":[],"env_from":[],"harness_env_names":["GITLAB_TOKEN"],
				"error":{"code":"run_failed","message":"Run failure"},
				"agent_error":{"code":"harness_failed","message":"Selected model is at capacity"},
				"input_fingerprint":"review-fingerprint",
				"created_at":"2026-10-02T10:00:00Z",
				"hooks":[{
					"id":"00000000-0000-4000-8000-000000000004",
					"name":"after_run",
                    "env_names":["GITLAB_REGISTRY_TOKEN"],
					"status":"completed",
					"exit_code":0,
					"error":{"code":"hook_failed","message":"Hook execution failed"},
					"output_completeness":"complete",
					"output":{"type":"text","text":"bundle-envelope","original_bytes":15}
				}]
			}],
			"next_cursor":null
		}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	sessions, err := client.FindSessions(context.Background(), testReviewSessionKey())

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "completed", sessions[0].AgentStatus)
	require.Equal(t, "run_failed", sessions[0].ErrorCode)
	require.Equal(t, "Run failure", sessions[0].ErrorMessage)
	require.Equal(t, "harness_failed", sessions[0].AgentErrorCode)
	require.Equal(t, "Selected model is at capacity", sessions[0].AgentErrorMessage)
	require.Equal(t, []HookResult{{
		Name:               "after_run",
		Status:             "completed",
		ExitCode:           new(0),
		OutputCompleteness: "complete",
		OutputType:         "text",
		Output:             "bundle-envelope",
		ErrorCode:          "hook_failed",
		ErrorMessage:       "Hook execution failed",
	}}, sessions[0].Hooks)
}

func TestFindLatestSessionUsesMRIdentityWithoutFingerprint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/api/v1/runs", request.URL.Path)
		require.Equal(t, "gitlab/mr-review", request.URL.Query().Get("namespace"))
		require.Equal(t, "gitlab.example.com:42!17", request.URL.Query().Get("external_key"))
		require.Empty(t, request.URL.Query().Get("input_fingerprint"))
		require.Equal(t, "desc", request.URL.Query().Get("order"))
		require.Equal(t, "1", request.URL.Query().Get("limit"))
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"items":[{
				"id":"00000000-0000-4000-8000-000000000002",
				"session_id":"00000000-0000-4000-8000-000000000001",
				"status":"failed",
				"input_fingerprint":"old-review-fingerprint",
				"created_at":"2026-10-02T10:00:00Z"
			}],
			"next_cursor":null
		}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	session, err := client.FindLatestSession(t.Context(), "gitlab/mr-review", "gitlab.example.com:42!17")

	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, "gitlab.example.com:42!17", session.MRKey)
	require.Equal(t, "old-review-fingerprint", session.InputFingerprint)
	require.Equal(t, "failed", session.Status)
}

func TestFindMessageMetadataUsesExactRunAndExternalKey(t *testing.T) {
	t.Parallel()

	sessionID := "00000000-0000-4000-8000-000000000001"
	runID := "00000000-0000-4000-8000-000000000002"
	externalKey := "gitlab-mr-review-input-v1:review-fingerprint"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "/api/v1/sessions/"+sessionID+"/history", request.URL.Path)
		require.Equal(t, externalKey, request.URL.Query().Get("message_external_key"))
		require.Equal(t, runID, request.URL.Query().Get("run_id"))
		require.Equal(t, strconv.Itoa(pageSize), request.URL.Query().Get("limit"))
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"event_cursor":"event-1",
			"items":[{
				"type":"message",
				"message":{
					"id":"00000000-0000-4000-8000-000000000003",
					"session_id":"00000000-0000-4000-8000-000000000001",
					"run_id":"00000000-0000-4000-8000-000000000002",
					"role":"user",
					"external_key":"gitlab-mr-review-input-v1:review-fingerprint",
					"metadata":{"schema_version":1},
					"created_at":"2026-10-02T10:00:00Z",
					"registered_sequence":"1",
					"text":"review"
				}
			}],
			"next_cursor":null
		}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	metadata, err := client.FindMessageMetadata(context.Background(), sessionID, runID, externalKey)

	require.NoError(t, err)
	require.JSONEq(t, `{"schema_version":1}`, string(metadata))
}

func TestCreateSessionIsAtomicAndUsesStableIdentityKey(t *testing.T) {
	t.Parallel()

	sessionID := "00000000-0000-4000-8000-000000000001"
	runID := "00000000-0000-4000-8000-000000000002"
	messageID := "00000000-0000-4000-8000-000000000003"
	var idempotencyKeys []string
	var requestBodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "/api/v1/sessions", request.URL.Path)
		require.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		idempotencyKeys = append(idempotencyKeys, request.Header.Get("Idempotency-Key"))
		if _, err := uuid.Parse(request.Header.Get("Idempotency-Key")); err != nil {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(response, `{"error":{"code":"idempotency_key_required"}}`)
			return
		}

		var body map[string]any
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		requestBodies = append(requestBodies, body)
		require.Equal(t, "gitlab/mr-review", body["namespace"])
		require.Equal(t, "https://gitlab.example.com:42!17", body["external_key"])
		require.Equal(t, "review-fingerprint", body["input_fingerprint"])
		require.Equal(t, false, body["allow_multiple_runs"])
		require.Len(t, body["messages"], 1)

		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusAccepted)
		require.NoError(t, json.NewEncoder(response).Encode(map[string]any{
			"session_id": sessionID,
			"run_id":     runID,
			"message_id": messageID,
		}))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	request := CreateSessionRequest{
		Configuration: ConfigurationInput{
			Agent:   AgentInput{Profile: "default"},
			Sandbox: SandboxInput{Template: "codex"},
		},
		Messages: []TextMessage{{Text: "Review merge request"}},
	}

	accepted, err := client.CreateSession(context.Background(), testReviewSessionKey(), 42, request)

	require.NoError(t, err)
	require.Equal(t, Accepted{
		SessionID: sessionID,
		RunID:     runID,
		MessageID: messageID,
	}, accepted)

	// Recreate the client to verify the key survives a connector restart.
	restartedClient := newTestClient(t, server.URL)
	_, err = restartedClient.CreateSession(context.Background(), testReviewSessionKey(), 42, request)
	require.NoError(t, err)
	require.Equal(t, idempotencyKeys[0], idempotencyKeys[1])
	require.Equal(t, requestBodies[0], requestBodies[1])
	require.Equal(t,
		"ded6690e-fcbc-54ad-9e7e-6c8279c68480",
		idempotencyKeys[0],
	)
}

func TestClientReturnsSafeTypedError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(response, `{"error":{"code":"temporarily_unavailable","message":"sensitive details","details":[],"phase":null}}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	_, err := client.FindSessions(context.Background(), testReviewSessionKey())

	require.Equal(t, "temporarily_unavailable", Code(err))
	var apiError *Error
	require.ErrorAs(t, err, &apiError)
	require.Equal(t, http.StatusServiceUnavailable, apiError.Status)
	require.NotContains(t, err.Error(), "sensitive details")
}

func TestFindSessionsRequiresCompleteIdentity(t *testing.T) {
	t.Parallel()

	client := &Client{}
	tests := []struct {
		key  ReviewSessionKey
		want string
	}{
		{ReviewSessionKey{MRKey: "external", ReviewFingerprint: "fingerprint"}, "find Orpheus sessions: namespace is required"},
		{ReviewSessionKey{Namespace: "namespace", ReviewFingerprint: "fingerprint"}, "find Orpheus sessions: MR key is required"},
		{ReviewSessionKey{Namespace: "namespace", MRKey: "external"}, "find Orpheus sessions: review fingerprint is required"},
	}
	for _, test := range tests {
		_, err := client.FindSessions(context.Background(), test.key)
		require.EqualError(t, err, test.want)
	}
}

func TestCancelRunAcceptsImmediateAndAsynchronousCancellation(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusOK, http.StatusAccepted} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				require.Equal(t, http.MethodPost, request.Method)
				require.Equal(t, "/api/v1/sessions/00000000-0000-4000-8000-000000000001/runs/00000000-0000-4000-8000-000000000002/cancel", request.URL.Path)
				require.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(status)
				_, _ = io.WriteString(response, `{"run_id":"00000000-0000-4000-8000-000000000002","status":"cancelling"}`)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL)

			err := client.CancelRun(
				context.Background(),
				"00000000-0000-4000-8000-000000000001",
				"00000000-0000-4000-8000-000000000002",
			)

			require.NoError(t, err)
		})
	}
}

func TestCancelRunReturnsTypedSafeError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(response, `{"error":{"code":"temporarily_unavailable","message":"sensitive details","details":[],"phase":null}}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	err := client.CancelRun(
		context.Background(),
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
	)

	require.Equal(t, "temporarily_unavailable", Code(err))
	require.NotContains(t, err.Error(), "sensitive details")
}

func TestCancelRunRejectsInvalidIDs(t *testing.T) {
	t.Parallel()

	client := &Client{}
	require.EqualError(t, client.CancelRun(context.Background(), "invalid", "00000000-0000-4000-8000-000000000002"), "cancel Orpheus run: invalid session ID")
	require.EqualError(t, client.CancelRun(context.Background(), "00000000-0000-4000-8000-000000000001", "invalid"), "cancel Orpheus run: invalid run ID")
}

func TestCreateSessionRejectsIdentityMismatch(t *testing.T) {
	t.Parallel()

	client := &Client{}
	wrongNamespace := "other"
	_, err := client.CreateSession(context.Background(), testReviewSessionKey(), 42, CreateSessionRequest{
		Namespace: &wrongNamespace,
		Messages:  []TextMessage{{Text: "Review merge request"}},
	})

	require.EqualError(t, err, "create Orpheus session: request namespace does not match review session identity")
}

func TestSessionIdempotencyKeyIncludesCompleteIdentity(t *testing.T) {
	t.Parallel()

	key := testReviewSessionKey()
	base := sessionIdempotencyKey(key, 42)

	changedNamespace := key
	changedNamespace.Namespace = "other"
	changedMR := key
	changedMR.MRKey = "https://gitlab.example.com:42!18"
	changedFingerprint := key
	changedFingerprint.ReviewFingerprint = "other-fingerprint"

	require.NotEqual(t, base, sessionIdempotencyKey(changedNamespace, 42))
	require.NotEqual(t, base, sessionIdempotencyKey(changedMR, 42))
	require.NotEqual(t, base, sessionIdempotencyKey(changedFingerprint, 42))
	require.NotEqual(t, base, sessionIdempotencyKey(key, 43))
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(baseURL, "secret", time.Second)
	require.NoError(t, err)

	return client
}

func TestCodeHandlesWrappedAndForeignErrors(t *testing.T) {
	t.Parallel()

	require.Equal(t, "conflict", Code(fmt.Errorf("wrapped: %w", &Error{Status: http.StatusConflict, Code: "conflict"})))
	require.Empty(t, Code(errors.New("foreign")))
}

func testReviewSessionKey() ReviewSessionKey {
	return ReviewSessionKey{
		Namespace:         "gitlab/mr-review",
		MRKey:             "https://gitlab.example.com:42!17",
		ReviewFingerprint: "review-fingerprint",
	}
}
