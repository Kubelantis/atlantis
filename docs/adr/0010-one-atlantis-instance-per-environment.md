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

Checking the code found these gaps in keeping each server to its own environment:

- **G1, the `run` alias.** Besides its own executable name and `@<bot user>`, every server accepts the hard-coded, undocumented alias `run` (`CommentParser.Parse`). So `run apply` reaches every instance and applies on each one the commenter is authorized for. Team allowlists and apply requirements can't tell it apart from an intended command.
- **G2, hooks before approval.** Pre-workflow hooks run in a clone of the pull request's head, with the server's credentials, before any project is built. `plan_requirements` are only checked later, per project. A hook that runs a repository script, or a tool that evaluates pull request files (for example HCL with `run_cmd`), runs pull request code before anyone approves it. Post-workflow hooks run after every autoplan, even when every plan was refused. Terragrunt and CDK Terrain discovery already wait for approval when `plan_requirements` include `approved`; hooks don't.
- **G3, missing or empty config.** If the repo config file is missing from the pull request or lists no projects, the default autodiscovery mode plans every modified directory. A targeted `<command> plan -d <dir>` plans any directory with default settings, and neither turning autodiscovery off nor `--silence-no-projects` prevents that.
- **G4, edited config.** The repo config is read from the pull request, so a pull request can add a directory to an instance, including one that belongs to another environment.
- **G5, one project, one instance.** With one file per server, nothing detects a directory listed for two instances, or for none.

## Decision

Run one Atlantis instance per environment, and add an opt-in instance mode to the fork that keeps each instance to its own projects.

**Instances.**

| | dev | staging | prod |
| --- | --- | --- | --- |
| Helm release and namespace | `atlantis-dev` | `atlantis-staging` | `atlantis-prod`, in a separate cluster or account where possible |
| Cloud identity | workload identity for the dev account | staging account | prod account only |
| GitHub identity and webhook | GitHub App `atlantis-dev` | `atlantis-staging` | `atlantis-prod` |
| Command and status name | `atlantis-dev` | `atlantis-staging` | `atlantis-prod` |
| S3 plan and log stores | own bucket and KMS key | own | own |

Each instance's cloud identity can reach only its own environment. That is the security boundary: dev's credentials can't read prod state or apply there, whatever a pull request does. One GitHub App per instance is required, since an App has one webhook URL. It also gives each environment its own bot identity.

Distinct command names keep a comment aimed at one environment. With a shared `atlantis` command, `atlantis apply` would apply every planned environment at once, and comment history wouldn't show which environment a command targeted.

**Instance mode.** A server turns it on with a block in its server-side `repos.yaml`, which pull requests can't change:

```yaml
instance:
  name: aws-prod                  # any identifier: environment, cloud, product, team
  allowed_dirs: ['^envs/prod/']   # optional
```

The name is free-form. With the block set, the server:

- accepts only its own executable name and `@<bot user>`, not the `run` alias (G1);
- runs pre- and post-workflow hooks only once the pull request is approved, when its `plan_requirements` include `approved`, as tool discovery does, and says on the pull request why they were skipped (G2);
- never falls back to autodiscovery. A missing or empty config, or one with no projects for this instance, means nothing to plan. A targeted `-d` or `-p` must match one of this instance's projects, or it is refused with a message saying why (G3);
- refuses any project outside `allowed_dirs`, if it is set (G4).

Without the block, a server behaves as today, so single-instance setups and teams moving from upstream Atlantis are unaffected.

**Mapping projects to instances.** Either layout works with instance mode:

- **One file per instance**, as upstream already supports: the server's `repo_config_file` names `atlantis-<name>.yaml`, and every project in it belongs to that instance. It uses only upstream syntax, so the files stay valid for upstream Atlantis, and CODEOWNERS can give each file its own owners.
- **One shared `atlantis.yaml`**, opt-in and fork-only. The file declares the instance names once, and every project names its instance:

  ```yaml
  version: 3
  instances: [aws-dev, aws-staging, aws-prod]
  projects:
  - dir: envs/prod/app
    workspace: default
    instance: aws-prod
  ```

  Every instance validates the whole file the same way. It refuses the file if a project has no `instance`, names one not in `instances`, or shares its directory and workspace with a project of another instance (G5). Each server then keeps only its own projects. A shared file gives one place to add or move a project. Upstream Atlantis rejects the unknown keys, so this layout is not portable back.

**Controlling which directories an instance plans (G4).** `allowed_dirs` is optional. It is the only boundary a pull request can't change, and it's recommended for prod and any instance whose credentials reach production. An organization may instead rely on its own controls:

- `[approved, mergeable]` requirements with branch protection requiring CODEOWNERS review of the repo config files;
- a CI or lint check of the repo config, for example that projects tagged `aws-prod` sit under `envs/prod/`;
- credentials and runners scoped so that an instance can only ever touch its own environment.

These depend on review or CI catching a change, where `allowed_dirs` is enforced by the server. If `allowed_dirs` is used, operators keep each instance's patterns from overlapping with the others'.

**Required settings.**

- the instance block on every instance;
- for prod, `plan_requirements`, `apply_requirements` and `import_requirements` of `[approved, mergeable, undiverged]`, with branch protection requiring CODEOWNERS review. Atlantis's `approved` counts any approval; `mergeable` is what makes code-owner review count;
- `--silence-no-projects=true`, so instances with nothing to plan for a pull request don't comment or set a status.

**Governance.**

- **CODEOWNERS:** the platform team owns the repo config files, and each environment's owners own `envs/<env>/**`.
- **Team allowlists:** on prod, engineers may plan; only a prod-approvers team may apply and import.
- **No repo-side overrides on prod:** `allowed_overrides: []`. Dev may allow `inputs`.
- **Policy checks:** Conftest checks on staging and prod, with policies and approvals owned by security.
- **Audit:** each instance ships its access and command logs to the SIEM, with its own API secret and web credentials.
- **Promotion order (optional):** branch protection requires the staging instance's status before merging.

**Rollout.**

1. Implement instance mode (issue, then pull request). Add CODEOWNERS and branch protection, and choose the mapping layout.
2. Deploy dev and staging with instance mode alongside the current setup, and compare plans.
3. Deploy prod with the settings above, moving one prod project group at a time.
4. Retire the shared instance and its broad credentials.

**Alternatives considered.**

- **One instance with per-project requirements and team allowlists.** Every environment's credentials stay in one place, and pull request code can still run with prod credentials during plan.
- **A separate opt-in flag per gap.** It closes the same gaps, but an operator who misses one flag reopens a gap. One instance block is harder to half-configure.
- **Reading the repo config from the base branch.** It stops a pull request from changing its instance's projects (G3, G4), but not G1 or G2, and a new project isn't planned until its config change is merged.
- **Projects defined only in server-side config.** Every project change becomes a platform deployment.
- **One command name with an `-x <instance>` flag.** A forgotten or mistyped `-x` silently does nothing. Distinct executable names already work.

## Consequences

- A pull request can only reach the environment whose instance plans it. Pull request code doesn't run with prod credentials before code-owner review, through plans or workflow hooks.
- Prod gets stricter rules than dev: approvers, policies and no repo-side overrides.
- Three instances to run, upgrade and monitor, with three GitHub Apps and three sets of credentials.
- Engineers use the instances' own command names. A comment with the old `atlantis` name, or with `run`, gets no response once the shared instance is retired.
- Prod depends on one opt-in fork feature, instance mode. Without it, G1–G4 stay open.
- Without `allowed_dirs`, keeping an instance to its environment's directories depends on review, CI or credential scoping. A pull request can still add projects inside its own environment; that stays gated by review.
- Follow-ups, each needing its own issue:
  - separate plan and apply credentials, since native `env` applies to every step and prod can't plan with a read-only role;
  - caller identity (ADR 0008), since API and UI actions share one identity until then. Leave the API off on prod meanwhile.
