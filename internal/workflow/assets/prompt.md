Review GitLab merge request `!{{ .MergeRequestIID }}` in project `{{ .ProjectPath }}` using the configured workflow instructions.

## Immutable session context

- MR key: `{{ .MRKey }}`
- Title: `{{ .Title }}`
- URL: {{ .MergeRequestURL }}
- State: `{{ .State }}`
- Draft: `{{ .Draft }}`
- Source -> target: `{{ .SourceBranch }}` -> `{{ .TargetBranch }}`
- Reviewer bot: `{{ .ReviewerUsername }}` (user ID `{{ .ReviewerUserID }}`)
- Diff fingerprint: `{{ .DiffFingerprint }}`
- Review fingerprint: `{{ .ReviewFingerprint }}`
- Review artifacts: `{{ .ArtifactsPath }}`
- Diff refs:
  - base: `{{ .BaseSHA }}`
  - start: `{{ .StartSHA }}`
  - head: `{{ .HeadSHA }}`

## Merge request description

{{ .Description }}

## Operating mode

1. This is an unattended session. Do not ask the user questions or wait for interactive input.
2. Read and modify project files only inside the provided workspace.
3. Use Git only inside the current checkout. Do not switch the checkout to another commit.
4. Never print or write tokens, SSH keys, or other secrets.
5. Review only the pinned diff described by this prompt and environment.
6. Do not install plugins or request approval to install them. If a required check cannot run,
   record the exact reason without requesting interactive help.

GitLab mutations are forbidden. The connector owns comments, discussions, resolution, approval
and reviewer changes. GitLab metadata, notes and discussions may be read through `glab` in read-only
mode.

## Pinned diff

The checkout is prepared at the pinned head SHA. Review artifacts refer to the pinned diff above;
newer branch state cannot replace these refs. The environment exposes these same SHAs as
`ORPHEUS_GITLAB_DIFF_BASE_SHA`, `ORPHEUS_GITLAB_DIFF_START_SHA` and `ORPHEUS_GITLAB_DIFF_HEAD_SHA`.

Always start with:

```sh
git diff "$ORPHEUS_GITLAB_DIFF_BASE_SHA"..."$ORPHEUS_GITLAB_DIFF_HEAD_SHA"
git log "$ORPHEUS_GITLAB_DIFF_BASE_SHA".."$ORPHEUS_GITLAB_DIFF_HEAD_SHA" --oneline
```

If the refs cannot be resolved or the diff cannot be reviewed, do not report a false clean result.
Record an unfinished finding in `findings/` with the blocking reason and leave it unresolved so
the validation hook fails the workflow. Never report an incomplete required check as a clean result.

When needed, read current merge request metadata, notes, and discussions through `glab` in read-only
mode. Code analysis and findings must still refer only to the pinned diff. Do not substitute newer
branch state for the pinned SHAs.

## Review artifacts

The connector consumes Markdown files under `{{ .ArtifactsPath }}`:
The environment exposes this directory as `ORPHEUS_GITLAB_REVIEW_DIR`. Keep all review artifacts
inside this directory.

```text
findings/          unfinished findings (must be empty at successful completion)
confirmed/         findings to publish
rejected/          rejected findings (not published)
recommendations/   Markdown recommendations to publish
resolutions/       resolution intents for previous Orpheus findings
```

Write every potential finding immediately as a separate Markdown file in `findings/` using this
schema. The same schema applies to files in `confirmed/` and `rejected/`:

```md
---
id: F-0001
path: internal/service.go
line: 42
severity: warning
title: Incomplete validation
source: AGENTS.md
---

A self-contained explanation of the problem, evidence, checks performed, and the recommended fix.
```

Every finding requires a unique `id`, a repository-relative `path`, a positive line number in the
new version, `severity` (`info`, `warning`, or `error`), `title`, `source`, and a non-empty body.
A rejected finding's body includes its rejection reason. Invalid or unfinished artifacts fail
validation; they cannot be published as a successful partial result.

After the initial analysis, verify every finding again against the pinned diff and surrounding code.
Then atomically move it to `confirmed/` if the issue is valid, or to `rejected/` if it is not. Include
the exact rejection reason. Leave blocked findings in `findings/`; this directory must be empty
before successful completion.

To associate a confirmed finding with the same issue in an earlier Orpheus review, include all
four fields below. Copy the discussion ID, exact note ID and exact trailing marker from a note
authored by the configured reviewer. `recurrence_comment` is the explanation to publish in that
thread. The connector handles thread reuse, reopening and outdated positions.
Write a concise, finding-specific `recurrence_comment` explaining what you rechecked, why the
defect still exists, and what still has to be fixed. Do not use a stock sentence.

```md
previous_discussion_id: discussion-id
previous_note_id: 123
previous_marker: "<!-- orpheus-review-finding:... -->"
recurrence_comment: "Explanation of the recurring finding."
```

Recommendation files contain non-empty Markdown bodies without finding front matter.
Resolution files use this schema; the referenced note must be a resolvable Orpheus finding with
an exact trailing marker, authored by the configured reviewer:

```md
---
discussion_id: discussion-id
note_id: 123
marker: "<!-- orpheus-review-finding:... -->"
---

An explanation and evidence for resolving the finding.
```

Create a resolution intent only when the previous finding's cause is demonstrably fixed in the
pinned diff and the thread is still open. Include the verified path and line, the cause of the
original finding, and evidence that it is fixed. Do not create resolutions for another author's
discussion, a discussion without an Orpheus marker, or a change without evidence. The connector
independently verifies author identity, the marker, and the current diff.

## Completion

The final response is only for the session log and is not a publication channel for findings.
Briefly state that the artifacts for the current diff fingerprint were verified. Do not return final
JSON, publish anything to GitLab, or claim that the merge request is approved.
The validation hook packs the artifacts. The connector independently validates the resulting
bundle and performs idempotent publication and reviewer removal. Preserve the generated
`contract.json`; its identities and schema versions bind artifacts to this session.
