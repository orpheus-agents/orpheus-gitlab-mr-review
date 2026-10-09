package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadGitLabConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("APP_MODE", "dev")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("HTTP_TIMEOUT_SECONDS", "12")
	t.Setenv("POLL_INTERVAL_SECONDS", "15")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "8")
	t.Setenv("GITLAB_BASE_URL", " https://gitlab.example.com/root/ ")
	t.Setenv("GITLAB_TOKEN", " token ")
	t.Setenv("ORPHEUS_BASE_URL", " https://orpheus.example.com/api/ ")
	t.Setenv("ORPHEUS_WEB_BASE_URL", " https://orpheus.example.com/ui/ ")
	t.Setenv("ORPHEUS_API_KEY", " orpheus-token ")
	t.Setenv("WORKFLOWS_DIR", " /var/app/workflows ")
	t.Setenv("RUN_TIMEOUT_SECONDS", "3600")
	t.Setenv("HOOK_TIMEOUT_SECONDS", "120")
	t.Setenv("MAX_SESSION_REQUEST_BYTES", "1048576")
	t.Setenv("RECONCILE_WORKER_COUNT", "6")
	t.Setenv("RECONCILE_QUEUE_CAPACITY", "48")
	t.Setenv("MAX_CONCURRENT_REVIEWS", "9")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ModeDevelopment, cfg.Mode)
	require.Equal(t, "debug", cfg.LogLevel)
	require.Equal(t, 12*time.Second, cfg.HTTPTimeout)
	require.Equal(t, 15*time.Second, cfg.PollInterval)
	require.Equal(t, 8*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, time.Hour, cfg.RunTimeout)
	require.Equal(t, 2*time.Minute, cfg.HookTimeout)
	require.Equal(t, 1<<20, cfg.MaxSessionRequestBytes)
	require.Equal(t, 6, cfg.ReconcileWorkerCount)
	require.Equal(t, 48, cfg.ReconcileQueueCapacity)
	require.Equal(t, 9, cfg.MaxConcurrentReviews)
	require.Equal(t, "/var/app/workflows", cfg.WorkflowsDir)
	require.Equal(t, "https://gitlab.example.com/root", cfg.GitLab.BaseURL)
	require.Equal(t, "token", cfg.GitLab.Token)
	require.Equal(t, "https://orpheus.example.com/api", cfg.Orpheus.BaseURL)
	require.Equal(t, "https://orpheus.example.com/ui", cfg.Orpheus.WebBaseURL)
	require.Equal(t, "orpheus-token", cfg.Orpheus.APIKey)
}

func TestLoadRequiresOrpheusSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("ORPHEUS_BASE_URL", "")

	_, err := Load()
	require.EqualError(t, err, "ORPHEUS_BASE_URL is required")
}

func TestLoadRequiresGitLabSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "")
	t.Setenv("GITLAB_TOKEN", "")

	_, err := Load()
	require.EqualError(t, err, "GITLAB_BASE_URL is required")
}

func TestLoadWorkflowsDirectory(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("ORPHEUS_BASE_URL", "https://orpheus.example.com")
	t.Setenv("ORPHEUS_API_KEY", "key")
	t.Setenv("WORKFLOWS_DIR", "")
	require.NoError(t, os.Unsetenv("WORKFLOWS_DIR"))
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "workflows", cfg.WorkflowsDir)
	require.NoError(t, os.WriteFile(filepath.Join(directory, ".env"), []byte("WORKFLOWS_DIR=mounted\n"), 0o600))
	cfg, err = Load()
	require.NoError(t, err)
	require.Equal(t, "mounted", cfg.WorkflowsDir)
	t.Setenv("WORKFLOWS_DIR", " /override ")
	cfg, err = Load()
	require.NoError(t, err)
	require.Equal(t, "/override", cfg.WorkflowsDir)
	t.Setenv("WORKFLOWS_DIR", "")
	_, err = Load()
	require.EqualError(t, err, "WORKFLOWS_DIR is required")
}

func TestParseBaseURL(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value   string
		want    string
		wantErr string
	}{
		"https":        {value: "https://gitlab.example.com/", want: "https://gitlab.example.com"},
		"subpath":      {value: "https://gitlab.example.com/root/", want: "https://gitlab.example.com/root"},
		"missing host": {value: "https://", wantErr: "must include a host"},
		"credentials":  {value: "https://user:secret@gitlab.example.com", wantErr: "must not include credentials"},
		"query":        {value: "https://gitlab.example.com?x=1", wantErr: "must not include a query or fragment"},
		"scheme":       {value: "ftp://gitlab.example.com", wantErr: "must use http or https"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := parseBaseURL("GITLAB_BASE_URL", test.value)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestParsePositiveSeconds(t *testing.T) {
	t.Parallel()

	got, err := parsePositiveSeconds("HTTP_TIMEOUT_SECONDS", "2")
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, got)

	_, err = parsePositiveSeconds("HTTP_TIMEOUT_SECONDS", "0")
	require.EqualError(t, err, "HTTP_TIMEOUT_SECONDS must be positive")

	_, err = parsePositiveSeconds("HTTP_TIMEOUT_SECONDS", "2s")
	require.ErrorContains(t, err, "parse HTTP_TIMEOUT_SECONDS")
}

func TestParsePositiveIntEnforcesMaximum(t *testing.T) {
	t.Parallel()

	value, err := parsePositiveInt("LIMIT", "10", 10)
	require.NoError(t, err)
	require.Equal(t, 10, value)

	_, err = parsePositiveInt("LIMIT", "11", 10)
	require.EqualError(t, err, "LIMIT must not exceed 10")
}

func TestLoadRejectsInvalidPollInterval(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("POLL_INTERVAL_SECONDS", "0")

	_, err := Load()
	require.EqualError(t, err, "POLL_INTERVAL_SECONDS must be positive")
}

func TestLoadRejectsInvalidShutdownTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "0")

	_, err := Load()
	require.EqualError(t, err, "SHUTDOWN_TIMEOUT_SECONDS must be positive")
}
