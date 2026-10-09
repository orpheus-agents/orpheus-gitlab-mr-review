package publication

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
)

func FindingMarker(diffFingerprint string, finding protocol.Finding) string {
	payload := strings.Join([]string{
		diffFingerprint,
		finding.ID,
		strings.TrimSpace(finding.Path),
		strconv.Itoa(finding.Line),
		strings.TrimSpace(finding.Severity),
		normalize(finding.Title),
		normalize(finding.Body),
		normalize(finding.Source),
	}, "\x00")
	return "<!-- orpheus-review-finding:" + markerHash(payload) + " -->"
}

func RecommendationMarker(diffFingerprint string, recommendation protocol.Recommendation) string {
	return "<!-- orpheus-review-recommendation:" + markerHash(diffFingerprint+"\x00"+recommendation.Name+"\x00"+normalize(recommendation.Body)) + " -->"
}

func CompletionMarker(reviewFingerprint string) string {
	return "<!-- orpheus-review-complete:" + reviewFingerprint + " -->"
}

func ResolutionMarker(reviewFingerprint string, resolution protocol.Resolution) string {
	payload := strings.Join([]string{"v1", reviewFingerprint, resolution.DiscussionID,
		strconv.FormatInt(resolution.NoteID, 10), resolution.Marker, normalize(resolution.Body)}, "\x00")
	return "<!-- orpheus-review-resolution:" + markerHash(payload) + " -->"
}

// One terminal error per review, independent of changing transport errors.
func ErrorMarker(reviewFingerprint string) string {
	return "<!-- orpheus-review-error:" + markerHash("v1\x00"+reviewFingerprint) + " -->"
}

func markerHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:])
}

func findingBody(finding protocol.Finding, marker string) string {
	return fmt.Sprintf("**%s**\n\n%s\n\nSeverity: `%s`  \nSource: `%s`\n\n%s",
		strings.TrimSpace(finding.Title), strings.TrimSpace(finding.Body), finding.Severity, finding.Source, marker)
}

func completionBody(count int, marker string) string {
	if count == 0 {
		return "✅ The current merge request version was reviewed and no findings were confirmed.\n\nAutomated review is not an approval and does not replace human review of business logic and architecture.\n\n" + marker
	}
	return fmt.Sprintf("✅ Review of the current merge request version is complete. Confirmed findings: %d.\n\nAutomated review is not an approval and does not replace human review of business logic and architecture.\n\n%s", count, marker)
}

func hasOwnedMarker(discussions []gitlab.Discussion, reviewerID int64, marker string) bool {
	for _, discussion := range discussions {
		for _, note := range discussion.Notes {
			if reviewerID > 0 && note.Author.ID == reviewerID && hasTrailingMarker(note.Body, marker) {
				return true
			}
		}
	}
	return false
}

func ownedMarkerTarget(
	discussions []gitlab.Discussion,
	reviewerID int64,
	discussionID string,
	noteID int64,
	marker string,
) (int, int, bool) {
	if reviewerID <= 0 || !isFindingMarker(marker) {
		return 0, 0, false
	}
	for discussionIndex := range discussions {
		discussion := discussions[discussionIndex]
		if discussion.ID != discussionID || len(discussion.Notes) == 0 || discussion.Notes[0].Author.ID != reviewerID {
			continue
		}
		for noteIndex, note := range discussion.Notes {
			if note.ID == noteID && note.Author.ID == reviewerID && note.Resolvable && hasTrailingMarker(note.Body, marker) {
				return discussionIndex, noteIndex, true
			}
		}
	}
	return 0, 0, false
}

func isFindingMarker(marker string) bool {
	const prefix = "<!-- orpheus-review-finding:"
	const suffix = " -->"
	if !strings.HasPrefix(marker, prefix) || !strings.HasSuffix(marker, suffix) {
		return false
	}
	hash := strings.TrimSuffix(strings.TrimPrefix(marker, prefix), suffix)
	if len(hash) != 64 {
		return false
	}
	for _, character := range hash {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func hasTrailingMarker(body, marker string) bool { return trailingMarker(body) == marker }

func trailingMarker(body string) string {
	trimmed := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if index := strings.LastIndexByte(trimmed, '\n'); index >= 0 {
		trimmed = strings.TrimSpace(trimmed[index+1:])
	}
	return trimmed
}

func hasReviewer(reviewers []gitlab.User, reviewerID int64) bool {
	for _, reviewer := range reviewers {
		if reviewer.ID == reviewerID {
			return true
		}
	}
	return false
}

func normalize(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func SkippedMarker(assignmentKey string) string {
	return "<!-- orpheus-review-skipped:" + markerHash("v1\x00"+assignmentKey) + " -->"
}
