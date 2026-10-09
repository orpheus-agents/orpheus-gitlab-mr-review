package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/publication"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/workflow"

	"go.uber.org/zap"
)

type orpheusAdapter interface {
	FindSessions(ctx context.Context, key orpheus.ReviewSessionKey) ([]orpheus.Session, error)
	FindLatestSession(ctx context.Context, namespace, mrKey string) (*orpheus.Session, error)
	ListActiveSessions(ctx context.Context, namespace string) ([]orpheus.Session, error)
	CancelRun(ctx context.Context, sessionID, runID string) error
	CreateSession(
		ctx context.Context,
		key orpheus.ReviewSessionKey,
		reviewerID int64,
		request orpheus.CreateSessionRequest,
	) (orpheus.Accepted, error)
	FindMessageMetadata(
		ctx context.Context,
		sessionID string,
		runID string,
		externalKey string,
	) (json.RawMessage, error)
}

type gitLabReviewSource interface {
	GetReviewInput(ctx context.Context, projectID, iid int64) (gitlab.ReviewInput, error)
}

type SessionContractBuilder func(input review.Input) (workflow.SessionContract, error)

type ReviewPublisher interface {
	Publish(ctx context.Context, input review.Input, bundle protocol.Bundle, notes ...review.NoteTemplates) error
	PublishError(ctx context.Context, input review.Input, failure publication.Failure) error
	PublishSkipped(ctx context.Context, input review.Input) error
	Finalize(ctx context.Context, input review.Input) (bool, error)
	Discard(ctx context.Context, input review.Input) error
}

var (
	ErrReconcilerStopping = errors.New("review reconciler is stopping")
	ErrReconcileQueueFull = errors.New("review reconcile queue is full")
)

type ReconcilerConfig struct {
	NotesForProject func(projectID int64) review.NoteTemplates
	MatchesWorkflow func(projectID int64) bool
	WorkerCount     int
	QueueCapacity   int
	MaxConcurrent   int
	Publisher       ReviewPublisher
}

type LifecyclePhase string

const (
	LifecyclePhaseFind    LifecyclePhase = "find_session"
	LifecyclePhaseBuild   LifecyclePhase = "build_session_contract"
	LifecyclePhaseCreate  LifecyclePhase = "create_session"
	LifecyclePhaseRead    LifecyclePhase = "read_session_result"
	LifecyclePhaseCancel  LifecyclePhase = "cancel_run"
	LifecyclePhaseRecover LifecyclePhase = "recover_sessions"
	LifecyclePhaseGitLab  LifecyclePhase = "fetch_gitlab_state"
	LifecyclePhasePublish LifecyclePhase = "publish_review"
)

type LifecycleError struct {
	Phase     LifecyclePhase
	Code      string
	Retryable bool
	Err       error
}

func (e *LifecycleError) Error() string {
	return fmt.Sprintf("Orpheus lifecycle %s failed (%s): %v", e.Phase, e.Code, e.Err)
}

func (e *LifecycleError) Unwrap() error { return e.Err }

func IsRetryable(err error) bool {
	var lifecycleError *LifecycleError
	return errors.As(err, &lifecycleError) && lifecycleError.Retryable
}

// Reconciler owns snapshot comparison, eligibility, admission, and Orpheus
// lifecycle decisions for fetched review inputs. Polling remains the watcher's
// only responsibility.
type Reconciler struct {
	logger          *zap.Logger
	gitlab          gitLabReviewSource
	orpheus         orpheusAdapter
	buildContract   SessionContractBuilder
	publisher       ReviewPublisher
	matchesWorkflow func(projectID int64) bool
	notesForProject func(projectID int64) review.NoteTemplates
	workerCount     int
	maxConcurrent   int
	queue           chan review.Snapshot
	stateMu         sync.Mutex
	stopping        bool
	runCalled       bool
	stopOnce        sync.Once
	stop            chan struct{}
	started         chan struct{}
	done            chan struct{}
	cancelMu        sync.Mutex
	cancelRequests  context.CancelFunc
}

func NewReconciler(
	logger *zap.Logger,
	gitLabClient gitLabReviewSource,
	client orpheusAdapter,
	buildContract SessionContractBuilder,
	configs ...ReconcilerConfig,
) *Reconciler {
	cfg := ReconcilerConfig{WorkerCount: 1, QueueCapacity: 4, MaxConcurrent: 4}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 1
	}
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = cfg.WorkerCount
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}

	return &Reconciler{
		logger:          logger,
		gitlab:          gitLabClient,
		orpheus:         client,
		buildContract:   buildContract,
		publisher:       cfg.Publisher,
		matchesWorkflow: cfg.MatchesWorkflow,
		notesForProject: cfg.NotesForProject,
		workerCount:     cfg.WorkerCount,
		maxConcurrent:   cfg.MaxConcurrent,
		queue:           make(chan review.Snapshot, cfg.QueueCapacity),
		stop:            make(chan struct{}),
		started:         make(chan struct{}),
		done:            make(chan struct{}),
	}
}

func (r *Reconciler) Submit(ctx context.Context, snapshot review.Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot.Reviewer.ID <= 0 {
		return errors.New("submit review snapshot: reviewer ID must be positive")
	}
	copyOfSnapshot := review.Snapshot{Inputs: append([]review.Input(nil), snapshot.Inputs...)}
	copyOfSnapshot.Reviewer = snapshot.Reviewer
	seen := make(map[string]struct{}, len(copyOfSnapshot.Inputs))
	for _, input := range copyOfSnapshot.Inputs {
		if input.MRKey == "" {
			return errors.New("submit review snapshot: merge request key is required")
		}
		if input.Reviewer.ID != snapshot.Reviewer.ID {
			return fmt.Errorf("submit review snapshot: reviewer mismatch for %q", input.MRKey)
		}
		if _, exists := seen[input.MRKey]; exists {
			return fmt.Errorf("submit review snapshot: duplicate merge request key %q", input.MRKey)
		}
		seen[input.MRKey] = struct{}{}
	}

	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.stopping {
		return ErrReconcilerStopping
	}
	select {
	case r.queue <- copyOfSnapshot:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrReconcileQueueFull
	}
}

func (r *Reconciler) Run(ctx context.Context) error {
	r.stateMu.Lock()
	if r.runCalled {
		r.stateMu.Unlock()
		return errors.New("review reconciler can only be run once")
	}
	r.runCalled = true
	r.stateMu.Unlock()

	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	r.cancelMu.Lock()
	r.cancelRequests = cancelRequests
	r.cancelMu.Unlock()
	close(r.started)
	defer close(r.done)
	defer func() {
		cancelRequests()
		r.cancelMu.Lock()
		r.cancelRequests = nil
		r.cancelMu.Unlock()
	}()

	r.logger.Debug("starting review reconciler",
		zap.Int("worker_count", r.workerCount),
		zap.Int("queue_capacity", cap(r.queue)),
	)
	r.runSnapshots(requestCtx, ctx)

	return nil
}

func (r *Reconciler) Stop(ctx context.Context) error {
	r.logger.Debug("stopping review reconciler")
	r.beginStop()

	select {
	case <-r.started:
	case <-ctx.Done():
		r.logger.Debug("review reconciler shutdown timed out before start", zap.Error(ctx.Err()))
		return ctx.Err()
	}

	select {
	case <-r.done:
		r.logger.Debug("review reconciler stopped")
		return nil
	case <-ctx.Done():
		r.logger.Debug("review reconciler shutdown timed out; cancelling active requests", zap.Error(ctx.Err()))
		r.cancelMu.Lock()
		cancelRequests := r.cancelRequests
		r.cancelMu.Unlock()
		if cancelRequests != nil {
			cancelRequests()
		}
		return ctx.Err()
	}
}

func (r *Reconciler) beginStop() {
	r.stopOnce.Do(func() {
		r.stateMu.Lock()
		r.stopping = true
		r.stateMu.Unlock()
		close(r.stop)
	})
}

func (r *Reconciler) runSnapshots(requestCtx, appCtx context.Context) {
	for {
		select {
		case snapshot := <-r.queue:
			r.processSnapshot(requestCtx, snapshot)
		case <-appCtx.Done():
			r.beginStop()
			r.drainSnapshots(requestCtx)
			return
		case <-r.stop:
			r.drainSnapshots(requestCtx)
			return
		}
	}
}

func (r *Reconciler) drainSnapshots(ctx context.Context) {
	for {
		select {
		case snapshot := <-r.queue:
			r.processSnapshot(ctx, snapshot)
		default:
			return
		}
	}
}

func (r *Reconciler) processSnapshot(ctx context.Context, snapshot review.Snapshot) {
	if err := r.reconcileSnapshot(ctx, snapshot); err != nil {
		r.logLifecycleError("tick", review.Input{}, err)
	}
}

type candidateInspection struct {
	input  review.Input
	create bool
	err    error
}

func (r *Reconciler) reconcileSnapshot(ctx context.Context, snapshot review.Snapshot) error {
	current := make(map[string]review.Input, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		current[input.MRKey] = input
	}

	active, err := r.orpheus.ListActiveSessions(ctx, workflow.Namespace)
	if err != nil {
		return lifecycleError(LifecyclePhaseRecover, lifecycleCode(err), retryable(err), err)
	}
	activeByMR := make(map[string]struct{}, len(active))
	for _, session := range active {
		if _, duplicate := activeByMR[session.MRKey]; duplicate {
			err := lifecycleError(LifecyclePhaseRecover, "multiple_active_sessions", false, fmt.Errorf("multiple active sessions for %q", session.MRKey))
			r.logLifecycleError("recover", review.Input{MRKey: session.MRKey}, err)
			continue
		}
		activeByMR[session.MRKey] = struct{}{}
		if err := r.reconcileActiveSession(ctx, snapshot.Reviewer, current, session); err != nil {
			if IsRetryable(err) {
				return err
			}
			if input, exists := current[session.MRKey]; exists && r.publisher != nil {
				if handledErr := r.finishFailure(ctx, input, session, err); handledErr != nil {
					if IsRetryable(handledErr) {
						return handledErr
					}
					r.logLifecycleError("recover", input, handledErr)
				}
				continue
			}
			r.logLifecycleError("recover", review.Input{MRKey: session.MRKey}, err)
		}
	}

	admissionInputs := make([]review.Input, 0, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		if _, active := activeByMR[input.MRKey]; active {
			continue
		}
		admissionInputs = append(admissionInputs, input)
	}
	inspections := r.inspectCandidates(ctx, admissionInputs)
	for _, inspection := range inspections {
		if inspection.err != nil && IsRetryable(inspection.err) {
			return inspection.err
		}
	}

	activeCount := len(active)
	for _, inspection := range inspections {
		if inspection.err != nil {
			r.logLifecycleError("admission", inspection.input, inspection.err)
			continue
		}
		if !inspection.create {
			continue
		}
		if activeCount >= r.maxConcurrent {
			r.logger.Debug("review admission capacity is exhausted; deferring until the next poll",
				zap.String("merge_request_key", inspection.input.MRKey),
				zap.Int("active_reviews", activeCount),
				zap.Int("max_concurrent_reviews", r.maxConcurrent),
			)
			continue
		}
		if err := r.createSession(ctx, inspection.input); err != nil {
			handledErr := r.finishFailure(ctx, inspection.input, orpheus.Session{}, err)
			if handledErr != nil {
				if IsRetryable(handledErr) {
					return handledErr
				}
				r.logLifecycleError("admission", inspection.input, handledErr)
			}
			continue
		}
		activeCount++
	}

	return nil
}

func (r *Reconciler) inspectCandidates(ctx context.Context, inputs []review.Input) []candidateInspection {
	if len(inputs) == 0 {
		return nil
	}
	type indexedInput struct {
		index int
		input review.Input
	}
	jobs := make(chan indexedInput)
	results := make(chan struct {
		index      int
		inspection candidateInspection
	}, len(inputs))
	workerCount := min(r.workerCount, len(inputs))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for job := range jobs {
				create, err := r.inspectCandidate(ctx, job.input)
				results <- struct {
					index      int
					inspection candidateInspection
				}{job.index, candidateInspection{input: job.input, create: create, err: err}}
			}
		}()
	}
	for index, input := range inputs {
		jobs <- indexedInput{index: index, input: input}
	}
	close(jobs)
	workers.Wait()
	close(results)

	inspections := make([]candidateInspection, len(inputs))
	for result := range results {
		inspections[result.index] = result.inspection
	}
	return inspections
}

func (r *Reconciler) reconcileActiveSession(
	ctx context.Context,
	reviewer gitlab.User,
	current map[string]review.Input,
	session orpheus.Session,
) error {
	metadataRaw, err := r.orpheus.FindMessageMetadata(
		ctx,
		session.ID,
		session.RunID,
		workflow.MessageExternalKey(session.InputFingerprint),
	)
	if err != nil {
		return lifecycleError(LifecyclePhaseRecover, lifecycleCode(err), retryable(err), err)
	}
	metadata, err := workflow.DecodeMetadata(metadataRaw)
	if err != nil {
		return lifecycleError(LifecyclePhaseRecover, "invalid_session_metadata", false, err)
	}
	if err := workflow.ValidateRecoveredMetadata(metadata, session.MRKey, session.InputFingerprint, reviewer.ID); err != nil {
		return lifecycleError(LifecyclePhaseRecover, "session_metadata_mismatch", false, err)
	}

	input, exists := current[session.MRKey]
	if !exists {
		if r.gitlab == nil {
			return lifecycleError(LifecyclePhaseGitLab, "gitlab_source_not_configured", false, errors.New("GitLab review source is missing"))
		}
		source, err := r.gitlab.GetReviewInput(ctx, metadata.GitLab.ProjectID, metadata.GitLab.MergeRequestIID)
		if err != nil {
			return lifecycleError(LifecyclePhaseGitLab, "gitlab_request_failed", gitlab.IsRetryable(err), err)
		}
		input, err = review.NewInput(metadata.GitLab.Host, reviewer, source)
		if err != nil {
			return lifecycleError(LifecyclePhaseGitLab, "invalid_gitlab_state", false, err)
		}
	}

	reason := activeCancellationReason(input, metadata)
	fields := []zap.Field{
		zap.String("merge_request_key", session.MRKey),
		zap.String("review_fingerprint", session.InputFingerprint),
		zap.String("session_id", session.ID),
		zap.String("run_id", session.RunID),
		zap.String("run_status", session.Status),
	}
	if reason == "" {
		switch session.Status {
		case "completed", "failed", "cancelled":
			input.ReviewFingerprint = metadata.Review.ReviewFingerprint
			return r.reconcileSession(ctx, input, session)
		}
		r.logger.Debug("reconciled active Orpheus review session", fields...)
		return nil
	}
	fields = append(fields, zap.String("cancellation_reason", reason))
	if session.Status == "cancelling" {
		r.logger.Debug("Orpheus review session cancellation is already in progress", fields...)
	} else if session.Status == "completed" || session.Status == "failed" || session.Status == "cancelled" {
		r.logger.Debug("discarding stale terminal Orpheus review session", fields...)
	} else if err := r.orpheus.CancelRun(ctx, session.ID, session.RunID); err != nil {
		return lifecycleError(LifecyclePhaseCancel, lifecycleCode(err), retryable(err), err)
	} else {
		r.logger.Info("cancelled stale Orpheus review session", fields...)
	}
	if r.publisher != nil && (reason == "diff_changed" || reason == string(review.ReasonDiffRefsIncomplete)) {
		if err := r.publisher.Discard(ctx, input); err != nil {
			return publicationLifecycleError(err)
		}
	}

	return nil
}

func activeCancellationReason(input review.Input, metadata workflow.MetadataV1) string {
	eligibility := review.EvaluateInputEligibility(input)
	for _, reason := range eligibility.Reasons {
		switch reason {
		case review.ReasonMRNotOpen, review.ReasonReviewerNotAssigned, review.ReasonDiffRefsIncomplete:
			return string(reason)
		}
	}
	if input.DiffFingerprint != metadata.Review.DiffFingerprint {
		return "diff_changed"
	}
	return ""
}

func (r *Reconciler) logLifecycleError(operation string, input review.Input, err error) {
	if errors.Is(err, context.Canceled) {
		r.logger.Debug("review lifecycle operation cancelled",
			zap.String("operation", operation),
			zap.String("merge_request_key", input.MRKey),
		)
		return
	}
	fields := []zap.Field{
		zap.String("operation", operation),
		zap.String("merge_request_key", input.MRKey),
		zap.String("project_path", input.Project.PathWithNamespace),
		zap.Error(err),
	}
	var lifecycleError *LifecycleError
	if errors.As(err, &lifecycleError) {
		fields = append(fields,
			zap.String("lifecycle_phase", string(lifecycleError.Phase)),
			zap.String("lifecycle_error_code", lifecycleError.Code),
			zap.Bool("retryable", lifecycleError.Retryable),
		)
	}
	r.logger.Error("failed to process GitLab review lifecycle", fields...)
}

func (r *Reconciler) Reconcile(ctx context.Context, input review.Input) error {
	create, err := r.inspectCandidate(ctx, input)
	if err != nil || !create {
		return err
	}
	if err := r.createSession(ctx, input); err != nil {
		return r.finishFailure(ctx, input, orpheus.Session{}, err)
	}
	return nil
}

func (r *Reconciler) inspectCandidate(ctx context.Context, input review.Input) (bool, error) {
	eligibility := review.EvaluateInputEligibility(input)
	if !eligibility.Eligible {
		r.logger.Debug("GitLab merge request is not eligible for review",
			zap.String("merge_request_key", input.MRKey),
			zap.String("project_path", input.Project.PathWithNamespace),
			zap.Strings("reasons", ineligibilityReasons(eligibility.Reasons)),
		)
		return false, nil
	}
	if r.publisher != nil {
		finalized, err := r.publisher.Finalize(ctx, input)
		if err != nil {
			return false, r.finishFailure(ctx, input, orpheus.Session{}, publicationLifecycleError(err))
		}
		if finalized {
			return false, nil
		}
	}
	if r.orpheus == nil || r.buildContract == nil {
		return false, lifecycleError(LifecyclePhaseBuild, "reconciler_not_configured", false, errors.New("orpheus dependencies are missing"))
	}

	key := orpheus.ReviewSessionKey{
		Namespace:         workflow.Namespace,
		MRKey:             input.MRKey,
		ReviewFingerprint: input.ReviewFingerprint,
	}
	sessions, err := r.orpheus.FindSessions(ctx, key)
	if err != nil {
		return false, lifecycleError(LifecyclePhaseFind, lifecycleCode(err), retryable(err), err)
	}
	if len(sessions) > 1 {
		err := lifecycleError(LifecyclePhaseRead, "multiple_matching_sessions", false, fmt.Errorf("found %d exact sessions", len(sessions)))
		return false, r.finishFailure(ctx, input, orpheus.Session{}, err)
	}
	if len(sessions) == 1 {
		return false, r.reconcileSession(ctx, input, sessions[0])
	}
	latest, err := r.orpheus.FindLatestSession(ctx, workflow.Namespace, input.MRKey)
	if err != nil {
		return false, lifecycleError(LifecyclePhaseRecover, lifecycleCode(err), retryable(err), err)
	}
	if latest != nil && !latest.CreatedAt.Before(review.LatestAssignment(input)) {
		err := r.reconcileActiveSession(ctx, input.Reviewer, map[string]review.Input{input.MRKey: input}, *latest)
		if err != nil {
			return false, r.finishFailure(ctx, input, *latest, err)
		}
		return false, nil
	}

	if r.matchesWorkflow != nil && !r.matchesWorkflow(input.MergeRequest.ProjectID) {
		if r.publisher == nil {
			return false, lifecycleError(LifecyclePhasePublish, "publisher_not_configured", false, errors.New("skip publication requires a publisher"))
		}
		if err := r.publisher.PublishSkipped(ctx, input); err != nil {
			return false, publicationLifecycleError(err)
		}
		r.logger.Info("skipped review without a matching workflow", zap.String("merge_request_key", input.MRKey))
		return false, nil
	}
	return true, nil
}

func (r *Reconciler) createSession(ctx context.Context, input review.Input) error {
	contract, err := r.buildContract(input)
	if err != nil {
		code := "invalid_session_contract"
		var tooLarge *workflow.RequestTooLargeError
		if errors.As(err, &tooLarge) {
			code = "session_request_too_large"
		}
		return lifecycleError(LifecyclePhaseBuild, code, false, err)
	}
	accepted, err := r.orpheus.CreateSession(ctx, contract.Key, contract.ReviewerID, contract.Request)
	if err != nil {
		return lifecycleError(LifecyclePhaseCreate, lifecycleCode(err), retryable(err), err)
	}

	r.logger.Info("created Orpheus review session",
		zap.String("merge_request_key", input.MRKey),
		zap.String("review_fingerprint", input.ReviewFingerprint),
		zap.String("session_id", accepted.SessionID),
		zap.String("run_id", accepted.RunID),
	)

	return nil
}

func (r *Reconciler) reconcileSession(ctx context.Context, input review.Input, session orpheus.Session) error {
	fields := []zap.Field{
		zap.String("merge_request_key", input.MRKey),
		zap.String("review_fingerprint", input.ReviewFingerprint),
		zap.String("session_id", session.ID),
		zap.String("run_id", session.RunID),
		zap.String("run_status", session.Status),
	}

	switch session.Status {
	case "accepted", "starting", "running", "cancelling", "finalizing":
		r.logger.Debug("Orpheus review session is still in progress", fields...)
	case "completed":
		bundle, notes, err := r.validateCompletedSession(ctx, input, session)
		if err != nil {
			return r.finishFailure(ctx, input, session, err)
		}
		fields = append(fields,
			zap.Int("confirmed_findings", bundle.Counts.Confirmed),
			zap.Int("rejected_findings", bundle.Counts.Rejected),
			zap.Int("recommendations", bundle.Counts.Recommendations),
			zap.Int("resolutions", bundle.Counts.Resolutions),
		)
		if r.publisher == nil {
			r.logger.Info("validated Orpheus review bundle", fields...)
			return nil
		}
		if err := r.publisher.Publish(ctx, input, bundle, notes); err != nil {
			return r.finishFailure(ctx, input, session, publicationLifecycleError(err))
		}
		r.logger.Info("completed GitLab review publication", fields...)
	case "failed":
		fields = append(fields, zap.String("orpheus_error_code", session.ErrorCode))
		r.logger.Warn("Orpheus review session failed without analysis retry", fields...)
		if r.publisher != nil {
			return r.finishFailure(ctx, input, session, lifecycleError(LifecyclePhaseRead, "run_failed", false, errors.New("orpheus run failed")))
		}
	case "cancelled":
		r.logger.Warn("Orpheus review session was cancelled without analysis retry", fields...)
		if r.publisher != nil {
			return r.finishFailure(ctx, input, session, lifecycleError(LifecyclePhaseRead, "run_cancelled", false, errors.New("orpheus run was cancelled")))
		}
	default:
		return r.finishFailure(ctx, input, session,
			lifecycleError(LifecyclePhaseRead, "unknown_run_status", false, fmt.Errorf("unsupported run status %q", session.Status)))
	}

	return nil
}

func publicationLifecycleError(err error) error {
	return lifecycleError(LifecyclePhasePublish, publication.Code(err), publication.IsRetryable(err), err)
}

func (r *Reconciler) finishFailure(ctx context.Context, input review.Input, session orpheus.Session, err error) error {
	if errors.Is(err, publication.ErrReviewInactive) {
		return nil
	}
	if errors.Is(err, publication.ErrStaleReview) {
		if r.publisher == nil {
			return err
		}
		if discardErr := r.publisher.Discard(ctx, input); discardErr != nil {
			return publicationLifecycleError(discardErr)
		}
		return nil
	}
	if IsRetryable(err) || errors.Is(err, context.Canceled) || r.publisher == nil {
		return err
	}
	r.logLifecycleError("terminal", input, err)
	var failure *LifecycleError
	code := "review_failed"
	if errors.As(err, &failure) {
		code = failure.Code
	}
	notes, notesErr := r.failureNotes(ctx, input, session)
	if notesErr != nil {
		return notesErr
	}
	if publishErr := r.publisher.PublishError(ctx, input, publication.Failure{Code: code, SessionID: session.ID, Notes: notes}); publishErr != nil {
		if errors.Is(publishErr, publication.ErrReviewInactive) {
			return nil
		}
		if errors.Is(publishErr, publication.ErrStaleReview) {
			if discardErr := r.publisher.Discard(ctx, input); discardErr != nil {
				return publicationLifecycleError(discardErr)
			}
			return nil
		}
		return publicationLifecycleError(publishErr)
	}
	return nil
}

func (r *Reconciler) validateCompletedSession(ctx context.Context, input review.Input, session orpheus.Session) (protocol.Bundle, review.NoteTemplates, error) {
	if session.AgentStatus != "completed" {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(
			LifecyclePhaseRead,
			"agent_not_completed",
			false,
			fmt.Errorf("agent status is %q", session.AgentStatus),
		)
	}

	var afterRun *orpheus.HookResult
	for i := range session.Hooks {
		if session.Hooks[i].Name != "after_run" {
			continue
		}
		if afterRun != nil {
			return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, "multiple_after_run_results", false, errors.New("multiple after_run results"))
		}
		afterRun = &session.Hooks[i]
	}
	if afterRun == nil {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, "after_run_missing", false, errors.New("after_run result is missing"))
	}
	if afterRun.Status != "completed" || afterRun.ExitCode == nil || *afterRun.ExitCode != 0 {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, "after_run_failed", false, fmt.Errorf("after_run status is %q", afterRun.Status))
	}
	if afterRun.OutputCompleteness != "complete" || afterRun.TruncationReason != "" || afterRun.OutputType != "text" {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, "after_run_output_incomplete", false, errors.New("after_run output is not complete text"))
	}

	metadataRaw, err := r.orpheus.FindMessageMetadata(
		ctx,
		session.ID,
		session.RunID,
		workflow.MessageExternalKey(input.ReviewFingerprint),
	)
	if err != nil {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, lifecycleCode(err), retryable(err), err)
	}
	metadata, err := workflow.DecodeMetadata(metadataRaw)
	if err != nil {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, "invalid_session_metadata", false, err)
	}
	if err := workflow.ValidateMetadata(metadata, input); err != nil {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, "session_metadata_mismatch", false, err)
	}
	bundle, err := protocol.Decode([]byte(afterRun.Output), protocol.DefaultLimits(), workflow.ProtocolExpected(metadata))
	if err != nil {
		return protocol.Bundle{}, review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, protocol.ErrorCode(err), false, err)
	}

	notes := review.NoteTemplates{}
	if metadata.Notes != nil {
		notes = *metadata.Notes
	}
	return bundle, notes, nil
}

func lifecycleError(phase LifecyclePhase, code string, canRetry bool, err error) error {
	return &LifecycleError{Phase: phase, Code: code, Retryable: canRetry, Err: err}
}

func lifecycleCode(err error) string {
	if code := orpheus.Code(err); code != "" {
		return code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "request_cancelled"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return "network_error"
	}

	return "unexpected_error"
}

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var apiError *orpheus.Error
	if errors.As(err, &apiError) {
		return apiError.Status == http.StatusRequestTimeout || apiError.Status == http.StatusTooManyRequests || apiError.Status >= 500
	}
	if gitlab.IsRetryable(err) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func ineligibilityReasons(reasons []review.IneligibilityReason) []string {
	values := make([]string, len(reasons))
	for i, reason := range reasons {
		values[i] = string(reason)
	}

	return values
}

func (r *Reconciler) failureNotes(ctx context.Context, input review.Input, session orpheus.Session) (review.NoteTemplates, error) {
	if session.ID == "" {
		if r.notesForProject != nil {
			return r.notesForProject(input.MergeRequest.ProjectID), nil
		}
		return review.NoteTemplates{}, nil
	}
	raw, err := r.orpheus.FindMessageMetadata(ctx, session.ID, session.RunID, workflow.MessageExternalKey(input.ReviewFingerprint))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return review.NoteTemplates{}, err
		}
		if retryable(err) {
			return review.NoteTemplates{}, lifecycleError(LifecyclePhaseRead, lifecycleCode(err), true, err)
		}
		return review.NoteTemplates{}, nil
	}
	metadata, err := workflow.DecodeMetadata(raw)
	if err != nil || workflow.ValidateMetadata(metadata, input) != nil || metadata.Notes == nil {
		return review.NoteTemplates{}, nil
	}
	return *metadata.Notes, nil
}
