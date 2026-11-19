# 10. One Atlantis instance per environment

Date: 2026-11-15

## Status

Proposed

## Context

A Terraform mono-repo with dev, staging and prod environments is usually served by one Atlantis. That instance holds credentials for every environment. `terraform plan` runs code from the pull request, for example through `data "external"`, so anyone who can open a pull request can run code with prod credentials. Approvals, policies and the audit trail are also shared, so prod can't have stricter rules than dev.

Atlantis already supports several servers on one repository:

- **Repo config file per server.** Server-side `repo_config_file` names the file a server reads from the pull request, for example `atlantis-prod.yaml`.
- **Command name per server.** `--executable-name` sets the comment command (`atlantis-prod plan`), and `--vcs-status-name` sets the commit status.
- **Per-server rules.** `--gh-team-allowlist`, `plan_requirements`, `apply_requirements`, `import_requirements`, `allowed_overrides` and Conftest policy checks are set per server.

Each instance of this fork is its own Helm release, with its own ServiceAccount, Leases and S3 stores.

GitHub delivers each event to every webhook on a repository, and a GitHub App has exactly one webhook URL. So several instances each receive every pull request and comment, independently.

Checking the code found four ways around this separation. Three let a server plan outside its own repo config file:

- **Missing or empty file.** If the file is missing from the pull request or lists no projects, the default autodiscovery mode plans every modified directory.
- **Targeted plans.** `<command> plan -d <dir>` for a directory that isn't in the file still runs, with default settings.
- **Edited file.** The file is read from the pull request, so the pull request can add directories to it.

The fourth defeats distinct command names. Besides its own executable name and `@<bot user>`, every server accepts the hard-coded, undocumented alias `run` (`CommentParser.Parse`). So `run apply` reaches every instance, and applies on each one the commenter is authorized for and whose apply requirements are met. Team allowlists and apply requirements can't tell that comment apart from an intended one.

## Decision

Run one Atlantis instance per environment.

**Instances.**

| | dev | staging | prod |
| --- | --- | --- | --- |
| Helm release and namespace | `atlantis-dev` | `atlantis-staging` | `atlantis-prod`, in a separate cluster or account where possible |
| Cloud identity | workload identity for the dev account | staging account | prod account only |
| GitHub identity and webhook | GitHub App `atlantis-dev` | `atlantis-staging` | `atlantis-prod` |
| Repo config file | `atlantis-dev.yaml` | `atlantis-staging.yaml` | `atlantis-prod.yaml` |
| Command and status name | `atlantis-dev` | `atlantis-staging` | `atlantis-prod` |
| S3 plan and log stores | own bucket and KMS key | own | own |

Each instance's cloud identity can reach only its own environment. That is the security boundary: even if `atlantis-dev.yaml` listed a prod directory, dev's credentials couldn't read prod state or apply there. One GitHub App per instance is required, since an App has one webhook URL. It also gives each environment its own bot identity.

**Distinct command names.** With a shared `atlantis` command, `atlantis apply` would apply every planned environment at once and `atlantis unlock` would discard plans everywhere. Comment history also wouldn't show which environment a command targeted. Autoplan is unaffected: each instance plans the projects in its own file on every pull request.

Distinct names are only environment-specific once the `run` alias is off. This needs a fork change: a new opt-in server flag, `--disable-run-alias`, under which a server accepts only its executable name and `@<bot user>`. Each instance's `@<bot user>` is already distinct, since each has its own GitHub App. Every instance sets the flag, and prod isn't rolled out without it.

**Required settings on every instance.** These close the four gaps above:

- `--disable-run-alias`, once the fork change lands;

- server-side `autodiscover: mode: disabled`, which overrides the repo's setting;
- `--silence-no-projects=true`, so a targeted plan outside the file is skipped;
- for prod, `plan_requirements`, `apply_requirements` and `import_requirements` of `[approved, mergeable, undiverged]`, with branch protection requiring CODEOWNERS review. Atlantis's `approved` counts any approval; `mergeable` is what makes code-owner review count, so nothing a pull request adds to `atlantis-prod.yaml` runs before a code owner approves.

**Governance.**

- **CODEOWNERS:** the platform team owns `atlantis-*.yaml`, and each environment's owners own `envs/<env>/**`.
- **Team allowlists:** on prod, engineers may plan; only a prod-approvers team may apply and import.
- **No repo-side overrides on prod:** `allowed_overrides: []`. Dev may allow `inputs`.
- **Policy checks:** Conftest checks on staging and prod, with policies and approvals owned by security.
- **Audit:** each instance ships its access and command logs to the SIEM, with its own API secret and web credentials.
- **Promotion order (optional):** branch protection requires the `atlantis-staging` status before merging.

**Rollout.**

1. Split the repo config into one file per environment, and add CODEOWNERS and branch protection. Implement `--disable-run-alias` (issue, then pull request).
2. Deploy dev and staging alongside the current setup, and compare plans.
3. Deploy prod with the settings above, including `--disable-run-alias`, moving one prod project group at a time.
4. Retire the shared instance and its broad credentials.

**Alternatives considered.**

- **One instance with per-project requirements and team allowlists.** Every environment's credentials stay in one place, and pull request code can still run with prod credentials during plan.
- **A per-project `instance` key in one shared `atlantis.yaml`.** It needs fork changes, and it has the same three gaps unless they are handled in the filter. CODEOWNERS also can't give each environment its own owners within one file.
- **One command name with an `-x <instance>` flag.** It needs fork changes, and a forgotten or mistyped `-x` silently does nothing. Distinct executable names work today.

## Consequences

- A malicious or mistaken pull request can only reach the environment whose instance plans it. Prod plans don't run before code-owner review.
- Prod gets stricter rules than dev: approvers, policies and no repo-side overrides.
- Three instances to run, upgrade and monitor, with three GitHub Apps and three sets of credentials.
- Engineers use `atlantis-<env>` commands. A comment with the old `atlantis` name, or with `run`, gets no response once the shared instance is retired.
- Prod depends on one fork change, `--disable-run-alias`. It is opt-in, so single-instance setups keep the `run` alias.
- Follow-ups, each needing its own issue:
  - a server-side directory allowlist per instance, as a hard boundary that pull requests can't change;
  - separate plan and apply credentials, since native `env` applies to every step and prod can't plan with a read-only role;
  - caller identity (ADR 0008), since API and UI actions share one identity until then. Leave the API off on prod meanwhile.
