package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/connector"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/observability"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/publication"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/workflow"

	"go.uber.org/zap"
)

type App struct {
	logger          *zap.Logger
	services        []namedService
	shutdownTimeout time.Duration
}

type service interface {
	Run(ctx context.Context) error
	Stop(ctx context.Context) error
}

type namedService struct {
	name    string
	service service
}

type serviceResult struct {
	name string
	err  error
}

func New(cfg config.Config) (*App, error) {
	logger, err := observability.NewLogger(cfg.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("create logger: %w", err)
	}

	gitLabClient, err := gitlab.New(cfg)
	if err != nil {
		_ = logger.Sync()
		return nil, err
	}
	orpheusClient, err := orpheus.New(cfg.Orpheus.BaseURL, cfg.Orpheus.APIKey, cfg.HTTPTimeout)
	if err != nil {
		_ = logger.Sync()
		return nil, err
	}
	workflows, err := workflow.Load(cfg.WorkflowsDir, cfg.MaxSessionRequestBytes)
	if err != nil {
		_ = logger.Sync()
		return nil, err
	}
	contractOptions := workflow.Options{
		GitLabHost:         cfg.GitLab.BaseURL,
		RunTimeoutSeconds:  int(cfg.RunTimeout / time.Second),
		HookTimeoutSeconds: int(cfg.HookTimeout / time.Second),
		MaxRequestBytes:    cfg.MaxSessionRequestBytes,
	}

	reconciler := connector.NewReconciler(
		logger,
		gitLabClient,
		orpheusClient,
		func(input review.Input) (workflow.SessionContract, error) {
			definition, ok := workflows.Select(input.MergeRequest.ProjectID)
			if !ok {
				return workflow.SessionContract{}, errors.New("no workflow configured for project")
			}
			return workflow.BuildSessionContract(input, definition.Options(contractOptions))
		},
		connector.ReconcilerConfig{
			NotesForProject: workflows.NotesForProject,
			MatchesWorkflow: func(projectID int64) bool { _, ok := workflows.Select(projectID); return ok },
			WorkerCount:     cfg.ReconcileWorkerCount,
			QueueCapacity:   cfg.ReconcileQueueCapacity,
			MaxConcurrent:   cfg.MaxConcurrentReviews,
			Publisher:       publication.New(logger, gitLabClient, cfg.GitLab.BaseURL, cfg.Orpheus.WebBaseURL),
		},
	)
	watcher := connector.NewWatcher(logger, gitLabClient, reconciler, connector.WatcherConfig{
		PollInterval: cfg.PollInterval,
		GitLabHost:   cfg.GitLab.BaseURL,
	})

	return &App{
		logger: logger,
		services: []namedService{
			{name: "review reconciler", service: reconciler},
			{name: "merge request watcher", service: watcher},
		},
		shutdownTimeout: cfg.ShutdownTimeout,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	appCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runResults := make(chan serviceResult, len(a.services))
	for _, item := range a.services {
		go func() {
			runResults <- serviceResult{name: item.name, err: item.service.Run(appCtx)}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case result := <-runResults:
		if result.err != nil {
			runErr = fmt.Errorf("run %s: %w", result.name, result.err)
		}
	}
	cancel()

	a.logger.Debug("starting graceful shutdown",
		zap.Int("service_count", len(a.services)),
		zap.Int64("shutdown_timeout_seconds", int64(a.shutdownTimeout/time.Second)),
	)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), a.shutdownTimeout)
	defer cancelShutdown()

	stopErr := a.stopServices(shutdownCtx)
	if stopErr == nil {
		a.logger.Debug("graceful shutdown completed")
	}

	return errors.Join(runErr, stopErr)
}

func (a *App) stopServices(ctx context.Context) error {
	stopResults := make(chan serviceResult, len(a.services))
	for _, item := range a.services {
		go func() {
			stopResults <- serviceResult{name: item.name, err: item.service.Stop(ctx)}
		}()
	}

	var errs []error
	for range a.services {
		select {
		case result := <-stopResults:
			if result.err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", result.name, result.err))
			}
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("stop services: %w", ctx.Err()))
			return errors.Join(errs...)
		}
	}

	return errors.Join(errs...)
}

func (a *App) Close() {
	_ = a.logger.Sync()
}
