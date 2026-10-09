# `orpheus-gitlab-mr-review` architecture

## 1. Overview

`orpheus-gitlab-mr-review` is a long-running CLI connector. It polls GitLab for merge requests assigned
to the authenticated user, creates one-shot review sessions through the Orpheus API, and publishes
validated results back to GitLab.

```text
GitLab API
    ↑  ↓
Watcher → bounded snapshot queue → Reconciler
                                      ↓  ↑
                                   Orpheus API
                                      ↓
                             Session / Run / HookResult
```

The process has no inbound API or health probes. A process supervisor controls it through exit status
and `SIGINT`/`SIGTERM`.

The connector does not interact with AgentBox directly. AgentBox provisioning, execution, workspace
lifecycle, and cleanup are internal Orpheus responsibilities hidden behind Session and Run APIs.

## 2. Responsibilities and state

The connector owns:

- GitLab polling and eligibility checks;
- merge request, diff, and review identity;
- Orpheus session admission and cancellation;
- the immutable workflow contract;
- terminal bundle validation;
- all GitLab publication and reviewer mutations;
- recovery after process restart.

Orpheus owns durable Session, Run, message metadata, and HookResult state. GitLab owns current merge
request state, reviewer assignment, discussions, and publication markers.

The connector has no database. Its queue and worker state are disposable; recovery uses only Orpheus
and GitLab.

## 3. Runtime flow

### 3.1 Startup and polling

Startup validates configuration, loads and validates all Markdown workflows from `WORKFLOWS_DIR`,
constructs the GitLab and Orpheus clients, and starts `Watcher.Run(ctx)` and `Reconciler.Run(ctx)` concurrently. The watcher
resolves the authenticated GitLab user before its first poll.

The watcher performs one immediate poll and then polls every `POLL_INTERVAL_SECONDS`. Each successful
tick:

1. lists all open merge requests assigned to the authenticated bot;
2. loads the merge request, project, notes, and diff refs for every candidate;
3. builds immutable `review.Input` values;
4. submits one complete `review.Snapshot` to the reconciler.

The GitLab adapter reads the merge request before and after the related requests. A concurrent change
invalidates the candidate, so mixed control-plane state is never submitted. A failure for any candidate
discards the whole tick. Snapshot submission is non-blocking; a full queue defers work to a later poll.

GitLab access and explicit reviewer assignment determine polling scope. Workflow `project_ids` select
the instructions and environment for new analyses; a workflow without `project_ids` is the fallback.
Overlapping selectors, duplicate IDs and multiple fallbacks fail startup. A missing fallback is allowed.

### 3.2 Identity and eligibility

The merge request key is:

```text
<normalized-gitlab-host>:<project-id>!<merge-request-iid>
```

The diff fingerprint hashes versioned canonical data containing the GitLab host, project and merge
request IDs, source and target branches, and all three diff refs. It identifies the exact code state.

The review fingerprint additionally includes the merge request description, resolvable human notes,
and relevant reviewer or resolution system notes. It identifies one admission input.

A candidate is eligible when the merge request is open, the bot is still assigned, the project IDs
match, and `base_sha`, `start_sha`, and `head_sha` are present. Draft status alone does not make a merge
request ineligible.

### 3.3 Reconciliation and admission

Each snapshot has two ordered phases.

The recovery phase first lists active Sessions in the `gitlab/mr-review` namespace. It validates each
Session against immutable message metadata and current GitLab state. If a Session is absent from the
snapshot, the connector fetches its merge request using IDs stored in metadata. This detects closure,
reviewer removal, or diff changes that happened before the first poll of a new process.

The admission phase then inspects current candidates with a bounded worker pool. For each candidate it:

1. completes any terminal lifecycle already recorded by a GitLab marker;
2. searches for an exact Session by namespace, merge request key, and review fingerprint;
3. checks the latest Session for the merge request to prevent an unintended second analysis;
4. selects a workflow by project ID only when no existing lifecycle owns the assignment;
5. publishes a skip note and removes the bot when no workflow matches, otherwise creates a Session
   when global capacity is available.

`MAX_CONCURRENT_REVIEWS` limits active review Sessions. Excess candidates remain assigned and are
reconsidered later. A stable `Idempotency-Key` protects `CreateSession` against a lost or uncertain
HTTP response. The key is a UUID v5 derived from the versioned canonical review identity: namespace,
merge request key, reviewer ID, and review fingerprint. It is recomputed after restart and requires no
local persistence. The UUID namespace, name prefix, and payload encoding remain stable across retries.

### 3.4 Orpheus Session contract

Every review uses one Session with `allow_multiple_runs=false`. The immutable request contains:

- workflow namespace, merge request key, and review fingerprint;
- one initial message with metadata schema v1;
- the embedded MR context and agent execution/artifact/publication contract, plus the selected workflow instructions;
- the workflow's agent profile, optional model, sandbox template and explicit services;
- an optional output language directive for agent-authored text;
- optional success and failure publication templates saved in message metadata;
- `before_run` and `after_run` hooks;
- run and hook timeouts.

Metadata binds the Session to GitLab identity, reviewer ID, diff refs, fingerprints, workflow ID and
revision, protocol versions, artifact path, and helper digest. Oversized requests are rejected before any
Orpheus mutation.

Optional `notes.success` and `notes.fail` are connector-owned publication settings. They are not agent
instructions. Their exact text contributes to the workflow revision and is saved in the initial
metadata, so accepted-session publication uses its original templates after restart or configuration
changes. Metadata without notes remains valid and uses the English service defaults. Before a session
is accepted, a preparation failure uses the selected workflow's current failure template.

`before_run` prepares a detached checkout at the pinned head SHA, verifies the embedded helper, and
creates the artifact contract. The agent writes review artifacts inside the sandbox. `after_run`
verifies the helper again and emits one compact result envelope through hook stdout.

The base prompt requires unattended operation, reading the pinned diff, recording blocking checks,
verifying findings and moving them to their final artifact directories. Workflow bodies define
project review criteria, checks, requirement sources and how to evaluate previous findings.

### 3.5 Result validation

A result is publishable only when:

- the Run and agent completed successfully;
- exactly one successful, complete, non-truncated text result exists for `after_run`;
- message metadata matches the review input;
- workflow revision, helper digest, GitLab identity, and fingerprints match;
- the versioned envelope and bundle pass digest, size, count, path, and field validation;
- no pending finding remains.

The envelope contains a bounded zlib-compressed, base64-encoded JSON bundle with confirmed findings,
recommendations, and resolution intents. Unknown versions and incomplete output fail closed.

### 3.6 GitLab publication

Only the connector mutates GitLab. Before every mutation it fetches the merge request and verifies
that it is open, the bot is assigned, all diff refs are present, and the diff fingerprint is unchanged.

The publisher creates inline discussions, falls back to regular notes for confirmed invalid positions,
publishes recommendations, applies bot-owned resolutions, writes a completion note, and removes only
the bot reviewer.

Every finding, recommendation, resolution, completion, error and skip mutation has a deterministic hidden
marker. The connector checks markers before a mutation and after an uncertain response. Reviewer
removal occurs only after a completion, error or skip marker is confirmed. Skip markers bind to the
reviewer assignment rather than the changing review fingerprint, so unrelated human activity cannot
restart a skipped request or duplicate its note while reviewer removal is pending.

For a recurring finding linked to a bot-owned discussion:

- an open discussion is kept without a duplicate;
- an outdated discussion gets a new thread at the current diff position;
- a discussion explicitly resolved by a human receives a finding-specific reply and is reopened;
- a finding omitted from the new bundle remains closed;
- an unknown resolution cause stops publication.

## 4. Stale, error, and recovery behavior

### Stale reviews

An active Run is cancelled when the merge request closes, the bot is removed, diff refs become
incomplete, or the diff fingerprint changes. If the Run becomes terminal before cancellation is
observed, its bundle is discarded and never published. Process shutdown itself does not cancel active
Orpheus Runs.

### Errors and retries

A transient GitLab or Orpheus failure ends the current reconciliation tick. The next poll reconstructs
state and retries the lifecycle without starting another analysis.

A permanent failure produces a short allowlisted GitLab note. Raw errors, hook output, payloads, and
credentials are never published. When a valid Session ID exists, the note links to Orpheus Web UI.
The connector confirms the error marker before removing its reviewer.

A failed, cancelled, or invalid Run is not analyzed again unless GitLab contains a new explicit
reviewer assignment.

### Restart recovery

After restart, the connector uses:

- namespace-wide active Session listing;
- exact and latest Session lookup by merge request identity;
- immutable message metadata;
- terminal HookResult data;
- GitLab publication, error and skip markers.

This restores active-run monitoring, stale cancellation, partial publication, error publication, and
pending reviewer removal. Recovery uses the saved workflow ID and revision rather than current
workflow definitions: changes to instructions, project routing or workflow removal do not alter accepted
sessions and never authorize another analysis. It also handles an active Session whose reviewer was
removed before the first new snapshot.

## 5. Concurrency and shutdown

`Watcher` and `Reconciler` communicate through a bounded in-memory channel. Snapshots are reconciled
sequentially; candidate lookups inside a snapshot use `RECONCILE_WORKER_COUNT` workers. A transient
recovery error prevents all new admission for that tick.

On `SIGINT` or `SIGTERM`, the application cancels the shared context, stops new polling, drains accepted
snapshots, and invokes both `Stop(shutdownCtx)` methods concurrently. In-flight HTTP work gets at most
`SHUTDOWN_TIMEOUT_SECONDS`.

The connector supports one active process for a GitLab installation and bot identity. There is no
distributed admission lock. Deployment must stop the running process before starting its replacement;
overlapping instances are unsupported.

## 6. Security boundary

- GitLab and Orpheus credentials come from environment or secret injection.
- Credentials are not included in prompts, metadata, bundles, markers, or logs.
- The connector GitLab token is not copied into the Orpheus Session request.
- Sandbox repository and `glab` access are supplied by the Orpheus sandbox template and must be
  read-only.
- The prompt forbids agent-side GitLab mutations; the connector performs all mutations.
- Workflow files must be regular UTF-8 Markdown files with valid front matter and non-empty bodies.
  All are validated once at startup, with atomic failure for invalid or ambiguous configuration.

## 7. Operational constraints

- Polling is the only event source.
- Only one connector process may be active for one GitLab installation and bot identity.
- User workflow definitions reload only after a process restart. The base MR prompt and agent
  execution/artifact contract remain embedded; project review criteria come from workflows.
- `language` is optional and affects agent-authored output only. Default service messages are English;
  optional success/failure templates publish user-provided text without translation. The skip note
  remains a service default because no workflow matches that project.
- Timeouts, workers, queue capacity and concurrency limits remain service settings shared by workflows.
- Sessions are one-shot; automatic analysis retry and reusable Sessions are unsupported.
- Transient lifecycle retries happen on a later poll; there is no independent retry scheduler.
- Detection of a new reviewer assignment currently relies on GitLab system-note text.
- Sandbox credential provisioning belongs to the Orpheus deployment.
- The service exposes no HTTP API or process probes.

## 8. Safety invariants

1. One reviewer assignment creates at most one Orpheus Session and one analysis Run.
2. A changed or incomplete diff is never published.
3. A lifecycle retry does not create another analysis for an existing Session.
4. Only validated `after_run` output can reach GitLab.
5. GitLab mutations are idempotent or verified from current GitLab state.
6. Reviewer removal requires a confirmed completion, error or skip marker, except when discarding a stale
   review request.
7. Human reviewers and discussions not owned by the bot are not modified.
8. Process restart does not cancel an active Orpheus Run.
9. Recovery uses only durable Orpheus and GitLab state.
10. The connector does not interact with AgentBox directly.
