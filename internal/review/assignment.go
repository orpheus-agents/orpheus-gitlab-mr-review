package review

import (
	"fmt"
	"strings"
	"time"
)

// LatestAssignment is the durable event authorizing another analysis. Other
// human activity can change the review fingerprint without requesting a run.
func LatestAssignment(input Input) time.Time {
	var latest time.Time
	username := "@" + strings.ToLower(input.Reviewer.Username)
	for _, note := range input.Notes {
		body := strings.ToLower(note.Body)
		if note.System && strings.Contains(body, "requested review from ") && strings.Contains(body, username) && note.CreatedAt.After(latest) {
			latest = note.CreatedAt
		}
	}
	return latest
}

// AssignmentKey keeps a skip decision stable while unrelated MR input changes.
// Without an assignment event, the zero timestamp represents the initial request.
func AssignmentKey(input Input) string {
	return fmt.Sprintf("%s:%d:%s", input.MRKey, input.Reviewer.ID, LatestAssignment(input).UTC().Format(time.RFC3339Nano))
}
