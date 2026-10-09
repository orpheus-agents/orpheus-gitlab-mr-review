package review

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NoteTemplates are user-owned publication text, independent of agent instructions.
type NoteTemplates struct {
	Success string `yaml:"success" json:"success,omitempty"`
	Fail    string `yaml:"fail" json:"fail,omitempty"`
}

type NoteData struct {
	FindingsCount int
	Reason        string
	SessionURL    string
}

var notePlaceholder = regexp.MustCompile(`{{\s*\.(FindingsCount|Reason|SessionURL)\s*}}`)

func (n NoteTemplates) Empty() bool { return n == (NoteTemplates{}) }

func (n NoteTemplates) Validate() error {
	for _, field := range []struct{ name, text string }{{"success", n.Success}, {"fail", n.Fail}} {
		if field.text == "" {
			continue
		}
		data := NoteData{}
		if field.name == "fail" {
			data.Reason = "failure reason"
		}
		if _, err := RenderNote(field.text, data); err != nil {
			return fmt.Errorf("notes.%s: %w", field.name, err)
		}
	}
	return nil
}

func validateNoteText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("note must not be blank")
	}
	rest := notePlaceholder.ReplaceAllString(text, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return errors.New("unsupported placeholder; use .FindingsCount, .Reason or .SessionURL")
	}
	return nil
}

// RenderNote substitutes fixed placeholders once, without interpreting data as a template.
func RenderNote(text string, data NoteData) (string, error) {
	if err := validateNoteText(text); err != nil {
		return "", err
	}
	rendered := notePlaceholder.ReplaceAllStringFunc(text, func(token string) string {
		switch notePlaceholder.FindStringSubmatch(token)[1] {
		case "FindingsCount":
			return strconv.Itoa(data.FindingsCount)
		case "Reason":
			return data.Reason
		case "SessionURL":
			return data.SessionURL
		default:
			return ""
		}
	})
	if strings.TrimSpace(rendered) == "" {
		return "", errors.New("rendered note is blank")
	}
	return rendered, nil
}
