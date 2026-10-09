package workflow

import "github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"

const (
	Namespace             = "gitlab/mr-review"
	MetadataSchemaVersion = 1
	ArtifactSchemaVersion = 1
	BundleSchemaVersion   = 1
)

type MetadataV1 struct {
	Notes            *review.NoteTemplates `json:"notes,omitempty"`
	SchemaVersion    int                   `json:"schema_version"`
	WorkflowID       string                `json:"workflow_id"`
	WorkflowRevision string                `json:"workflow_revision"`
	GitLab           GitLabMetadata        `json:"gitlab"`
	Review           ReviewMetadata        `json:"review"`
	DiffRefs         DiffRefsMetadata      `json:"diff_refs"`
	Protocol         ProtocolMetadata      `json:"protocol"`
}

type GitLabMetadata struct {
	Host             string `json:"host"`
	ProjectID        int64  `json:"project_id"`
	ProjectPath      string `json:"project_path"`
	MergeRequestIID  int64  `json:"mr_iid"`
	MergeRequestURL  string `json:"mr_url"`
	ReviewerUserID   int64  `json:"reviewer_user_id"`
	ReviewerUsername string `json:"reviewer_username"`
}

type ReviewMetadata struct {
	DiffFingerprint   string `json:"diff_fingerprint"`
	ReviewFingerprint string `json:"review_fingerprint"`
	ArtifactsPath     string `json:"artifacts_path"`
}

type DiffRefsMetadata struct {
	BaseSHA  string `json:"base_sha"`
	StartSHA string `json:"start_sha"`
	HeadSHA  string `json:"head_sha"`
}

type ProtocolMetadata struct {
	ArtifactSchemaVersion int    `json:"artifact_schema_version"`
	BundleSchemaVersion   int    `json:"bundle_schema_version"`
	HelperSHA256          string `json:"helper_sha256"`
}
