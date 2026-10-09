package review

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoteTemplatesRenderFixedPlaceholdersWithoutInterpretingValues(t *testing.T) {
	t.Parallel()
	text := "⚠️ {{ .Reason }}\n\n[Details]({{.SessionURL}})\nFindings: {{ .FindingsCount }}"
	output, err := RenderNote(text, NoteData{FindingsCount: 2, Reason: "unavailable {{ .SessionURL }}", SessionURL: "https://orpheus.example.test/sessions/1"})
	require.NoError(t, err)
	require.Equal(t, "⚠️ unavailable {{ .SessionURL }}\n\n[Details](https://orpheus.example.test/sessions/1)\nFindings: 2", output)
	for _, text := range []string{"{{ .Unknown }}", "{{ .Reason", "{{ printf \"secret\" }}", "{{ .Reason.Code }}", "{{if .Reason}}failed{{end}}"} {
		require.Error(t, (NoteTemplates{Fail: text}).Validate())
	}
	require.NoError(t, (NoteTemplates{}).Validate())
	require.NoError(t, (NoteTemplates{Success: "✅ Nothing found", Fail: "⚠️ Failed"}).Validate())
	_, err = RenderNote("{{ .SessionURL }}", NoteData{})
	require.ErrorContains(t, err, "blank")
}
