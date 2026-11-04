# 6. Structured audit events

Date: 2026-10-30

## Status

Proposed

## Context

Atlantis applies infrastructure changes with privileged cloud credentials. Operators need to answer "who did what, to which project, when, from where, and with what result", and to send that to a SIEM. Today they can't do that from one source. See issue #23.

- **Application logs** are zap JSON on stdout. Per-command lines carry `repo`, `pull`, the replica and OpenTelemetry trace IDs (`DefaultCommandRunner.buildLogger`), but not the actor, the VCS host or the command. Lines are free text, and `--log-level=warn` drops most of them.
- **PR comments** are the de facto trail. Repo admins can edit or delete them, `--hide-prev-plan-comments` hides them, and actions outside a PR (UI lock deletes, the global apply lock, API calls) don't produce one.
- **Webhooks** fire only for apply results and drift.
- **Job logs** hold Terraform output, which can contain secrets, and they are deleted when the pull closes.
- **The Kubernetes API audit log** records lock, `PullStatus` and lease writes as the Atlantis service account, never the end user.
- **The HTTP access log (#41)** records each UI or API request that can change state at info level: method, path, source address, actor and HTTP status. It also logs failed and unparsable web logins at warn. It redacts query values other than a lock `id`, and #40 redacts credentials in URLs. These are free-text application log lines, filtered by `--log-level`. They record that a request arrived and its HTTP status, not the action's outcome. A UI lock delete shows the lock `id` and `HTTP 200`, but not whether the discard was recorded or which pull request the plan belonged to.
- **API and UI actors are shared identities.** The access log names them `api`, the shared web user or `anonymous`. The API uses one shared secret, and the UI at most one shared basic-auth user (issue #24).

Clusters already collect container stdout. A format that log pipelines can filter, and that is emitted whatever the log level, reaches a SIEM without new infrastructure.

## Decision

Atlantis will emit one **audit event** per auditable action, as a single JSON line on stdout from a dedicated logger named `audit`. Events are written at info level through their own zap core, so `--log-level` doesn't suppress them. A new flag, `--disable-audit-log`, turns them off.

**Actions.** The events cover:
- plan, apply, policy check, `approve_policies`, unlock, import and state rm, from comments, autoplan and the API;
- lock delete and global apply-lock enable and disable, from the UI and the API;
- API plan, apply and drift runs;
- failed web logins (`login`, outcome `refused`), with the username tried and the source address, never the password.

An action emits one event when it finishes. That includes refusals (apply requirements, the global apply lock, the pull lock, a discarded plan), which are often what an investigation needs.

**Format** (version 1):

```json
{
  "logger": "audit",
  "ts": "2026-10-30T12:00:00.000Z",
  "audit_version": 1,
  "action": "apply",
  "outcome": "success",
  "reason": "",
  "actor": {"source": "vcs", "name": "octocat", "verified": true},
  "trigger": "comment",
  "vcs_host": "github.com",
  "repo": "org/infra",
  "pull": 42,
  "head_commit": "3f2c…",
  "project": "network",
  "dir": "envs/prod",
  "workspace": "default",
  "plan_sha256": "9a1b…",
  "replica": "atlantis-1",
  "trace_id": "4bf9…"
}
```

- **`outcome`** is `success`, `failure` or `refused`. `reason` is a short message for failure or refusal. It is never Terraform output and never command stderr.
- **`actor.source`** says where the actor's name comes from:
  - `vcs`: the comment or event author, from a signed webhook;
  - `api`: the API token. The name comes from the named tokens in #24, otherwise it is `api`;
  - `ui`: the trusted proxy user from #24, otherwise `ui`;
  - `system`: autoplan triggered by a push, or a scheduled drift run.

  `verified` is true only when the name comes from an authenticated source. A shared secret or shared basic-auth user is not verified.
- **`trigger`** is `comment`, `autoplan`, `api`, `ui` or `schedule`.
- **Project fields** (`project`, `dir`, `workspace`) are empty for pull-level or global actions. Each project of a multi-project command gets its own event.
- **`plan_sha256`** is the plan hash Atlantis already records for apply validation (`ExpectedPlanHash`). It ties an apply to the plan that was reviewed.
- **`replica` and `trace_id`** link the event to the forwarding replica's logs and to Kubernetes API audit entries made during the same request.

Fields are only added within a version. Removing or renaming one bumps `audit_version`.

**Relation to the access log (#41).** Both stay, at different layers:
- **The access log** records HTTP requests, including those refused before they reach an action (failed logins, a missing API token). It stays application logging, under `--log-level`.
- **Audit events** record actions and how they ended, and are not filtered by `--log-level`.
- **UI and API actions** produce both, joined by `trace_id`. The audit event is the record of what happened. The access line shows the request that caused it.
- **Failed logins** are logged in both, so a SIEM that reads only audit events still sees them.

The same fields are added to the per-command application log context (actor, `vcs_host`, `command`), so ordinary log lines can be joined with audit events.

**Alternatives considered:**

- **Extend the apply webhooks to every action.** Webhooks are push-only, best effort (failures are logged and dropped) and configured per deployment. They suit notifications, not records. A webhook sink for audit events can be added later on top of this format.
- **Kubernetes Events or a custom resource per action.** These are rate-limited, expire (Events after an hour by default), and would put the end user's identity into the cluster API. That doesn't help a SIEM more than stdout does.
- **Rely on PR comments plus debug logs.** These are editable, hideable and incomplete, which is the problem this ADR addresses.
- **A separate audit file or sidecar.** A file adds a volume and rotation to every pod for no gain over stdout, which the cluster already collects.

## Consequences

- **What it gives operators:** one filterable stream (`logger == "audit"`) answers who did what, where and with what result, including refusals, across replicas.
- **Identity is only as good as its source.** Actions from VCS comments are attributed to a verified VCS user. API and UI actions show `api` or `ui` and are unverified until #24 adds named API tokens and trusted proxy identity.
- **Volume:** about one line per project per command. That's small next to job output.
- **Privacy:** events contain VCS usernames and repository names, so operators should apply their log retention and access policies to them. The events never include Terraform output or secrets. Secret handling in job and application logs is covered separately in #25.
- **Compatibility:** this is additive. Existing log lines keep their messages and gain fields. Pipelines that drop unknown fields are unaffected. The format becomes a supported interface, so changes follow the versioning rule above.
- **Kubernetes API audit:** it continues to show only the service account. The docs will say that the audit events are the source of user identity, correlated through `trace_id`.
- **Follow-ups:**
  - #24: caller identity for the API and UI;
  - an optional webhook or OTLP sink for audit events.

  Done since this ADR was proposed: #25 (secrets in logs, by #40) and #26 (UI and API access logging at the default level, by #41). The access log complements audit events, as described above.
