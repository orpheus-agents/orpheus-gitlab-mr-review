package connector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

type gitLabClientStub struct {
	currentUserErr   error
	listErr          error
	mergeRequests    []gitlab.MergeRequest
	reviewInput      gitlab.ReviewInput
	reviewInputErr   error
	listCalls        int
	reviewInputCalls int
	listStarted      chan struct{}
	listRelease      chan struct{}
	requestCanceled  chan struct{}
}

type reviewInputSinkStub struct {
	snapshots []review.Snapshot
	err       error
}

func (s *reviewInputSinkStub) Submit(_ context.Context, snapshot review.Snapshot) error {
	s.snapshots = append(s.snapshots, snapshot)
	return s.err
}

func (s *gitLabClientStub) CurrentUser(context.Context) (gitlab.User, error) {
	if s.currentUserErr != nil {
		return gitlab.User{}, s.currentUserErr
	}

	return gitlab.User{ID: 42, Username: "reviewer"}, nil
}

func (s *gitLabClientStub) ListMergeRequestsForReview(ctx context.Context, _ int64) ([]gitlab.MergeRequest, error) {
	s.listCalls++
	if s.listStarted != nil {
		close(s.listStarted)
	}
	if s.listRelease != nil {
		select {
		case <-s.listRelease:
		case <-ctx.Done():
			if s.requestCanceled != nil {
				close(s.requestCanceled)
			}
			return nil, ctx.Err()
		}
	}
	return s.mergeRequests, s.listErr
}

func (s *gitLabClientStub) GetReviewInput(context.Context, int64, int64) (gitlab.ReviewInput, error) {
	s.reviewInputCalls++
	return s.reviewInput, s.reviewInputErr
}

func newTestWatcher(logger *zap.Logger, client gitLabClient) *Watcher {
	return NewWatcher(logger, client, &reviewInputSinkStub{}, WatcherConfig{
		PollInterval: time.Hour,
		GitLabHost:   "https://gitlab.example.com",
	})
}

func TestWatcherRunPollsImmediately(t *testing.T) {
	started := make(chan struct{})
	client := &gitLabClientStub{listStarted: started}
	watcher := newTestWatcher(zaptest.NewLogger(t), client)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watcher.Run(ctx)
	}()
	<-started
	cancel()

	err := <-done

	require.NoError(t, err)
	require.Equal(t, 1, client.listCalls)
}

func TestWatcherRunFailsWhenReviewerCannotBeResolved(t *testing.T) {
	client := &gitLabClientStub{currentUserErr: errors.New("request failed")}
	watcher := newTestWatcher(zaptest.NewLogger(t), client)

	err := watcher.Run(context.Background())

	require.EqualError(t, err, "resolve authenticated GitLab reviewer: request failed")
	require.Zero(t, client.listCalls)
}

func TestWatcherPollErrorDoesNotStopRun(t *testing.T) {
	started := make(chan struct{})
	client := &gitLabClientStub{listErr: errors.New("request failed"), listStarted: started}
	watcher := newTestWatcher(zaptest.NewLogger(t), client)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watcher.Run(ctx)
	}()
	<-started
	cancel()

	err := <-done

	require.NoError(t, err)
	require.Equal(t, 1, client.listCalls)
}

func TestWatcherPollFetchesAndPassesCompleteSnapshotToReconciler(t *testing.T) {
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	client := &gitLabClientStub{
		mergeRequests: []gitlab.MergeRequest{{ProjectID: 74, IID: 2989}},
		reviewInput: gitlab.ReviewInput{
			MergeRequest: gitlab.MergeRequest{
				ProjectID:    74,
				IID:          2989,
				State:        "opened",
				SourceBranch: "feature",
				TargetBranch: "main",
				Reviewers:    []gitlab.User{reviewer},
				DiffRefs: gitlab.DiffRefs{
					BaseSHA:  "base",
					StartSHA: "start",
					HeadSHA:  "head",
				},
			},
			Project: gitlab.Project{ID: 74, PathWithNamespace: "team/project"},
		},
	}
	inputSink := &reviewInputSinkStub{}
	watcher := NewWatcher(zaptest.NewLogger(t), client, inputSink, WatcherConfig{
		PollInterval: time.Hour,
		GitLabHost:   "https://gitlab.example.com/",
	})

	watcher.poll(context.Background(), reviewer)

	require.Equal(t, 1, client.reviewInputCalls)
	require.Len(t, inputSink.snapshots, 1)
	require.Equal(t, reviewer, inputSink.snapshots[0].Reviewer)
	require.Len(t, inputSink.snapshots[0].Inputs, 1)
	require.Equal(t, "https://gitlab.example.com:74!2989", inputSink.snapshots[0].Inputs[0].MRKey)
	require.NotEmpty(t, inputSink.snapshots[0].Inputs[0].DiffFingerprint)
	require.NotEmpty(t, inputSink.snapshots[0].Inputs[0].ReviewFingerprint)
}

func TestWatcherDoesNotSubmitPartialSnapshotWhenInputFetchFails(t *testing.T) {
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	client := &gitLabClientStub{
		mergeRequests:  []gitlab.MergeRequest{{ProjectID: 74, IID: 2989}},
		reviewInputErr: errors.New("request failed"),
	}
	inputSink := &reviewInputSinkStub{}
	watcher := NewWatcher(zaptest.NewLogger(t), client, inputSink, WatcherConfig{
		PollInterval: time.Hour,
		GitLabHost:   "https://gitlab.example.com",
	})

	watcher.poll(context.Background(), reviewer)

	require.Empty(t, inputSink.snapshots)
}

func TestWatcherDefersFullQueueWithoutStoppingPoll(t *testing.T) {
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	client := &gitLabClientStub{
		mergeRequests: []gitlab.MergeRequest{
			{ProjectID: 74, IID: 2989},
			{ProjectID: 74, IID: 2990},
		},
		reviewInput: gitlab.ReviewInput{
			MergeRequest: gitlab.MergeRequest{
				ProjectID: 74,
				IID:       2989,
				State:     "opened",
				Reviewers: []gitlab.User{reviewer},
				DiffRefs:  gitlab.DiffRefs{BaseSHA: "base", StartSHA: "start", HeadSHA: "head"},
			},
			Project: gitlab.Project{ID: 74, PathWithNamespace: "team/project"},
		},
	}
	core, logs := observer.New(zap.DebugLevel)
	watcher := NewWatcher(zap.New(core), client, &reviewInputSinkStub{err: ErrReconcileQueueFull}, WatcherConfig{
		PollInterval: time.Hour,
		GitLabHost:   "https://gitlab.example.com",
	})

	watcher.poll(context.Background(), reviewer)

	require.Equal(t, 2, client.reviewInputCalls)
	require.Equal(t, 1, logs.FilterMessage("review reconcile queue is full; deferring snapshot until the next poll").Len())
}

func TestWatcherLetsInFlightPollFinishDuringShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := &gitLabClientStub{listStarted: started, listRelease: release}
	core, logs := observer.New(zap.DebugLevel)
	watcher := newTestWatcher(zap.New(core), client)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watcher.Run(ctx)
	}()

	<-started
	cancel()
	stopDone := make(chan error, 1)
	go func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
		defer cancelShutdown()
		stopDone <- watcher.Stop(shutdownCtx)
	}()
	select {
	case err := <-stopDone:
		t.Fatalf("watcher stopped before the in-flight poll completed: %v", err)
	default:
	}
	close(release)

	require.NoError(t, <-stopDone)
	require.NoError(t, <-done)
	require.Equal(t, 1, logs.FilterMessage("stopping merge request watcher").Len())
	require.Equal(t, 1, logs.FilterMessage("merge request watcher stopped").Len())
}

func TestWatcherCancelsInFlightPollAfterShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	requestCanceled := make(chan struct{})
	client := &gitLabClientStub{
		listStarted:     started,
		listRelease:     release,
		requestCanceled: requestCanceled,
	}
	core, logs := observer.New(zap.DebugLevel)
	watcher := newTestWatcher(zap.New(core), client)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watcher.Run(ctx)
	}()

	<-started
	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelShutdown()
	require.ErrorIs(t, watcher.Stop(shutdownCtx), context.DeadlineExceeded)

	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight poll context was not cancelled")
	}
	require.NoError(t, <-done)
	require.Equal(t, 1, logs.FilterMessage("stopping merge request watcher").Len())
	require.Equal(t, 1, logs.FilterMessage("merge request watcher shutdown timed out; cancelling active requests").Len())
}
