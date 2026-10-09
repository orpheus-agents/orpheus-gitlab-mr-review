package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func DecodeMetadata(raw json.RawMessage) (MetadataV1, error) {
	var metadata MetadataV1
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return MetadataV1{}, fmt.Errorf("decode workflow metadata: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return MetadataV1{}, errors.New("decode workflow metadata: multiple JSON values")
		}
		return MetadataV1{}, fmt.Errorf("decode workflow metadata: %w", err)
	}

	return metadata, nil
}

func ValidateMetadata(metadata MetadataV1, input review.Input) error {
	if err := ValidateRecoveredMetadata(metadata, input.MRKey, input.ReviewFingerprint, input.Reviewer.ID); err != nil {
		return err
	}
	switch {
	case metadata.GitLab.ProjectID != input.MergeRequest.ProjectID || metadata.GitLab.MergeRequestIID != input.MergeRequest.IID:
		return errors.New("validate workflow metadata: GitLab identity mismatch")
	case metadata.Review.DiffFingerprint != input.DiffFingerprint:
		return errors.New("validate workflow metadata: review fingerprint mismatch")
	case metadata.DiffRefs.BaseSHA != input.MergeRequest.DiffRefs.BaseSHA ||
		metadata.DiffRefs.StartSHA != input.MergeRequest.DiffRefs.StartSHA ||
		metadata.DiffRefs.HeadSHA != input.MergeRequest.DiffRefs.HeadSHA:
		return errors.New("validate workflow metadata: diff refs mismatch")
	}

	return nil
}

// ValidateRecoveredMetadata validates the immutable identity needed to recover
// an existing review session without requiring its current GitLab state to
// still match the reviewed diff.
func ValidateRecoveredMetadata(metadata MetadataV1, mrKey, reviewFingerprint string, reviewerID int64) error {
	if metadata.Notes != nil {
		if err := metadata.Notes.Validate(); err != nil {
			return fmt.Errorf("validate workflow metadata: %w", err)
		}
	}
	expectedMRKey := fmt.Sprintf("%s:%d!%d", metadata.GitLab.Host, metadata.GitLab.ProjectID, metadata.GitLab.MergeRequestIID)
	switch {
	case metadata.SchemaVersion != MetadataSchemaVersion:
		return errors.New("validate workflow metadata: unsupported schema version")
	case !ValidID(metadata.WorkflowID):
		return errors.New("validate workflow metadata: invalid workflow ID")
	case !digestPattern.MatchString(metadata.WorkflowRevision):
		return errors.New("validate workflow metadata: invalid workflow revision")
	case metadata.Protocol.ArtifactSchemaVersion != ArtifactSchemaVersion:
		return errors.New("validate workflow metadata: unsupported artifact schema version")
	case metadata.Protocol.BundleSchemaVersion != BundleSchemaVersion:
		return errors.New("validate workflow metadata: unsupported bundle schema version")
	case !digestPattern.MatchString(metadata.Protocol.HelperSHA256):
		return errors.New("validate workflow metadata: invalid helper digest")
	case expectedMRKey != mrKey:
		return errors.New("validate workflow metadata: merge request identity mismatch")
	case metadata.GitLab.ReviewerUserID != reviewerID:
		return errors.New("validate workflow metadata: reviewer identity mismatch")
	case metadata.Review.ReviewFingerprint != reviewFingerprint:
		return errors.New("validate workflow metadata: review fingerprint mismatch")
	case metadata.Review.ArtifactsPath != ".orpheus/reviews/"+metadata.Review.DiffFingerprint:
		return errors.New("validate workflow metadata: artifacts path mismatch")
	case metadata.DiffRefs.BaseSHA == "" || metadata.DiffRefs.StartSHA == "" || metadata.DiffRefs.HeadSHA == "":
		return errors.New("validate workflow metadata: incomplete diff refs")
	}

	return nil
}

func ProtocolExpected(metadata MetadataV1) protocol.Expected {
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
