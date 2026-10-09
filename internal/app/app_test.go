package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type serviceStub struct {
	run  func(ctx context.Context) error
	stop func(ctx context.Context) error
}

func TestNewRegistersIndependentWatcherAndReconcilerServices(t *testing.T) {
	instructionsPath := filepath.Join(t.TempDir(), "instructions.md")
	require.NoError(t, os.WriteFile(instructionsPath, []byte("---\nid: project-review\nprofile: review-profile\nsandbox_template: review-sandbox\nservices: []\n---\n# Project policy\n\nReview the pinned diff.\n"), 0o600))
	application, err := New(config.Config{
		WorkflowsDir:           filepath.Dir(instructionsPath),
		LogLevel:               "debug",
		HTTPTimeout:            time.Second,
		PollInterval:           time.Minute,
		ShutdownTimeout:        time.Second,
		RunTimeout:             time.Hour,
		HookTimeout:            time.Minute,
		MaxSessionRequestBytes: 1 << 20,
		ReconcileWorkerCount:   3,
		ReconcileQueueCapacity: 12,
		MaxConcurrentReviews:   5,
		GitLab: config.GitLab{
			BaseURL: "https://gitlab.example.com",
			Token:   "gitlab-token",
		},
		Orpheus: config.Orpheus{
			BaseURL: "https://orpheus.example.com",
			APIKey:  "orpheus-token",
		},
	})
	require.NoError(t, err)
	t.Cleanup(application.Close)

	require.Len(t, application.services, 2)
	require.Equal(t, "review reconciler", application.services[0].name)
	require.Equal(t, "merge request watcher", application.services[1].name)
}

func (s serviceStub) Run(ctx context.Context) error {
	return s.run(ctx)
}

func (s serviceStub) Stop(ctx context.Context) error {
	return s.stop(ctx)
}

func TestRunStartsAndStopsAllServices(t *testing.T) {
	started := make(chan string, 2)
	stopped := make(chan string, 2)
	newService := func(name string) serviceStub {
		return serviceStub{
			run: func(ctx context.Context) error {
				started <- name
				<-ctx.Done()
				return nil
			},
			stop: func(ctx context.Context) error {
				require.NoError(t, ctx.Err())
				stopped <- name
				return nil
			},
		}
	}
	application := &App{
		logger: zap.NewNop(),
		services: []namedService{
			{name: "first", service: newService("first")},
			{name: "second", service: newService("second")},
		},
		shutdownTimeout: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- application.Run(ctx)
	}()

	require.ElementsMatch(t, []string{"first", "second"}, []string{<-started, <-started})
	cancel()

	require.NoError(t, <-done)
	require.ElementsMatch(t, []string{"first", "second"}, []string{<-stopped, <-stopped})
}

func TestRunStopsAllServicesAfterRunError(t *testing.T) {
	wantErr := errors.New("run failed")
	stopped := make(chan string, 2)
	application := &App{
		logger: zap.NewNop(),
		services: []namedService{
			{
				name: "failed",
				service: serviceStub{
					run: func(context.Context) error { return wantErr },
					stop: func(context.Context) error {
						stopped <- "failed"
						return nil
					},
				},
			},
			{
				name: "peer",
				service: serviceStub{
					run: func(ctx context.Context) error {
						<-ctx.Done()
						return nil
					},
					stop: func(context.Context) error {
						stopped <- "peer"
						return nil
					},
				},
			},
		},
		shutdownTimeout: time.Second,
	}

	err := application.Run(context.Background())

	require.ErrorIs(t, err, wantErr)
	require.ErrorContains(t, err, "run failed")
	require.ElementsMatch(t, []string{"failed", "peer"}, []string{<-stopped, <-stopped})
}
