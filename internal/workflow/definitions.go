package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"go.yaml.in/yaml/v3"
)

// Definition contains user-owned execution settings and review instructions.
type Definition struct {
	ID              string               `yaml:"id"`
	ProjectIDs      []int64              `yaml:"project_ids"`
	Profile         string               `yaml:"profile"`
	Model           string               `yaml:"model"`
	SandboxTemplate string               `yaml:"sandbox_template"`
	Services        []string             `yaml:"services"`
	Language        string               `yaml:"language"`
	Notes           review.NoteTemplates `yaml:"notes"`
	Instructions    string               `yaml:"-"`
}

// Set is an immutable collection with unambiguous project ownership.
type Set struct {
	definitions  []Definition
	projects     map[int64]int
	defaultIndex int
}

var workflowIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)
var serviceCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var languagePattern = regexp.MustCompile(`^[A-Za-z]{2,8}(?:-[A-Za-z0-9]{1,8})*$`)

func ValidID(id string) bool { return workflowIDPattern.MatchString(id) }

// Load reads only root-level Markdown files, in filename order, at startup.
// A failure returns no partial configuration. maxBytes bounds each file.
func Load(directory string, maxBytes int) (Set, error) {
	if strings.TrimSpace(directory) == "" {
		return Set{}, errors.New("load workflows: directory is required")
	}
	if maxBytes <= 0 {
		return Set{}, errors.New("load workflows: maximum size must be positive")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return Set{}, fmt.Errorf("load workflows directory %q: %w", directory, err)
	}
	set := Set{projects: map[int64]int{}, defaultIndex: -1}
	ids := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "#") || strings.ToLower(filepath.Ext(name)) != ".md" {
			continue
		}
		path := filepath.Join(directory, name)
		content, err := readDefinition(path, maxBytes)
		if err != nil {
			return Set{}, err
		}
		definition, err := parseDefinition(content)
		if err != nil {
			return Set{}, fmt.Errorf("workflow %q: %w", path, err)
		}
		if previous, exists := ids[definition.ID]; exists {
			return Set{}, fmt.Errorf("workflow %q: duplicate id %q (already defined in %q)", path, definition.ID, previous)
		}
		ids[definition.ID] = path
		index := len(set.definitions)
		if len(definition.ProjectIDs) == 0 {
			if set.defaultIndex >= 0 {
				return Set{}, fmt.Errorf("workflow %q: multiple default workflows (%s and %s)", path, set.definitions[set.defaultIndex].ID, definition.ID)
			}
			set.defaultIndex = index
		}
		for _, id := range definition.ProjectIDs {
			if previous, exists := set.projects[id]; exists {
				return Set{}, fmt.Errorf("workflow %q: project_id %d is already assigned to workflow %s", path, id, set.definitions[previous].ID)
			}
			set.projects[id] = index
		}
		set.definitions = append(set.definitions, definition)
	}
	if len(set.definitions) == 0 {
		return Set{}, errors.New("load workflows: at least one Markdown workflow is required")
	}
	return set, nil
}

func (s Set) Select(projectID int64) (Definition, bool) {
	if len(s.definitions) == 0 || projectID <= 0 {
		return Definition{}, false
	}
	index, found := s.projects[projectID]
	if !found {
		index = s.defaultIndex
	}
	if index < 0 {
		return Definition{}, false
	}
	definition := s.definitions[index]
	definition.ProjectIDs = slices.Clone(definition.ProjectIDs)
	definition.Services = slices.Clone(definition.Services)
	return definition, true
}

func (d Definition) Options(base Options) Options {
	base.WorkflowID = d.ID
	base.Instructions = d.Instructions
	base.AgentProfile = d.Profile
	base.AgentModel = d.Model
	base.SandboxTemplate = d.SandboxTemplate
	base.Services = slices.Clone(d.Services)
	base.Language = d.Language
	base.Notes = d.Notes
	return base
}

func readDefinition(path string, maxBytes int) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect workflow %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workflow %q: not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect workflow %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workflow %q: not a regular file", path)
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read workflow %q: %w", path, err)
	}
	if len(content) > maxBytes {
		return nil, fmt.Errorf("workflow %q: content exceeds %d byte limit", path, maxBytes)
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("workflow %q: file is not UTF-8", path)
	}
	return content, nil
}

func parseDefinition(content []byte) (Definition, error) {
	var definition Definition
	body, err := decodeFrontMatter(content, &definition)
	if err != nil {
		return Definition{}, err
	}
	definition.Instructions = body
	if err := definition.validate(); err != nil {
		return Definition{}, err
	}
	return definition, nil
}

func (s Set) NotesForProject(projectID int64) review.NoteTemplates {
	definition, _ := s.Select(projectID)
	return definition.Notes
}

func decodeFrontMatter(content []byte, target any) (string, error) {
	// Preserve the Markdown body verbatim, including line endings and blank lines.
	lines := bytes.SplitAfter(content, []byte("\n"))
	lineValue := func(line []byte) string { return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r") }
	if lineValue(lines[0]) != "---" {
		return "", errors.New("YAML front matter is required")
	}
	for i := 1; i < len(lines); i++ {
		if lineValue(lines[i]) != "---" {
			continue
		}
		decoder := yaml.NewDecoder(bytes.NewReader(bytes.Join(lines[1:i], nil)))
		decoder.KnownFields(true)
		if err := decoder.Decode(target); err != nil {
			return "", fmt.Errorf("invalid YAML front matter: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return "", errors.New("front matter must contain exactly one YAML document")
		}
		// YAML null must not silently turn an explicit selector into a fallback.
		var node yaml.Node
		if err := yaml.Unmarshal(bytes.Join(lines[1:i], nil), &node); err != nil {
			return "", fmt.Errorf("invalid YAML front matter: %w", err)
		}
		if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
			return "", errors.New("front matter must be a mapping")
		}
		fields := node.Content[0].Content
		for j := 0; j < len(fields); j += 2 {
			key, value := fields[j].Value, fields[j+1]
			switch key {
			case "notes":
				if value.Kind != yaml.MappingNode {
					return "", errors.New("notes must be a mapping")
				}
				for k := 0; k < len(value.Content); k += 2 {
					noteValue := value.Content[k+1]
					if noteValue.Tag != "!!str" || strings.TrimSpace(noteValue.Value) == "" {
						return "", fmt.Errorf("notes.%s must be a non-empty string", value.Content[k].Value)
					}
				}
			case "id", "profile", "model", "sandbox_template", "language":
				if value.Tag != "!!str" {
					return "", fmt.Errorf("%s must be a string", key)
				}
			case "project_ids", "services":
				if value.Kind != yaml.SequenceNode {
					return "", fmt.Errorf("%s must be a list", key)
				}
				for _, item := range value.Content {
					if key == "services" && item.Tag != "!!str" {
						return "", errors.New("services must contain strings")
					}
					if key == "project_ids" && item.Tag != "!!int" {
						return "", errors.New("project_ids must contain integers")
					}
				}
			}
		}
		return string(bytes.Join(lines[i+1:], nil)), nil
	}
	return "", errors.New("YAML front matter is not closed")
}

func (d *Definition) validate() error {
	if err := d.Notes.Validate(); err != nil {
		return err
	}
	if !ValidID(d.ID) {
		return errors.New("id is required and must match [a-z0-9][a-z0-9_-]* (at most 80 characters)")
	}
	d.Profile = strings.TrimSpace(d.Profile)
	d.Model = strings.TrimSpace(d.Model)
	d.SandboxTemplate = strings.TrimSpace(d.SandboxTemplate)
	if d.Profile == "" {
		return errors.New("profile is required")
	}
	if d.SandboxTemplate == "" {
		return errors.New("sandbox_template is required")
	}
	if d.Services == nil {
		return errors.New("services is required; use [] when no services are needed")
	}
	seenServices := map[string]bool{}
	for _, code := range d.Services {
		if !serviceCodePattern.MatchString(code) || seenServices[code] {
			return errors.New("services must contain unique valid service codes")
		}
		seenServices[code] = true
	}
	if d.ProjectIDs != nil && len(d.ProjectIDs) == 0 {
		return errors.New("project_ids must be non-empty when present; omit it for a default workflow")
	}
	seenProjects := map[int64]bool{}
	for _, id := range d.ProjectIDs {
		if id <= 0 || seenProjects[id] {
			return errors.New("project_ids must contain unique positive IDs")
		}
		seenProjects[id] = true
	}
	if d.Language != "" && !languagePattern.MatchString(d.Language) {
		return errors.New("language must be a language tag, such as en, ru or pt-BR")
	}
	if strings.TrimSpace(d.Instructions) == "" {
		return errors.New("markdown instructions are required")
	}
	return nil
}
