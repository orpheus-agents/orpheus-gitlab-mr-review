package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"

	"github.com/stretchr/testify/require"
)

const testInstructions = "# Kubernetes compliance\n\nApply the mounted project compliance policy.\n"

func TestBuildSessionContract(t *testing.T) {
	t.Parallel()

	input := contractReviewInput()
	options := contractOptions()
	revision, err := Revision(options)
	require.NoError(t, err)
	contract, err := BuildSessionContract(input, options)

	require.NoError(t, err)
	require.Equal(t, Namespace, contract.Key.Namespace)
	require.Equal(t, input.MRKey, contract.Key.MRKey)
	require.Equal(t, input.ReviewFingerprint, contract.Key.ReviewFingerprint)
	require.Equal(t, input.Reviewer.ID, contract.ReviewerID)
	require.Equal(t, revision, contract.WorkflowRevision)
	require.Equal(t, ".orpheus/reviews/"+input.DiffFingerprint, contract.Metadata.Review.ArtifactsPath)
	require.Equal(t, MetadataV1{
		SchemaVersion:    1,
		WorkflowID:       "project-review",
		WorkflowRevision: revision,
		GitLab: GitLabMetadata{
			Host:             "https://gitlab.example.com",
			ProjectID:        74,
			ProjectPath:      "team/project",
			MergeRequestIID:  2989,
			MergeRequestURL:  "https://gitlab.example.com/team/project/-/merge_requests/2989",
			ReviewerUserID:   42,
			ReviewerUsername: "orpheus",
		},
		Review: ReviewMetadata{
			DiffFingerprint:   strings.Repeat("d", 64),
			ReviewFingerprint: strings.Repeat("e", 64),
			ArtifactsPath:     ".orpheus/reviews/" + strings.Repeat("d", 64),
		},
		DiffRefs: DiffRefsMetadata{BaseSHA: "base", StartSHA: "start", HeadSHA: "head"},
		Protocol: ProtocolMetadata{ArtifactSchemaVersion: 1, BundleSchemaVersion: 1, HelperSHA256: helperDigest()},
	}, contract.Metadata)

	request := contract.Request
	require.False(t, *request.AllowMultipleRuns)
	require.Equal(t, Namespace, *request.Namespace)
	require.Equal(t, input.MRKey, *request.ExternalKey)
	require.Equal(t, input.ReviewFingerprint, *request.InputFingerprint)
	require.Equal(t, "review-profile", request.Configuration.Agent.Profile)
	require.Equal(t, "gpt-review", *request.Configuration.Agent.Model)
	require.Equal(t, testInstructions, *request.Configuration.Agent.Instructions)
	require.Equal(t, "review-sandbox", request.Configuration.Sandbox.Template)
	require.Equal(t, []string{"gitlab", "redmine"}, *request.Configuration.Sandbox.Services)
	require.Nil(t, request.Services)
	require.Equal(t, contract.Metadata.Protocol.HelperSHA256, helperDigest())
	require.NotNil(t, request.Configuration.Hooks)
	require.Equal(t, 120, *request.Configuration.Hooks.TimeoutSeconds)
	require.Contains(t, *request.Configuration.Hooks.BeforeRun, "git checkout --detach")
	require.Contains(t, *request.Configuration.Hooks.AfterRun, "exec python3")
	require.Equal(t, input.Project.HTTPURLToRepo, (*request.Env)["ORPHEUS_GITLAB_PROJECT_CLONE_URL"])
	require.Equal(t, input.Project.HTTPURLToRepo, (*request.Configuration.Sandbox.Env)["ORPHEUS_GITLAB_PROJECT_CLONE_URL"])
	require.Equal(t, input.MergeRequest.DiffRefs.HeadSHA, (*request.Configuration.Sandbox.Env)["ORPHEUS_GITLAB_DIFF_HEAD_SHA"])
	require.Equal(t, 3600, *request.Configuration.Limits.RunTimeoutSeconds)
	require.Len(t, request.Messages, 1)
	require.Equal(t, "gitlab-mr-review-input-v1:"+input.ReviewFingerprint, *request.Messages[0].ExternalKey)
	require.Equal(t, contract.Prompt, request.Messages[0].Text)
	require.Contains(t, contract.Prompt, "Review GitLab merge request `!2989`")
	require.Contains(t, contract.Prompt, "ORPHEUS_GITLAB_DIFF_BASE_SHA")
	require.Contains(t, contract.Prompt, `git diff "$ORPHEUS_GITLAB_DIFF_BASE_SHA"..."$ORPHEUS_GITLAB_DIFF_HEAD_SHA"`)
	require.Contains(t, contract.Prompt, `git log "$ORPHEUS_GITLAB_DIFF_BASE_SHA".."$ORPHEUS_GITLAB_DIFF_HEAD_SHA" --oneline`)
	require.Contains(t, contract.Prompt, "leave it unresolved so")
	require.NotContains(t, contract.Prompt, testInstructions)
	require.Contains(t, contract.Prompt, input.ReviewFingerprint)

	var messageMetadata MetadataV1
	require.NoError(t, json.Unmarshal(*request.Messages[0].Metadata, &messageMetadata))
	require.Equal(t, contract.Metadata, messageMetadata)
	require.JSONEq(t, fmt.Sprintf(`{
		"schema_version": 1,
		"workflow_id": "project-review",
		"workflow_revision": %q,
		"gitlab": {
			"host": "https://gitlab.example.com",
			"project_id": 74,
			"project_path": "team/project",
			"mr_iid": 2989,
			"mr_url": "https://gitlab.example.com/team/project/-/merge_requests/2989",
			"reviewer_user_id": 42,
			"reviewer_username": "orpheus"
		},
		"review": {
			"diff_fingerprint": %q,
			"review_fingerprint": %q,
			"artifacts_path": %q
		},
		"diff_refs": {"base_sha": "base", "start_sha": "start", "head_sha": "head"},
		"protocol": {"artifact_schema_version": 1, "bundle_schema_version": 1, "helper_sha256": %q}
	}`,
		revision,
		strings.Repeat("d", 64),
		strings.Repeat("e", 64),
		".orpheus/reviews/"+strings.Repeat("d", 64),
		helperDigest(),
	), string(*request.Messages[0].Metadata))
	encodedRequest, err := json.Marshal(request)
	require.NoError(t, err)
	require.Equal(t, len(encodedRequest), contract.RequestBytes)
}

func TestBuildSessionContractSendsExplicitEmptyServices(t *testing.T) {
	t.Parallel()
	options := contractOptions()
	options.Services = []string{}
	contract, err := BuildSessionContract(contractReviewInput(), options)
	require.NoError(t, err)
	encoded, err := json.Marshal(contract.Request)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"services":[]`)
	options.Services = nil
	_, err = BuildSessionContract(contractReviewInput(), options)
	require.ErrorContains(t, err, "services must be explicitly configured")
}

func TestBuildSessionContractRequiresHTTPSCloneURL(t *testing.T) {
	t.Parallel()

	for _, cloneURL := range []string{"", " \t"} {
		input := contractReviewInput()
		input.Project.HTTPURLToRepo = cloneURL
		_, err := BuildSessionContract(input, contractOptions())
		require.EqualError(t, err, "build session contract: project HTTPS clone URL is required")
	}
	for _, cloneURL := range []string{
		"http://gitlab.example.com/team/project.git",
		"git@gitlab.example.com:team/project.git",
		"ssh://git@gitlab.example.com/team/project.git",
		"file:///tmp/project.git",
		"/tmp/project.git",
		"https:///team/project.git",
		"https://:443/team/project.git",
		"https://gitlab.example.com/%zz",
	} {
		input := contractReviewInput()
		input.Project.HTTPURLToRepo = cloneURL
		_, err := BuildSessionContract(input, contractOptions())
		require.EqualError(t, err, "build session contract: project clone URL must be an absolute HTTPS URL")
	}

	input := contractReviewInput()
	input.Project.HTTPURLToRepo = "  " + input.Project.HTTPURLToRepo + " \t"
	contract, err := BuildSessionContract(input, contractOptions())
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(input.Project.HTTPURLToRepo), (*contract.Request.Env)["ORPHEUS_GITLAB_PROJECT_CLONE_URL"])
}

func TestSessionContractIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := BuildSessionContract(contractReviewInput(), contractOptions())
	require.NoError(t, err)
	second, err := BuildSessionContract(contractReviewInput(), contractOptions())
	require.NoError(t, err)

	firstJSON, err := json.Marshal(first.Request)
	require.NoError(t, err)
	secondJSON, err := json.Marshal(second.Request)
	require.NoError(t, err)
	require.Equal(t, firstJSON, secondJSON)
	require.Equal(t, first.WorkflowRevision, second.WorkflowRevision)
}

func TestBuildSessionContractEnforcesMaximumRequestSize(t *testing.T) {
	t.Parallel()

	input := contractReviewInput()
	options := contractOptions()
	contract, err := BuildSessionContract(input, options)
	require.NoError(t, err)

	options.MaxRequestBytes = contract.RequestBytes - 1
	_, err = BuildSessionContract(input, options)

	var tooLarge *RequestTooLargeError
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, contract.RequestBytes, tooLarge.Size)
	require.Equal(t, options.MaxRequestBytes, tooLarge.Limit)

	options.MaxRequestBytes = contract.RequestBytes
	_, err = BuildSessionContract(input, options)
	require.NoError(t, err)
}

func TestWorkflowRevisionCoversExternalInstructionsAndEmbeddedAssets(t *testing.T) {
	t.Parallel()

	revision, err := Revision(contractOptions())
	require.NoError(t, err)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, revision)
	changedInstructions, err := Revision(Options{Instructions: testInstructions + "Additional rule.\n"})
	require.NoError(t, err)
	require.NotEqual(t, revision, changedInstructions)
	_, err = Revision(Options{Instructions: " \n"})
	require.EqualError(t, err, "build workflow revision: instructions are required")

	base := revisionForParts("first", "second")
	require.NotEqual(t, base, revisionForParts("changed", "second"))
	require.NotEqual(t, base, revisionForParts("second", "first"))
}

func TestEmbeddedHelperProducesBundleAcceptedByConnector(t *testing.T) {
	t.Parallel()

	options := contractOptions()
	options.Notes = review.NoteTemplates{Success: "✅ Completed: {{ .FindingsCount }}", Fail: "⚠️ Failed: {{ .Reason }}"}
	contract, err := BuildSessionContract(contractReviewInput(), options)
	require.NoError(t, err)
	directory := t.TempDir()
	reviewDirectory := filepath.Join(directory, "review")
	for _, name := range []string{"findings", "confirmed", "rejected", "recommendations", "resolutions", "tmp"} {
		require.NoError(t, os.MkdirAll(filepath.Join(reviewDirectory, name), 0o700))
	}
	metadata, err := json.Marshal(contract.Metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(reviewDirectory, "contract.json"), metadata, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(reviewDirectory, "confirmed", "F-0001.md"), []byte(`---
id: F-0001
path: internal/service.go
line: 42
severity: warning
title: Incomplete validation
source: AGENTS.md
---

The input can bypass validation.
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(reviewDirectory, "rejected", "F-0002.md"), []byte(`---
id: F-0002
path: internal/other.go
line: 10
severity: info
title: Invalid candidate
source: manual inspection
---

Rejected after checking the caller.
`), 0o600))
	helperPath := filepath.Join(directory, "review-pack.py")
	require.NoError(t, os.WriteFile(helperPath, []byte(helperAsset), 0o500))
	command := exec.Command("python3", helperPath, reviewDirectory)
	output, err := command.Output()
	require.NoError(t, err)

	bundle, err := protocol.Decode(output, protocol.DefaultLimits(), protocol.Expected{
		Workflow: protocol.WorkflowIdentity{
			ID:           contract.Metadata.WorkflowID,
			Revision:     contract.Metadata.WorkflowRevision,
			HelperSHA256: contract.Metadata.Protocol.HelperSHA256,
		},
		Identity: protocol.ReviewIdentity{
			GitLabHost:      contract.Metadata.GitLab.Host,
			ProjectID:       contract.Metadata.GitLab.ProjectID,
			MergeRequestIID: contract.Metadata.GitLab.MergeRequestIID,
			ReviewerUserID:  contract.Metadata.GitLab.ReviewerUserID,
		},
		Review: protocol.ReviewIdentityV1{
			DiffFingerprint:   contract.Metadata.Review.DiffFingerprint,
			ReviewFingerprint: contract.Metadata.Review.ReviewFingerprint,
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, bundle.Counts.Confirmed)
	require.Equal(t, 1, bundle.Counts.Rejected)
	require.Len(t, bundle.Confirmed, 1)
}

func TestEmbeddedHelperFailsClosedWithPendingFinding(t *testing.T) {
	t.Parallel()

	contract, err := BuildSessionContract(contractReviewInput(), contractOptions())
	require.NoError(t, err)
	directory := t.TempDir()
	reviewDirectory := filepath.Join(directory, "review")
	for _, name := range []string{"findings", "confirmed", "rejected", "recommendations", "resolutions", "tmp"} {
		require.NoError(t, os.MkdirAll(filepath.Join(reviewDirectory, name), 0o700))
	}
	metadata, err := json.Marshal(contract.Metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(reviewDirectory, "contract.json"), metadata, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(reviewDirectory, "findings", "F-0001.md"), []byte("pending"), 0o600))
	helperPath := filepath.Join(directory, "review-pack.py")
	require.NoError(t, os.WriteFile(helperPath, []byte(helperAsset), 0o500))
	command := exec.Command("python3", helperPath, reviewDirectory)
	output, err := command.Output()

	require.Error(t, err)
	require.Empty(t, output)
}

func TestRenderedHooksPreparePinnedCheckoutAndProduceBundle(t *testing.T) {
	t.Parallel()
	for _, binary := range []string{"sh", "git", "python3", "base64", "gzip", "sha256sum"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("required hook runtime %s is unavailable", binary)
		}
	}

	directory := t.TempDir()
	origin := filepath.Join(directory, "origin.git")
	source := filepath.Join(directory, "source")
	workspace := filepath.Join(directory, "workspace")
	runGit(t, directory, "init", "--bare", origin)
	require.NoError(t, os.Mkdir(source, 0o700))
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "review@example.com")
	runGit(t, source, "config", "user.name", "Review Test")
	require.NoError(t, os.WriteFile(filepath.Join(source, "service.go"), []byte("package service\n"), 0o600))
	runGit(t, source, "add", "service.go")
	runGit(t, source, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	require.NoError(t, os.WriteFile(filepath.Join(source, "service.go"), []byte("package service\n\nconst enabled = true\n"), 0o600))
	runGit(t, source, "add", "service.go")
	runGit(t, source, "commit", "-m", "head")
	headSHA := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	runGit(t, source, "remote", "add", "origin", origin)
	runGit(t, source, "push", "origin", "HEAD:refs/heads/main")
	runGit(t, source, "push", "origin", "HEAD:refs/merge-requests/2989/head")
	runGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	require.NoError(t, os.Mkdir(workspace, 0o700))

	input := contractReviewInput()
	input.MergeRequest.DiffRefs.BaseSHA = baseSHA
	input.MergeRequest.DiffRefs.StartSHA = baseSHA
	input.MergeRequest.DiffRefs.HeadSHA = headSHA
	contract, err := BuildSessionContract(input, contractOptions())
	require.NoError(t, err)
	// Exercise the hook commands against a local repository without network authentication.
	hooks, err := renderHooks(contract.Metadata, origin, input.MergeRequest.IID, contractOptions().HookTimeoutSeconds)
	require.NoError(t, err)

	before := exec.Command("sh", "-c", *hooks.Hooks.BeforeRun)
	before.Dir = workspace
	before.Env = hookEnvironment(hooks.Environment)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	before.Stdout = &stdout
	before.Stderr = &stderr
	require.NoError(t, before.Run(), stderr.String())
	require.Empty(t, stdout.String())
	require.Equal(t, headSHA, strings.TrimSpace(runGit(t, workspace, "rev-parse", "HEAD")))
	require.Equal(t, "HEAD", strings.TrimSpace(runGit(t, workspace, "rev-parse", "--abbrev-ref", "HEAD")))
	require.FileExists(t, filepath.Join(workspace, contract.Metadata.Review.ArtifactsPath, "contract.json"))

	findingPath := filepath.Join(workspace, contract.Metadata.Review.ArtifactsPath, "confirmed", "F-0001.md")
	require.NoError(t, os.WriteFile(findingPath, []byte(`---
id: F-0001
path: service.go
line: 3
severity: warning
title: Example finding
source: test policy
previous_discussion_id: old-discussion
previous_note_id: 123
previous_marker: "<!-- orpheus-review-finding:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa -->"
recurrence_comment: "The guard is still missing on the same parser path."
---

The changed behavior needs an explicit guard.
`), 0o600))
	after := exec.Command("sh", "-c", *hooks.Hooks.AfterRun)
	after.Dir = workspace
	after.Env = hookEnvironment(hooks.Environment)
	after.Stderr = &stderr
	output, err := after.Output()
	require.NoError(t, err, stderr.String())

	bundle, err := protocol.Decode(output, protocol.DefaultLimits(), protocolExpected(contract.Metadata))
	require.NoError(t, err)
	require.Equal(t, "F-0001", bundle.Confirmed[0].ID)
	require.Equal(t, "old-discussion", bundle.Confirmed[0].Previous.DiscussionID)
	require.Equal(t, "The guard is still missing on the same parser path.", bundle.Confirmed[0].Previous.RecurrenceComment)
}

func TestEmbeddedPromptOwnsCoreOperatingContractAndIsEnglish(t *testing.T) {
	t.Parallel()

	cyrillic := regexp.MustCompile(`\p{Cyrillic}`)
	require.False(t, cyrillic.MatchString(promptAsset))
	require.Contains(t, promptAsset, "## Operating mode")
	require.Contains(t, promptAsset, "## Pinned diff")
	require.Contains(t, promptAsset, "GitLab mutations are forbidden")
	require.Contains(t, promptAsset, "## Review artifacts")
	require.Contains(t, promptAsset, "## Completion")
	require.NotContains(t, promptAsset, "Kubernetes compliance")
	require.NotContains(t, promptAsset, "analysis retry")
}

func TestMetadataRoundTripAndInputValidation(t *testing.T) {
	t.Parallel()

	input := contractReviewInput()
	contract, err := BuildSessionContract(input, contractOptions())
	require.NoError(t, err)
	raw, err := json.Marshal(contract.Metadata)
	require.NoError(t, err)

	metadata, err := DecodeMetadata(raw)
	require.NoError(t, err)
	require.NoError(t, ValidateMetadata(metadata, input))
	require.Equal(t, protocolExpected(metadata), ProtocolExpected(metadata))

	metadata.Review.ReviewFingerprint = strings.Repeat("f", 64)
	require.EqualError(t, ValidateMetadata(metadata, input), "validate workflow metadata: review fingerprint mismatch")
	_, err = DecodeMetadata([]byte(`{"schema_version":1,"unknown":true}`))
	require.ErrorContains(t, err, `unknown field "unknown"`)
}

func TestBuildSessionContractRejectsInvalidInputBeforeRendering(t *testing.T) {
	t.Parallel()

	input := contractReviewInput()
	input.ReviewFingerprint = "invalid"

	_, err := BuildSessionContract(input, contractOptions())

	require.EqualError(t, err, "build session contract: invalid review fingerprint")
	require.False(t, errors.As(err, new(*RequestTooLargeError)))
}

func contractOptions() Options {
	return Options{
		WorkflowID:         "project-review",
		GitLabHost:         "https://gitlab.example.com/",
		Instructions:       testInstructions,
		AgentProfile:       "review-profile",
		AgentModel:         "gpt-review",
		SandboxTemplate:    "review-sandbox",
		Services:           []string{"gitlab", "redmine"},
		RunTimeoutSeconds:  3600,
		HookTimeoutSeconds: 120,
		MaxRequestBytes:    1 << 20,
	}
}

func protocolExpected(metadata MetadataV1) protocol.Expected {
	return protocol.Expected{
		Workflow: protocol.WorkflowIdentity{
			ID:           metadata.WorkflowID,
			Revision:     metadata.WorkflowRevision,
			HelperSHA256: metadata.Protocol.HelperSHA256,
		},
		Identity: protocol.ReviewIdentity{
			GitLabHost:      metadata.GitLab.Host,
			ProjectID:       metadata.GitLab.ProjectID,
			MergeRequestIID: metadata.GitLab.MergeRequestIID,
			ReviewerUserID:  metadata.GitLab.ReviewerUserID,
		},
		Review: protocol.ReviewIdentityV1{
			DiffFingerprint:   metadata.Review.DiffFingerprint,
			ReviewFingerprint: metadata.Review.ReviewFingerprint,
		},
	}
}

func hookEnvironment(environment map[string]string) []string {
	values := os.Environ()
	for name, value := range environment {
		values = append(values, name+"="+value)
	}

	return values
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))

	return string(output)
}

func contractReviewInput() review.Input {
	reviewer := gitlab.User{ID: 42, Username: "orpheus"}
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
			ID:           100,
			ProjectID:    74,
			IID:          2989,
			Title:        "Prevent duplicate review sessions",
			Description:  "Keep review admission idempotent.",
			State:        "opened",
			SourceBranch: "feature/review",
			TargetBranch: "main",
			WebURL:       "https://gitlab.example.com/team/project/-/merge_requests/2989",
			Reviewers:    []gitlab.User{reviewer},
			DiffRefs: gitlab.DiffRefs{
				BaseSHA:  "base",
				StartSHA: "start",
				HeadSHA:  "head",
			},
		},
	}
}
