package publication

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
)

// Failure contains only a classification and a public session identity. Raw
// errors, hook output, and credentials must never reach the GitLab note.
type Failure struct {
	Notes     review.NoteTemplates
	Code      string
	SessionID string
}

// Finalize recovers a terminal publication before any new analysis admission.
// A marker is durable evidence; removing the reviewer is independently retried.
func (p *Publisher) Finalize(ctx context.Context, input review.Input) (bool, error) {
	if err := p.ensureCurrent(ctx, input); err != nil {
		if errors.Is(err, ErrReviewInactive) {
			return true, nil
		}
		return false, err
	}
	discussions, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return false, err
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, CompletionMarker(input.ReviewFingerprint)) &&
		!hasOwnedMarker(discussions, input.Reviewer.ID, ErrorMarker(input.ReviewFingerprint)) &&
		!hasOwnedMarker(discussions, input.Reviewer.ID, SkippedMarker(review.AssignmentKey(input))) {
		return false, nil
	}
	return true, p.removeReviewer(ctx, input)
}

func (p *Publisher) PublishError(ctx context.Context, input review.Input, failure Failure) error {
	if finalized, err := p.Finalize(ctx, input); err != nil || finalized {
		return err
	}
	discussions, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return err
	}
	marker := ErrorMarker(input.ReviewFingerprint)
	body := "⚠️ Automated review could not be completed: " + safeFailureReason(failure.Code) + "."
	if link := p.sessionURL(failure.SessionID); link != "" {
		body += "\n\n[Review details in Orpheus](" + link + ")"
	}
	if failure.Notes.Fail != "" {
		rendered, err := review.RenderNote(failure.Notes.Fail, review.NoteData{Reason: safeFailureReason(failure.Code), SessionURL: p.sessionURL(failure.SessionID)})
		if err != nil {
			return permanent("invalid_failure_note", err)
		}
		body = rendered
	}
	discussions, err = p.ensureNote(ctx, input, body+"\n\n"+marker, marker, discussions)
	if err != nil {
		return err
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return unconfirmed("error_marker_missing")
	}
	return p.removeReviewer(ctx, input)
}

// Discard never publishes a result. For a changed or incomplete diff it removes
// only the bot from the freshly fetched version, guarded again in the adapter.
// Closed MRs and requests already removed by a human require no mutation.
func (p *Publisher) Discard(ctx context.Context, input review.Input) error {
	current, err := p.client.GetMergeRequest(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID)
	if err != nil {
		return classify("read_discard_state", err)
	}
	if current.State != "opened" || !hasReviewer(current.Reviewers, input.Reviewer.ID) {
		return nil
	}
	if err := p.client.RemoveMergeRequestReviewer(ctx, current.ProjectID, current.IID, input.Reviewer.ID, current.DiffRefs); err != nil {
		return classify("discard_review_request", err)
	}
	return nil
}

func (p *Publisher) ensureNote(ctx context.Context, input review.Input, body, marker string, discussions []gitlab.Discussion) ([]gitlab.Discussion, error) {
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	mutationErr := p.client.CreateMergeRequestNote(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, body)
	refreshed, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return discussions, classify("verify_note", errors.Join(mutationErr, err))
	}
	if hasOwnedMarker(refreshed, input.Reviewer.ID, marker) {
		return refreshed, nil
	}
	if mutationErr != nil {
		return refreshed, classify("create_note", mutationErr)
	}
	return refreshed, unconfirmed("note_marker_missing")
}

func (p *Publisher) ensureReply(ctx context.Context, input review.Input, discussionID, body, marker string, discussions []gitlab.Discussion) ([]gitlab.Discussion, error) {
	for _, discussion := range discussions {
		if discussion.ID == discussionID && hasOwnedMarker([]gitlab.Discussion{discussion}, input.Reviewer.ID, marker) {
			return discussions, nil
		}
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	mutationErr := p.client.AddMergeRequestDiscussionNote(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, discussionID, body)
	refreshed, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return discussions, classify("verify_resolution_reply", errors.Join(mutationErr, err))
	}
	for _, discussion := range refreshed {
		if discussion.ID == discussionID && hasOwnedMarker([]gitlab.Discussion{discussion}, input.Reviewer.ID, marker) {
			return refreshed, nil
		}
	}
	if mutationErr != nil {
		return refreshed, classify("create_resolution_reply", mutationErr)
	}
	return refreshed, unconfirmed("resolution_marker_missing")
}

func unconfirmed(code string) error {
	return &Error{Code: code, Retryable: true, Err: errors.New("GitLab mutation is not yet confirmed")}
}

func (p *Publisher) sessionURL(sessionID string) string {
	if _, err := uuid.Parse(sessionID); err != nil {
		return ""
	}
	base, err := url.Parse(p.webBaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return ""
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/sessions/" + sessionID
	base.RawPath = ""
	// Escape Markdown delimiters as well as URL delimiters.
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(base.String())
}

func safeFailureReason(code string) string {
	switch code {
	case "session_request_too_large":
		return "the review request exceeds the configured size limit"
	case "invalid_session_contract":
		return "the review environment could not be prepared"
	case "run_failed", "run_cancelled", "agent_not_completed":
		return "the review run did not finish successfully"
	case "after_run_missing", "after_run_failed", "after_run_output_incomplete", "multiple_after_run_results":
		return "the review result could not be collected and verified"
	default:
		return "the review result could not be safely validated or published"
	}
}

// PublishSkipped records the decision durably before removing the bot reviewer.
func (p *Publisher) PublishSkipped(ctx context.Context, input review.Input) error {
	if finalized, err := p.Finalize(ctx, input); err != nil || finalized {
		return err
	}
	discussions, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return err
	}
	marker := SkippedMarker(review.AssignmentKey(input))
	body := "⏭️ Automated review was skipped: no workflow is configured for this project.\n\n" + marker
	discussions, err = p.ensureNote(ctx, input, body, marker, discussions)
	if err != nil {
		return err
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return unconfirmed("skipped_marker_missing")
	}
	return p.removeReviewer(ctx, input)
}
