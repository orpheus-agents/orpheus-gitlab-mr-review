package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/stretchr/testify/require"
)

func TestWorkflowNotesLoadAndFreezeInSessionMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := testFrontMatter + "notes:\n  success: '✅ Завершено. Находок: {{ .FindingsCount }}.'\n  fail: '⚠️ Не удалось завершить: {{ .Reason }}.'\n---\nInstructions"
	writeWorkflow(t, dir, "review.md", content)
	workflows, err := Load(dir, 4096)
	require.NoError(t, err)
	definition, ok := workflows.Select(10)
	require.True(t, ok)
	require.Contains(t, definition.Notes.Success, "Завершено")
	contract, err := BuildSessionContract(contractReviewInput(), definition.Options(contractOptions()))
	require.NoError(t, err)
	require.Equal(t, &definition.Notes, contract.Metadata.Notes)
	raw := *contract.Request.Messages[0].Metadata
	decoded, err := DecodeMetadata(raw)
	require.NoError(t, err)
	require.Equal(t, contract.Metadata.Notes, decoded.Notes)
	require.NoError(t, ValidateMetadata(decoded, contractReviewInput()))
	require.Equal(t, "Instructions", *contract.Request.Configuration.Agent.Instructions, "publication notes are not agent instructions")
	writeWorkflow(t, dir, "review.md", strings.Replace(content, "Завершено", "Изменено", 1))
	updated, err := Load(dir, 4096)
	require.NoError(t, err)
	changed, _ := updated.Select(10)
	changedContract, err := BuildSessionContract(contractReviewInput(), changed.Options(contractOptions()))
	require.NoError(t, err)
	require.NotEqual(t, contract.WorkflowRevision, changedContract.WorkflowRevision)
	require.Equal(t, contract.Key, changedContract.Key)
	require.Contains(t, decoded.Notes.Success, "Завершено")
	// Metadata produced before configurable notes were introduced remains valid.
	legacy := contract.Metadata
	legacy.Notes = nil
	raw, err = json.Marshal(legacy)
	require.NoError(t, err)
	decoded, err = DecodeMetadata(raw)
	require.NoError(t, err)
	require.Nil(t, decoded.Notes)
	require.NoError(t, ValidateMetadata(decoded, contractReviewInput()))
	decoded.Notes = &review.NoteTemplates{Fail: "{{ .Unknown }}"}
	require.ErrorContains(t, ValidateMetadata(decoded, contractReviewInput()), "unsupported placeholder")
}

func TestWorkflowNotesRejectBadMetadataAndTemplatesAtStartup(t *testing.T) {
	t.Parallel()
	for _, notes := range []string{
		"notes: {success: '{{ .Reason }}'}", "notes: {fail: '{{ .SessionURL }}'}", "notes: {skip: skipped}", "skip: true", "notes: null", "notes: [success]", "notes: {unknown: text}", "notes: {success: true}", "notes: {fail: ''}", "notes: {success: ' '}", "notes: {fail: '{{ .Unknown }}'}", "notes: {success: '{{ .FindingsCount'}", "notes: {success: first, success: second}",
	} {
		t.Run(notes, func(t *testing.T) {
			dir := t.TempDir()
			writeWorkflow(t, dir, "bad.md", testFrontMatter+notes+"\n---\nInstructions")
			_, err := Load(dir, 4096)
			require.ErrorContains(t, err, "bad.md")
		})
	}
}
