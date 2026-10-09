package workflow

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
)

//go:embed assets/prompt.md
var promptAsset string

//go:embed assets/review_pack.py
var helperAsset string

const revisionFormatVersion = "workflow-revision-v3"

func Revision(options Options) (string, error) {
	if strings.TrimSpace(options.Instructions) == "" {
		return "", errors.New("build workflow revision: instructions are required")
	}

	// Bind effective execution settings as well as user instructions and the protocol.
	settings, err := json.Marshal(struct {
		ID, Profile, Model, Template, Language string
		Services                               []string
		RunTimeout, HookTimeout                int
		Notes                                  review.NoteTemplates
	}{options.WorkflowID, options.AgentProfile, options.AgentModel, options.SandboxTemplate, options.Language, options.Services, options.RunTimeoutSeconds, options.HookTimeoutSeconds, options.Notes})
	if err != nil {
		return "", err
	}
	return revisionForParts(
		revisionFormatVersion,
		agentInstructions(options),
		string(settings),
		promptAsset,
		helperAsset,
		beforeRunTemplate,
		afterRunTemplate,
	), nil
}

func revisionForParts(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		writeRevisionPart(digest, part)
	}

	return fmt.Sprintf("sha256:%x", digest.Sum(nil))
}

func writeRevisionPart(digest hash.Hash, value string) {
	_, _ = fmt.Fprintf(digest, "%d:", len(value))
	_, _ = io.WriteString(digest, value)
}
