package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"

	"go.uber.org/zap"
)

var ErrStaleReview = errors.New("review diff is stale")
var ErrReviewInactive = errors.New("review request is no longer active")

type Client interface {
	GetMergeRequest(ctx context.Context, projectID, iid int64) (gitlab.MergeRequest, error)
	ListMergeRequestDiscussions(ctx context.Context, projectID, iid int64) ([]gitlab.Discussion, error)
	ListMergeRequestDiffs(ctx context.Context, projectID, iid int64) ([]gitlab.DiffFile, error)
	CreateMergeRequestDiscussion(ctx context.Context, projectID, iid int64, body string, position gitlab.Position) error
	CreateMergeRequestNote(ctx context.Context, projectID, iid int64, body string) error
	AddMergeRequestDiscussionNote(ctx context.Context, projectID, iid int64, discussionID, body string) error
	SetMergeRequestDiscussionResolved(ctx context.Context, projectID, iid int64, discussionID string, resolved bool) error
	RemoveMergeRequestReviewer(ctx context.Context, projectID, iid, reviewerID int64, expected gitlab.DiffRefs) error
}

type Error struct {
	Code      string
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("GitLab publication failed (%s): %v", e.Code, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

func IsRetryable(err error) bool {
	var publicationError *Error
	return errors.As(err, &publicationError) && publicationError.Retryable
}

func Code(err error) string {
	var publicationError *Error
	if errors.As(err, &publicationError) {
		return publicationError.Code
	}
	return ""
}

type Publisher struct {
	logger     *zap.Logger
	client     Client
	host       string
	webBaseURL string
}

func New(logger *zap.Logger, client Client, gitLabHost string, webBaseURL ...string) *Publisher {
	p := &Publisher{logger: logger, client: client, host: gitLabHost}
	if len(webBaseURL) > 0 {
		p.webBaseURL = webBaseURL[0]
	}
	return p
}

func (p *Publisher) Publish(ctx context.Context, input review.Input, bundle protocol.Bundle, notes ...review.NoteTemplates) error {
	if p.client == nil {
		return permanent("publisher_not_configured", errors.New("GitLab publication client is missing"))
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}

	projectID := input.MergeRequest.ProjectID
	iid := input.MergeRequest.IID
	discussions, err := p.client.ListMergeRequestDiscussions(ctx, projectID, iid)
	if err != nil {
		return classify("list_discussions", err)
	}
	completionMarker := CompletionMarker(bundle.Review.ReviewFingerprint)
	if hasOwnedMarker(discussions, input.Reviewer.ID, completionMarker) {
		return p.removeReviewer(ctx, input)
	}
	if hasOwnedMarker(discussions, input.Reviewer.ID, ErrorMarker(input.ReviewFingerprint)) ||
		hasOwnedMarker(discussions, input.Reviewer.ID, SkippedMarker(review.AssignmentKey(input))) {
		return p.removeReviewer(ctx, input)
	}
	diffs, err := p.client.ListMergeRequestDiffs(ctx, projectID, iid)
	if err != nil {
		return classify("list_diffs", err)
	}

	for _, finding := range bundle.Confirmed {
		discussions, err = p.publishFinding(ctx, input, finding, diffs, discussions)
		if err != nil {
			return err
		}
	}
	for _, recommendation := range bundle.Recommendations {
		discussions, err = p.publishRecommendation(ctx, input, bundle, recommendation, discussions)
		if err != nil {
			return err
		}
	}
	for _, resolution := range bundle.Resolutions {
		discussions, err = p.applyResolution(ctx, input, resolution, discussions)
		if err != nil {
			return err
		}
	}

	completionBody := completionBody(len(bundle.Confirmed), completionMarker)
	if len(notes) > 0 && notes[0].Success != "" {
		rendered, err := review.RenderNote(notes[0].Success, review.NoteData{FindingsCount: len(bundle.Confirmed)})
		if err != nil {
			return permanent("invalid_success_note", err)
		}
		completionBody = rendered + "\n\n" + completionMarker
	}
	discussions, err = p.ensureNote(ctx, input, completionBody, completionMarker, discussions)
	if err != nil {
		return err
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, completionMarker) {
		return unconfirmed("completion_marker_missing")
	}

	p.logger.Info("published GitLab review",
		zap.String("merge_request_key", input.MRKey),
		zap.Int("confirmed_findings", len(bundle.Confirmed)),
		zap.Int("recommendations", len(bundle.Recommendations)),
		zap.Int("resolutions", len(bundle.Resolutions)),
	)
	return p.removeReviewer(ctx, input)
}

func (p *Publisher) publishFinding(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	diffs []gitlab.DiffFile,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	marker := FindingMarker(input.DiffFingerprint, finding)
	if finding.Previous != nil {
		return p.publishRecurringFinding(ctx, input, finding, marker, diffs, discussions)
	}
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	return p.publishNewFinding(ctx, input, finding, marker, diffs, discussions)
}

func (p *Publisher) publishRecurringFinding(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	marker string,
	diffs []gitlab.DiffFile,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	previous := *finding.Previous
	threadIndex, _, ok := ownedMarkerTarget(
		discussions, input.Reviewer.ID, previous.DiscussionID, previous.NoteID, previous.Marker,
	)
	if !ok {
		return discussions, permanent("previous_finding_not_owned", fmt.Errorf("previous finding target %q/%d is not an owned Orpheus note", previous.DiscussionID, previous.NoteID))
	}
	thread := discussions[threadIndex]
	if !thread.Resolved {
		return discussions, nil
	}

	switch thread.ResolutionCause {
	case gitlab.ResolutionCauseOutdatedByPush:
		if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
			return discussions, nil
		}
		return p.publishNewFinding(ctx, input, finding, marker, diffs, discussions)
	case gitlab.ResolutionCauseExplicit:
		if thread.ResolvedBy.ID == input.Reviewer.ID {
			if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
				return discussions, nil
			}
			return p.publishNewFinding(ctx, input, finding, marker, diffs, discussions)
		}
		if !hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
			body := strings.TrimSpace(previous.RecurrenceComment) + "\n\n" + marker
			var err error
			discussions, err = p.ensureReply(ctx, input, previous.DiscussionID, body, marker, discussions)
			if err != nil {
				return discussions, err
			}
		}
		threadIndex, _, ok = ownedMarkerTarget(
			discussions, input.Reviewer.ID, previous.DiscussionID, previous.NoteID, previous.Marker,
		)
		if !ok {
			return discussions, permanent("previous_finding_disappeared", errors.New("previous finding target disappeared while publishing"))
		}
		if !discussions[threadIndex].Resolved {
			return discussions, nil
		}
		if err := p.ensureCurrent(ctx, input); err != nil {
			return discussions, err
		}
		if err := p.client.SetMergeRequestDiscussionResolved(
			ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, previous.DiscussionID, false,
		); err != nil {
			refreshed, fetchErr := p.refreshDiscussions(ctx, input)
			if fetchErr != nil {
				return discussions, classify("verify_reopen_discussion", errors.Join(err, fetchErr))
			}
			discussions = refreshed
			threadIndex, _, ok = ownedMarkerTarget(
				discussions, input.Reviewer.ID, previous.DiscussionID, previous.NoteID, previous.Marker,
			)
			if !ok || discussions[threadIndex].Resolved {
				return discussions, classify("reopen_discussion", err)
			}
			return discussions, nil
		}
		refreshed, err := p.refreshDiscussions(ctx, input)
		if err != nil {
			return discussions, classify("verify_reopen_discussion", err)
		}
		threadIndex, _, ok = ownedMarkerTarget(
			refreshed, input.Reviewer.ID, previous.DiscussionID, previous.NoteID, previous.Marker,
		)
		if !ok || refreshed[threadIndex].Resolved {
			return refreshed, unconfirmed("reopen_not_confirmed")
		}
		return refreshed, nil
	case gitlab.ResolutionCauseUnknown:
		return discussions, permanent("unknown_discussion_resolution_cause", fmt.Errorf("cannot safely classify how discussion %q was resolved", previous.DiscussionID))
	default:
		return discussions, permanent("invalid_discussion_resolution_state", fmt.Errorf("discussion %q is resolved without a resolution cause", previous.DiscussionID))
	}
}

func (p *Publisher) publishNewFinding(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	marker string,
	diffs []gitlab.DiffFile,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	body := findingBody(finding, marker)
	diff, ok := findDiff(diffs, finding.Path)
	if !ok || diff.Collapsed || diff.TooLarge {
		reason := "path is not available in the current GitLab diff"
		if ok {
			reason = "GitLab omitted the file diff because it is collapsed or too large"
		}
		return p.publishFindingFallback(ctx, input, finding, body, marker, reason, discussions)
	}
	position, err := diffPosition(input.MergeRequest.DiffRefs, diff, finding.Line)
	if err != nil {
		return p.publishFindingFallback(ctx, input, finding, body, marker, err.Error(), discussions)
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	err = p.client.CreateMergeRequestDiscussion(
		ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, body, position,
	)
	refreshed, fetchErr := p.refreshDiscussions(ctx, input)
	if fetchErr != nil {
		return discussions, classify("verify_finding", errors.Join(err, fetchErr))
	}
	if hasOwnedMarker(refreshed, input.Reviewer.ID, marker) {
		return refreshed, nil
	}
	if err == nil {
		return refreshed, unconfirmed("finding_marker_missing")
	}
	if errors.Is(err, gitlab.ErrInvalidDiscussionPosition) {
		return p.publishFindingFallback(ctx, input, finding, body, marker, "GitLab rejected the current inline position", refreshed)
	}
	return refreshed, classify("create_finding", err)
}

func (p *Publisher) publishFindingFallback(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	findingText, marker, reason string,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	body := fmt.Sprintf("⚠️ **Inline review finding for `%s:%d`**\n\n%s\n\nInline fallback: %s\n\n%s",
		finding.Path, finding.Line, strings.TrimSpace(strings.TrimSuffix(findingText, marker)), reason, marker)
	return p.ensureNote(ctx, input, body, marker, discussions)
}

func (p *Publisher) publishRecommendation(
	ctx context.Context,
	input review.Input,
	bundle protocol.Bundle,
	recommendation protocol.Recommendation,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	marker := RecommendationMarker(bundle.Review.DiffFingerprint, recommendation)
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	body := "**Orpheus project-rule recommendation**\n\n" + strings.TrimSpace(recommendation.Body) + "\n\n" + marker
	return p.ensureNote(ctx, input, body, marker, discussions)
}

func (p *Publisher) applyResolution(
	ctx context.Context,
	input review.Input,
	resolution protocol.Resolution,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	threadIndex, _, ok := ownedMarkerTarget(
		discussions, input.Reviewer.ID, resolution.DiscussionID, resolution.NoteID, resolution.Marker,
	)
	if !ok {
		return discussions, permanent("resolution_not_owned", errors.New("resolution target is not an owned Orpheus finding"))
	}
	marker := ResolutionMarker(input.ReviewFingerprint, resolution)
	if !hasOwnedMarker([]gitlab.Discussion{discussions[threadIndex]}, input.Reviewer.ID, marker) {
		var err error
		discussions, err = p.ensureReply(ctx, input, resolution.DiscussionID,
			strings.TrimSpace(resolution.Body)+"\n\n"+marker, marker, discussions)
		if err != nil {
			return discussions, err
		}
		threadIndex, _, ok = ownedMarkerTarget(discussions, input.Reviewer.ID,
			resolution.DiscussionID, resolution.NoteID, resolution.Marker)
		if !ok {
			return discussions, permanent("resolution_target_disappeared", errors.New("resolution target disappeared"))
		}
	}
	if discussions[threadIndex].Resolved {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	if err := p.client.SetMergeRequestDiscussionResolved(
		ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, resolution.DiscussionID, true,
	); err != nil {
		refreshed, fetchErr := p.refreshDiscussions(ctx, input)
		if fetchErr != nil {
			return discussions, classify("verify_resolution", errors.Join(err, fetchErr))
		}
		threadIndex, _, ok = ownedMarkerTarget(
			refreshed, input.Reviewer.ID, resolution.DiscussionID, resolution.NoteID, resolution.Marker,
		)
		if !ok || !refreshed[threadIndex].Resolved {
			return refreshed, classify("resolve_discussion", err)
		}
		return refreshed, nil
	}
	refreshed, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return discussions, err
	}
	threadIndex, _, ok = ownedMarkerTarget(refreshed, input.Reviewer.ID,
		resolution.DiscussionID, resolution.NoteID, resolution.Marker)
	if !ok || !refreshed[threadIndex].Resolved {
		return refreshed, unconfirmed("resolution_not_confirmed")
	}
	return refreshed, nil
}

func (p *Publisher) ensureCurrent(ctx context.Context, input review.Input) error {
	current, err := p.client.GetMergeRequest(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID)
	if err != nil {
		return classify("read_current_merge_request", err)
	}
	if current.State != "opened" {
		return permanent("merge_request_not_open", ErrReviewInactive)
	}
	if !hasReviewer(current.Reviewers, input.Reviewer.ID) {
		return permanent("reviewer_not_assigned", ErrReviewInactive)
	}
	if strings.TrimSpace(current.DiffRefs.BaseSHA) == "" || strings.TrimSpace(current.DiffRefs.StartSHA) == "" ||
		strings.TrimSpace(current.DiffRefs.HeadSHA) == "" {
		return permanent("diff_refs_incomplete", ErrStaleReview)
	}
	fingerprint, err := review.DiffFingerprint(p.host, current)
	if err != nil {
		return permanent("diff_fingerprint_failed", err)
	}
	if fingerprint != input.DiffFingerprint {
		return permanent("stale_diff", ErrStaleReview)
	}
	return nil
}

func (p *Publisher) refreshDiscussions(ctx context.Context, input review.Input) ([]gitlab.Discussion, error) {
	discussions, err := p.client.ListMergeRequestDiscussions(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID)
	if err != nil {
		return nil, classify("list_discussions", err)
	}
	return discussions, nil
}

func (p *Publisher) removeReviewer(ctx context.Context, input review.Input) error {
	if err := p.ensureCurrent(ctx, input); err != nil {
		if errors.Is(err, ErrReviewInactive) {
			return nil
		}
		return err
	}
	if err := p.client.RemoveMergeRequestReviewer(
		ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, input.Reviewer.ID, input.MergeRequest.DiffRefs,
	); err != nil {
		if errors.Is(err, gitlab.ErrReviewInputChanged) {
			return permanent("stale_diff", ErrStaleReview)
		}
		current, fetchErr := p.client.GetMergeRequest(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID)
		if fetchErr == nil && !hasReviewer(current.Reviewers, input.Reviewer.ID) {
			return nil
		}
		return classify("remove_reviewer", errors.Join(err, fetchErr))
	}
	p.logger.Info("removed GitLab review request",
		zap.String("merge_request_key", input.MRKey),
		zap.Int64("reviewer_user_id", input.Reviewer.ID),
	)
	return nil
}

func classify(code string, err error) error {
	var publicationError *Error
	retryable := gitlab.IsRetryable(err)
	if errors.As(err, &publicationError) {
		retryable = retryable || publicationError.Retryable
	}
	return &Error{Code: code, Retryable: retryable, Err: err}
}

func permanent(code string, err error) error {
	return &Error{Code: code, Retryable: false, Err: err}
}
