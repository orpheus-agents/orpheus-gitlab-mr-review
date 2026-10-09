<p align="center">
  <a href="https://orpheus-agents.github.io/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset=".github/orpheus-logo.svg">
      <img src=".github/orpheus-logo-light.svg" alt="Orpheus" width="240">
    </picture>
  </a>
</p>

# orpheus-gitlab-mr-review

A CLI connector that triggers GitLab merge request review through Orpheus.

- assign the connector's GitLab user as an MR reviewer
- service creates an Orpheus Session
- publishes the validated findings
- removes itself from reviewers when the lifecycle is complete

## Requirements

- GitLab bot user with an API token and access to the projects it should review;
- Orpheus API key, agent profile, and sandbox template;
- a mounted directory of Markdown workflow files with YAML front matter;
- read-only repository access over HTTPS and `glab` authentication inside the Orpheus sandbox template;
- Git, Python 3, `base64`, `gzip`, and `sha256sum` in the sandbox.

The connector communicates only with GitLab and the Orpheus API. AgentBox execution is managed by
Orpheus.

The connector clones repositories over HTTPS using GitLab's `http_url_to_repo`. The sandbox must
provide Git authentication for private repositories over HTTPS. A missing or invalid HTTPS clone URL
rejects the session contract; HTTP and SSH clone URLs are not accepted.

## Configuration

Copy the example configuration:

```shell
cp .env.dist .env
```

Set the required values:

```dotenv
APP_MODE=prod

GITLAB_BASE_URL=https://gitlab.example.com
GITLAB_TOKEN=<bot-token>

ORPHEUS_BASE_URL=https://orpheus.example.com
ORPHEUS_WEB_BASE_URL=https://orpheus.example.com
ORPHEUS_API_KEY=<api-key>
WORKFLOWS_DIR=workflows
```

`WORKFLOWS_DIR` defaults to `workflows`, relative to the process working directory. Timeout, polling,
worker, queue and concurrency settings remain service environment settings; see `.env.dist`.

## Workflows

Each root-level `.md` file in `WORKFLOWS_DIR` defines one workflow. Its YAML front matter configures
project routing and the agent environment; the Markdown body contains your review instructions.
Hidden files, editor backup files beginning with `#`, non-Markdown files and subdirectories are ignored.

For projects 10, 11 and 13, create `workflows/projects.md`:

```markdown
---
id: selected-projects
project_ids: [10, 11, 13]
profile: review
sandbox_template: gitlab-review
services: [gitlab, redmine]
language: de
notes:
  success: "✅ Prüfung abgeschlossen. Bestätigte Befunde: {{ .FindingsCount }}."
  fail: "⚠️ Die automatische Prüfung konnte nicht abgeschlossen werden."
---

# Project review instructions

Add the review rules for projects 10, 11 and 13 here.
```

For all remaining projects, create `workflows/default.md` without `project_ids`:

```markdown
---
id: default-review
profile: review
sandbox_template: gitlab-review
services: [gitlab]
---

# Default review instructions

Add the review rules for all other projects here.
```

Copyable files are in [examples/workflows](examples/workflows). Replace their instruction bodies and
Orpheus profile, template and service names with your actual configuration before using them:

```shell
mkdir -p workflows
cp examples/workflows/*.md workflows/
```

| Field | Meaning |
| --- | --- |
| `id` | Required stable, unique workflow ID: lowercase letters, digits, `_` and `-`, up to 80 characters, starting with a letter or digit. It is independent of the filename. |
| `project_ids` | Optional non-empty list of unique positive GitLab project IDs. Omitting it defines the fallback workflow. |
| `profile` | Required Orpheus agent profile name. The profile selects the harness, model and agent runtime settings configured on the Orpheus server. `review` is an example name. |
| `model` | Optional override of the profile's model. |
| `sandbox_template` | Required Orpheus sandbox template name. |
| `services` | Required explicit list of service codes. Use `services: []` when no services are needed. |
| `language` | Optional output language tag, such as `ru`, `en` or `pt-BR`. |
| `notes.success` | Optional Markdown template for the completion note. |
| `notes.fail` | Optional Markdown template for the failure note. |

Explicit project routes take precedence over the fallback, independently of filename order. There may
be at most one fallback; project IDs cannot overlap between workflows. A fallback is optional. If no
workflow matches a newly assigned MR, the connector posts an English ⏭️ skip note and removes only the
bot reviewer without creating an agent session.

The workflow fully specifies its environment. Agent settings are not inherited from connector env
variables. `profile`, `sandbox_template` and service codes must already exist in Orpheus; unknown names
are rejected by its API. `services` is sent as `configuration.sandbox.services`, including an explicit
empty list. Service codes start with a lowercase letter, contain lowercase letters, digits, `_` or `-`,
and are at most 64 characters long. Service credentials and repository access are supplied by Orpheus;
the connector's `GITLAB_TOKEN` is not copied to the agent. Service selection requires Orpheus v0.6.0 or
later.

The Markdown body is passed as agent instructions and replaces the profile's instructions. The
connector adds an output language directive only when `language` is present. It applies to agent
responses, finding titles and bodies, recommendations, resolution explanations and recurrence
comments. Without `language`, the connector imposes no output language. Protocol field names, enum
values and hidden markers stay unchanged. Default completion, failure and skip messages are English
with ✅, ⚠️ and ⏭️ emoji. `language` does not translate service messages or custom publication templates.

### Publication notes

Customize completion and failure text through the optional `notes` block in a workflow's front matter:

```yaml
notes:
  success: "✅ Review completed. Confirmed findings: {{ .FindingsCount }}."
  fail: |
    ⚠️ Review could not be completed: {{ .Reason }}.

    Orpheus session: {{ .SessionURL }}
```

Each field is optional. Omitting it uses the existing English service message. Custom templates replace
the whole visible note, including emoji and any links; the connector appends its hidden marker itself.
Write templates in the language you want to publish. They are separate from the instructions sent to
the agent and are not affected by `language`.

Templates support fixed placeholders:

| Placeholder | Value |
| --- | --- |
| `{{ .FindingsCount }}` | Number of confirmed findings in the successful review, including zero. |
| `{{ .Reason }}` | Allowlisted English failure reason; raw errors, credentials and hook output are never substituted. |
| `{{ .SessionURL }}` | Public Orpheus session URL for a failure, or an empty string when unavailable. |

Only these substitutions are supported; template functions, conditions and other expressions are
rejected at startup. Explicitly blank fields, invalid placeholders and templates that can render an
empty note also fail startup. Include meaningful text around optional values such as `SessionURL`.

Effective templates are included in the workflow revision and initial session metadata. Publication
and failure recovery use the saved templates even after files change or the workflow is removed.
Sessions created without custom notes continue to use the service defaults. Confirmed terminal
markers prevent a retry from publishing the note again.

The unmatched-project skip note remains an English service message because no workflow is selected.

The connector embeds the MR context and the agent execution contract: unattended operation,
pinned-diff commands, blocking incomplete checks, finding verification and artifact transitions,
artifact schemas and publication ownership. Define project review criteria, checks, requirement
sources and rules for evaluating previous findings in your workflow body. Findings are published
from validated artifacts; the final agent response is a session log, not a GitLab publication channel.

Files must be regular UTF-8 files with valid front matter and a non-empty instruction body. Unknown
fields, malformed metadata, duplicate IDs, overlapping project routes, multiple fallbacks, empty
directories and files larger than `MAX_SESSION_REQUEST_BYTES` stop the entire service at startup.
The fully assembled session request is also bounded by `MAX_SESSION_REQUEST_BYTES`.

Workflows are loaded once at startup. Restart after editing them. Accepted sessions retain their saved
workflow ID and revision and are recovered even if the workflow is removed or its project routes
change. Editing configuration alone does not trigger another analysis; assign the reviewer again to
request one.

When upgrading from instruction files, put each policy into a workflow Markdown body, add the front
matter and set `WORKFLOWS_DIR`. Replace `ORPHEUS_AGENT_INSTRUCTION_FILES`, `ORPHEUS_AGENT_PROFILE`,
`ORPHEUS_AGENT_MODEL`, `ORPHEUS_SANDBOX_TEMPLATE` and `ORPHEUS_SERVICES` with workflow fields; those
connector env variables are no longer read.

## Run locally

Go 1.27 or newer is required.

```shell
go run .
```

For development with Air:

```shell
make up
docker compose logs -f air
```

The process runs until it receives `SIGINT` or `SIGTERM` and performs bounded graceful shutdown.

All application logs are newline-delimited JSON on stdout, including startup failures before
configuration is loaded. Each record contains `timestamp`, `level`, and `msg`; errors include an
`error` field. Configuration, initialization, and runtime failures exit with code 1. Startup failures
are reported regardless of `LOG_LEVEL`.

## Run with Docker

Mount the workflow directory read-only and set its container path in `.env`:

```shell
docker run --detach \
  --name orpheus-gitlab-mr-review \
  --restart unless-stopped \
  --env-file .env \
  --volume /host/path/workflows:/var/app/workflows:ro \
  retailcrm/orpheus-gitlab-mr-review:latest
```

For this example, configure:

```dotenv
WORKFLOWS_DIR=/var/app/workflows
```

Run exactly one connector instance for a GitLab installation and bot identity. Concurrent instances
are unsupported because admission does not use a distributed lock.

## Request a review

1. Add the bot user to the GitLab project.
2. Assign the bot as reviewer on an open merge request.
3. Wait for the next poll. The connector creates or resumes the matching Orpheus Session.
4. After successful analysis, findings and a completion note are published in GitLab.
5. The connector removes only itself from the reviewer list.

For a new review after completion or failure, assign the bot as reviewer again. Project scope is
controlled by GitLab access, reviewer assignment and workflow project routing.

The service exposes no HTTP API or health endpoints. Use process status and structured logs for
operations and diagnostics.

See [ARCHITECTURE.md](ARCHITECTURE.md) for lifecycle, idempotency, stale-result handling, and restart
recovery details.
