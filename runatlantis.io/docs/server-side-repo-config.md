# Server Side Repo Config

A Server-Side Config file is used for more groups of server config that can't reasonably be expressed through flags.

One such usecase is to control per-repo behaviour
and what users can do in repo-level `atlantis.yaml` files.

## Do I Need A Server-Side Config File?

You do not need a server-side repo config file unless you want to customize
some aspect of Atlantis on a per-repo basis.

Read through the [use-cases](#use-cases) to determine if you need it.

## Enabling Server Side Config

To use server side repo config create a config file, ex. `repos.yaml`, and pass it to
the `atlantis server` command via the `--repo-config` flag, ex. `--repo-config=path/to/repos.yaml`.

If you don't wish to write a config file to disk, you can use the
`--repo-config-json` flag or `ATLANTIS_REPO_CONFIG_JSON` environment variable
to specify your config as JSON. See [--repo-config-json](server-configuration.md#repo-config-json)
for an example.

## Example Server Side Repo

```yaml
# repos lists the config for specific repos.
repos:
  # id can either be an exact repo ID or a regex.
  # If using a regex, it must start and end with a slash.
  # Repo ID's are of the form {VCS hostname}/{org}/{repo name}, ex.
  # github.com/runatlantis/atlantis.
- id: /.*/
  # branch is a regex matching pull requests by base branch
  # (the branch the pull request is getting merged into).
  # By default, all branches are matched
  branch: /.*/

  # repo_config_file specifies which repo config file to use for this repo.
  # By default, atlantis.yaml is used.
  repo_config_file: path/to/atlantis.yaml

  # plan_requirements sets the Plan Requirements for all repos that match.
  plan_requirements: [approved, mergeable, undiverged]

  # apply_requirements sets the Apply Requirements for all repos that match.
  apply_requirements: [approved, mergeable, undiverged]

  # import_requirements sets the Import Requirements for all repos that match.
  import_requirements: [approved, mergeable, undiverged]

  # inputs configures the built-in steps for all repos that match.
  # See Native Inputs below.
  inputs:
    var_files: [env/prod.tfvars]
    extra_args:
      plan: ["-lock=false"]

  # tool runs the built-in steps with terraform (default), terragrunt or cdktn.
  tool: terraform

  # allowed_overrides specifies which keys can be overridden by this repo in
  # its atlantis.yaml file.
  allowed_overrides: [apply_requirements, delete_source_branch_on_merge, repo_locking, repo_locks, inputs, tool]

  # delete_source_branch_on_merge defines whether the source branch would be deleted on merge
  # If false (default), the source branch won't be deleted on merge
  delete_source_branch_on_merge: true

  # repo_locking defines whether lock repository when planning.
  # If true (default), atlantis try to get a lock.
  # deprecated: use repo_locks instead
  repo_locking: true

  # repo_locks defines whether the repository would be locked on apply instead of plan, or disabled
  # Valid values are on_plan (default), on_apply or disabled.
  repo_locks:
    mode: on_plan

  # pre_workflow_hooks defines arbitrary list of scripts to execute in the clone before Atlantis runs a command.
  pre_workflow_hooks:
    - run: my-pre-workflow-hook-command arg1

  # post_workflow_hooks defines arbitrary list of scripts to execute in the clone after Atlantis runs a command.
  post_workflow_hooks:
    - run: my-post-workflow-hook-command arg1

  # policy_check defines if policy checking should be enabled on this repository.
  policy_check: false

  # autodiscover defines how atlantis should automatically discover projects in this repository.
  # If any part of this setting is set here, it overrides the entire setting in the repo config.
  autodiscover:
    mode: auto
    # Optionally ignore some paths for autodiscovery by a glob path.
    # When autodiscovery is enabled, also applies to all targeted -d commands
    # (plan, apply, import, etc.) when the path has no explicit project configuration.
    ignore_paths:
      - foo/*

  # id can also be an exact match.
- id: github.com/myorg/specific-repo
```

## Use Cases

Here are some of the reasons you might want to use a repo config.

### Requiring PR Is Approved Before an applicable subcommand

If you want to require that all (or specific) repos must have pull requests
approved before Atlantis will allow running `apply` or `import`, use the `plan_requirements`, `apply_requirements` or `import_requirements` keys.

For all repos:

```yaml
# repos.yaml
repos:
- id: /.*/
  plan_requirements: [approved]
  apply_requirements: [approved]
  import_requirements: [approved]
```

For a specific repo:

```yaml
# repos.yaml
repos:
- id: github.com/myorg/myrepo
  plan_requirements: [approved]
  apply_requirements: [approved]
  import_requirements: [approved]
```

See [Command Requirements](command-requirements.md) for more details.

### Requiring PR Is "Mergeable" Before Apply or Import

If you want to require that all (or specific) repos must have pull requests
in a mergeable state before Atlantis will allow running `apply` or `import`, use the `plan_requirements`, `apply_requirements` or `import_requirements` keys.

For all repos:

```yaml
# repos.yaml
repos:
- id: /.*/
  plan_requirements: [mergeable]
  apply_requirements: [mergeable]
  import_requirements: [mergeable]
```

For a specific repo:

```yaml
# repos.yaml
repos:
- id: github.com/myorg/myrepo
  plan_requirements: [mergeable]
  apply_requirements: [mergeable]
  import_requirements: [mergeable]
```

See [Command Requirements](command-requirements.md) for more details.

### Repos Can Set Their Own Apply Requirements

If you want all (or specific) repos to be able to override the default apply requirements, use
the `allowed_overrides` key.

To allow all repos to override the default:

```yaml
# repos.yaml
repos:
- id: /.*/
  # The default will be approved.
  plan_requirements: [approved]
  apply_requirements: [approved]
  import_requirements: [approved]

  # But all repos can set their own using atlantis.yaml
  allowed_overrides: [plan_requirements, apply_requirements, import_requirements]
```

To allow only a specific repo to override the default:

```yaml
# repos.yaml
repos:
# Set a default for all repos.
- id: /.*/
  plan_requirements: [approved]
  apply_requirements: [approved]
  import_requirements: [approved]

# Allow a specific repo to override.
- id: github.com/myorg/myrepo
  allowed_overrides: [plan_requirements, apply_requirements, import_requirements]
```

Then each allowed repo can have an `atlantis.yaml` file that
sets `plan_requirements`, `apply_requirements` or `import_requirements` to an empty array (disabling the requirement).

```yaml
# atlantis.yaml in the repo root or set repo_config_file in repos.yaml
version: 3
projects:
- dir: .
  plan_requirements: []
  apply_requirements: []
  import_requirements: []
```

### Native Inputs

`inputs` configures the built-in steps (`init`, `plan`, `apply` and the others). Set it on a server-side repo entry as the default, and list `inputs` in `allowed_overrides` to let projects in `atlantis.yaml` override it (see [Repos Can Set Their Own Native Inputs Or Tool](#repos-can-set-their-own-native-inputs-or-tool) for how the two combine).

```yaml
repos:
  - id: /.*/
    allowed_overrides: [inputs]
    inputs:
      var_files: [env/prod.tfvars]          # plan/import: -var-file, in order
      vars:                                 # plan/import: -var, sorted by name
        region: eu-west-1
      backend_config: [prod.backend.hcl]    # init: -backend-config, plus -reconfigure
      env:                                  # every step
        TF_AWS_DEFAULT_TAGS_repository: "github.com/${BASE_REPO_OWNER}/${BASE_REPO_NAME}"
      extra_args:                           # appended to the built-in step
        plan: [-lock-timeout=5m]
```

- Paths are relative to the project directory and may not point outside the repository.
- `backend_config` entries are files or `key=value` pairs. `init` also gets `-reconfigure`, so projects that switch backends between environments need no cleanup step.
- `env` values may use `${BASE_REPO_OWNER}`, `${BASE_REPO_NAME}`, `${REPO_REL_DIR}`, `${WORKSPACE}`, `${PROJECT_NAME}`, `${PULL_NUM}` and `${HEAD_COMMIT}`.
- `extra_args` keys are built-in steps: `init`, `plan`, `apply`, `show`, `policy_check`, `import`, `state_rm`.

### OpenTofu

Set `terraform_distribution: opentofu` to run [OpenTofu](https://opentofu.org) instead of Terraform for matching repos. A project's own `terraform_distribution` in `atlantis.yaml` takes precedence, and the server-wide default is [`--default-tf-distribution`](server-configuration.md#default-tf-distribution).

```yaml
repos:
  - id: /my-org/infra-.*/
    terraform_distribution: opentofu
```

- It works with every `tool`: Terragrunt and CDK Terrain projects run the OpenTofu binary Atlantis selects.
- The version comes from `terraform_version` or `required_version` (read from `.tofu` files too). Without either, a project on the distribution that is not the server default uses that distribution's binary on `PATH`, or its latest release when downloads are allowed. Atlantis never uses a Terraform version number for OpenTofu, or the other way round.
- Changes to `.tofu` and `.tofu.json` files trigger autoplan.

### Terragrunt

::: warning Experimental
Terragrunt support, including unit discovery, is experimental: its configuration and behaviour may change in a later release without a deprecation period.
:::

Set `tool: terragrunt` to run the built-in steps with [Terragrunt](https://terragrunt.gruntwork.io) instead of Terraform. List `tool` in `allowed_overrides` to let projects in `atlantis.yaml` choose.

```yaml
repos:
  - id: /.*/
    allowed_overrides: [tool]
    tool: terragrunt
```

- Every built-in step runs as `terragrunt <command>` with the same arguments, so `inputs`, comment arguments, policy checks and plan files work as they do for Terraform.
- Terragrunt runs the Terraform or OpenTofu binary Atlantis selects for the project (`terraform_version`, `terraform_distribution`) through `TG_TF_PATH`.
- Atlantis sets `TG_NON_INTERACTIVE=true`, `TG_TF_FORWARD_STDOUT=true`, `TG_LOG_LEVEL=error` and `TG_NO_COLOR=true` so the output in pull request comments is Terraform's own. `inputs.env` can override them.
- The `terragrunt` binary must be on `PATH`. The Atlantis image includes it on amd64 and arm64.
- Each project is one Terragrunt unit: a directory with a `terragrunt.hcl`. `run --all` is not used.

#### Terragrunt Unit Discovery

When the server-side `tool` of a repo is `terragrunt` and [autodiscovery](repo-level-atlantis-yaml.md#autodiscovery-config) is on (the default when `atlantis.yaml` lists no projects), Atlantis runs `terragrunt find` on each pull request and makes every unit a project. No `atlantis.yaml` or terragrunt-atlantis-config is needed.

`terragrunt find` evaluates the pull request's configuration, including `run_cmd`. When the server-side `plan_requirements` include `approved`, Atlantis does not run it until the pull request is approved, so autoplan on an unapproved pull request reports that approval is needed. Every command that discovers projects checks the approval again when it runs, so dismissing the approval stops discovery, and if Atlantis cannot fetch the pull request's approval from the VCS host the command fails and can be retried. The same applies to CDK Terrain stack discovery.

- A `terragrunt.hcl` that other units include (a parent config) is not a project.
- A unit autoplans when a file changes in its directory, in a file it reads (included configs, `read_terragrunt_config`, its `terraform.source` module), or in any of those for a unit it depends on through `dependency` or `dependencies` blocks, directly or not.
- Units plan and apply after the units they depend on, through [execution order groups](repo-level-atlantis-yaml.md#order-of-planning-applying). A unit planned before its dependency is applied uses the dependency's `mock_outputs`; plan it again after the apply.
- Projects in `atlantis.yaml` keep their own settings; units in the same directory are not added again. With `autodiscover.mode: enabled`, units are added next to the configured projects. `autodiscover.ignore_paths` applies to units too.
- Directory-based autodiscovery of `.tf` files is off for these repos.
- If `terragrunt find` cannot read a unit's configuration it leaves out that unit's dependencies, and changes to them will not autoplan it.

A unit can adjust its project with `locals` in its `terragrunt.hcl`, named as in terragrunt-atlantis-config. Values must be literals, because Atlantis reads them without evaluating the configuration:

```hcl
locals {
  atlantis_skip               = true                       # not a project
  atlantis_autoplan           = false                      # plan only on request
  atlantis_terraform_version  = "1.9.8"                    # pin the version
  extra_atlantis_dependencies = ["../../policies/*.json"]  # more paths that autoplan it, relative to the unit
}
```

Other terragrunt-atlantis-config locals, such as `atlantis_workflow`, are ignored.


### CDK Terrain

::: warning Experimental
CDK Terrain support, including stack discovery, is experimental: its configuration and behaviour may change in a later release without a deprecation period.
:::

Set `tool: cdktn` to plan and apply [CDK Terrain](https://github.com/open-constructs/cdk-terrain) apps, the community continuation of CDK for Terraform. A CDK Terrain app is a directory with a `cdktf.json`.

```yaml
repos:
  - id: /.*/
    tool: cdktn
```

- Before running Terraform for a project, Atlantis synthesizes its app once per commit with `cdktn synth --output cdktf.out`, then runs the built-in steps in the stack's directory under `cdktf.out`. Plan files stay in the project directory, so `inputs.vars`, `inputs.env`, `extra_args`, comment arguments, policy checks and plan stores work as they do for Terraform.
- Relative paths in `inputs.var_files` and `inputs.backend_config` resolve from the synthesized stack directory, not the app.
- If the app has a `package.json` and no `node_modules`, Atlantis runs `npm ci` (or `npm install` without a lock file) first, with `--ignore-scripts`. Other languages need their dependencies available in the image.
- The `cdktn` binary and Node must be on `PATH`. The full Atlantis images include them; the `-slim` images do not.

#### CDK Terrain Stack Discovery

When autodiscovery is on, Atlantis finds every `cdktf.json` (outside `node_modules` and `cdktf.out`), synthesizes the app and makes each stack a project. Synthesizing runs the app's code, so with `plan_requirements: [approved]` on the server it waits until the pull request is approved, as for [Terragrunt unit discovery](#terragrunt-unit-discovery).

- The project is named after the stack, prefixed with the app directory unless the app is at the repository root, and plans with `atlantis plan -p <name>`.
- A stack autoplans when any file of its app changes, except `cdktf.out`, `node_modules` and apps nested inside it.
- Stacks plan and apply after the stacks they depend on, from cross-stack references or `addDependency`. A cross-stack reference reads the other stack's state, so plan the dependent stack again after the first apply.

To configure stacks by hand, set `stack` on a project in `atlantis.yaml`; it may be left out when the app has a single stack:

```yaml
version: 3
projects:
- name: prod-network
  dir: infra
  stack: network
```

### Migrating Custom Workflows To Native Inputs

::: warning Experimental
`atlantis migrate-workflows` is experimental: its configuration and behaviour may change in a later release without a deprecation period.
:::

`atlantis migrate-workflows` converts custom workflows and custom policy checks into `inputs`, and keeps workflow hooks:

```bash
atlantis migrate-workflows --repos-yaml repos.yaml --atlantis-yaml atlantis.yaml --report migration.md
# review the printed files, then:
atlantis migrate-workflows --repos-yaml repos.yaml --atlantis-yaml atlantis.yaml --write
```

- Built-in steps and their arguments, fixed env values, and env values built from `echo "...$BASE_REPO_NAME..."` become inputs.
- `rm -rf .terraform`, `terraform workspace select $WORKSPACE` and `echo` placeholders are dropped: native workflows already cover them.
- `run: terraform plan ...` style commands become the built-in step with extra arguments.
- `tofu ...` commands become built-in steps with `terraform_distribution: opentofu`, and so does Terragrunt pointed at `tofu` (`TG_TF_PATH` / `TERRAGRUNT_TFPATH`).
- Other custom commands, `multienv` and `custom_policy_check` are removed and listed in the report. Commands that must run in Atlantis's clone of the pull request, such as decrypting secrets, rendering backend files or running Infracost on the plan, belong in server-side [pre-workflow](pre-workflow-hooks.md) or [post-workflow](post-workflow-hooks.md) hooks, which the tool keeps. With a TypeSafe API key (`ATLANTIS_TYPESAFE_API_KEY`), Jev labels each removed command (wrapper tool, policy scanner, cost estimation, credentials, ...) so the report says what replaces it. Labels below 0.8 confidence are marked uncertain. Labels never change the converted files.
- A server-side workflow named `default` becomes a first catch-all repo entry with inputs.
- In `atlantis.yaml`, env names and arguments that repo config cannot set (see [the repo-side limits](#repos-can-set-their-own-native-inputs-or-tool)) are left out and reported as removed; set them in server-side `inputs` instead.
- `--write` keeps the originals as `.orig`; `--strict` exits non-zero if anything was removed or needs review.

### Running Scripts Before Atlantis Workflows

If you want to run scripts in Atlantis's clone before it runs a command, for
example to decrypt secrets or render backend files, you can create
`pre_workflow_hooks`:

```yaml
repos:
  - id: /.*/
    pre_workflow_hooks:
      - run: my custom command
      - run: |
          my bash script inline
```

See [Pre Workflow Hooks](pre-workflow-hooks.md) for more details on writing
pre workflow hooks.

### Running Scripts After Atlantis Workflows

If you want to run scripts in Atlantis's clone after it runs a command, for
example to estimate cost from the plan, you can create `post_workflow_hooks`:

```yaml
repos:
  - id: /.*/
    post_workflow_hooks:
      - run: my custom command
      - run: |
          my bash script inline
```

See [Post Workflow Hooks](post-workflow-hooks.md) for more details on writing
post workflow hooks.

### Repos Can Set Their Own Native Inputs Or Tool

To let repos set `inputs` or `tool` on their projects in `atlantis.yaml`, list
the keys in `allowed_overrides`:

```yaml
# repos.yaml
repos:
- id: /.*/
  inputs:
    var_files: [default.tfvars]
  allowed_overrides: [inputs, tool]
```

```yaml
# atlantis.yaml
version: 3
projects:
- dir: envs/prod
  inputs:
    var_files: [prod.tfvars]   # replaces the server-side var_files
- dir: live/vpc
  tool: terragrunt
```

Project inputs combine with the server-side ones:

- `var_files`, `vars` and `backend_config` set in a project replace the server-side value.
- `env` is merged by name; a project value wins for a name both set.
- `extra_args` is merged by step. A project's arguments are added after the server-side ones for the same step, so a project cannot remove an argument the server passes.
- An argument the server already passes is not added twice. Atlantis compares whole arguments: a single token such as `-lock-timeout=5m`, or a flag and its value given as two tokens, such as `-target module.a`. So the same flag with a different value (`-target module.b`) is kept. A value that starts with `-`, such as `-1`, isn't recognized as a value; write such arguments in the `-flag=value` form (`-parallelism=-1`).

Repo config is written by pull request authors, so its inputs are limited:

- Repo-side `vars`, `var_files`, `backend_config` and `extra_args` can reference only the Atlantis context variables: `$WORKSPACE`, `$DIR`, `$ATLANTIS_TERRAFORM_VERSION`, `$BASE_REPO_OWNER`, `$BASE_REPO_NAME`, `$REPO_REL_DIR`, `$PROJECT_NAME`, `$PULL_NUM` and `$HEAD_COMMIT` (also written `${NAME}`). Any other reference, such as `$AWS_REGION`, is passed to Terraform as written, never expanded from the Atlantis server's environment. Server-side inputs can reference any environment variable.
- Repo-side `env` cannot set variables that change which programs or plugins run, or Atlantis's own settings: `PATH`, `BASH_ENV`, `ENV`, `HOME`, `NODE_OPTIONS`, `TF_CLI_CONFIG_FILE`, `TERRAFORM_CONFIG`, `TF_PLUGIN_CACHE_DIR`, `TF_DATA_DIR`, `XDG_DATA_HOME`, `XDG_DATA_DIRS`, `TG_TF_PATH`, `TERRAGRUNT_TFPATH`, or names starting with `LD_`, `DYLD_`, `GIT_`, `ATLANTIS_` or `TF_CLI_ARGS`. It also cannot set variables that redirect provider traffic or credentials: `HTTPS_PROXY`, `HTTP_PROXY`, `ALL_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `NODE_EXTRA_CA_CERTS`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE` or `TF_REATTACH_PROVIDERS`. Names are matched without regard to case.
- This list is defence in depth, not a sandbox: `terraform plan` runs code from the pull request, for example through `data "external"`. Require approval before planning (`plan_requirements: [approved]`) and grant `allowed_overrides: [inputs]` only to repos whose authors you trust with the server's credentials.
- Repo-side `extra_args` cannot set the `policy_check` key, even to an empty list, and `extra_args.init` cannot pass `-plugin-dir`.

Set these in server-side `inputs` instead.

### Removed Keys

Custom workflows were replaced by native inputs and `tool`, and Conftest is the
only policy engine. These keys are rejected with an error that names the key:

- `workflows`, and `workflow` on repo entries
- `allowed_workflows` and `allow_custom_workflows`
- `custom_policy_check`
- `workflow` and `custom_policy_check` in `allowed_overrides`

In `atlantis.yaml`, `workflows` and the project keys `workflow` and
`custom_policy_check` are rejected too. `atlantis migrate-workflows` converts
them; see [Migrating Custom Workflows To Native Inputs](#migrating-custom-workflows-to-native-inputs).

### Multiple Atlantis Servers Handle The Same Repository

Running multiple Atlantis servers to handle the same repository can be done to separate permissions for each Atlantis server.
In this case, a different [atlantis.yaml](repo-level-atlantis-yaml.md) repository config file can be used by using different `repos.yaml` files.

For example, consider a situation where a separate `production-server` atlantis uses repo config `atlantis-production.yaml` and `staging-server` atlantis uses repo config `atlantis-staging.yaml`.

Firstly, deploy 2 Atlantis servers, `production-server` and `staging-server`.
Each server has different permissions and a different `repos.yaml` file.
The `repos.yaml` contains `repo_config_file` key to specify the repository atlantis config file path.

```yaml
# repos.yaml
repos:
- id: /.*/
  # for production-server
  repo_config_file: atlantis-production.yaml
  # for staging-server
  # repo_config_file: atlantis-staging.yaml
```

Then, create `atlantis-production.yaml` and `atlantis-staging.yaml` files in the repository.
See the configuration examples in [atlantis.yaml](repo-level-atlantis-yaml.md).

```yaml
# atlantis-production.yaml
version: 3
projects:
- name: project
  branch: /production/
  dir: infrastructure/production
---
# atlantis-staging.yaml
version: 3
projects:
  - name: project
    branch: /staging/
    dir: infrastructure/staging
```

Now, 2 webhook URLs can be setup for the repository, which send events to `production-server` and `staging-server` respectively.
Each servers handle different repository config files.

:::tip Notes

* If `no projects` comments are annoying, set [--silence-no-projects](server-configuration.md#silence-no-projects).
* The command trigger executable name can be reconfigured from `atlantis` to something else by setting [Executable Name](server-configuration.md#executable-name).
* When using different atlantis server vcs users such as `@atlantis-staging`, the comment `@atlantis-staging plan` can be used instead `atlantis plan` to call `staging-server` only.
:::

## Reference

### Top-Level Keys

| Key        | Type                                                  | Default   | Required | Description                                                                           |
|------------|-------------------------------------------------------|-----------|----------|---------------------------------------------------------------------------------------|
| repos      | array[[Repo](#repo)]                                  | see below | no       | List of repos to apply settings to.                                                   |
| policies   | Policies.                                             | none      | no       | List of policy sets to run and associated metadata                                    |
| metrics    | Metrics.                                              | none      | no       | Map of metric configuration                                                           |
| team_authz | [TeamAuthz](#teamauthz)                               | none      | no       | Configuration of team permission checking                                             |

::: tip A Note On Defaults

#### `repos`

`repos` always contains a first element with the Atlantis default config:

```yaml
repos:
- id: /.*/
  branch: /.*/
  plan_requirements: []
  apply_requirements: []
  import_requirements: []
  allowed_overrides: []
  tool: terraform
```

:::

### Repo

| Key | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| id | string | none | yes | Value can be a regular expression when specified as /&lt;regex&gt;/ or an exact string match. Repo IDs are of the form `{vcs hostname}/{org}/{name}`, ex. `github.com/owner/repo`. Hostname is specified without scheme or port. For Bitbucket Server, {org} is the **name** of the project, not the key. |
| branch | string | none | no | An regex matching pull requests by base branch (the branch the pull request is getting merged into). By default, all branches are matched |
| repo_config_file | string | none | no | Repo config file path in this repo. By default, use `atlantis.yaml` which is located on repository root. When multiple atlantis servers work with the same repo, please set different file names. |
| pre_workflow_hooks | [][WorkflowHook](pre-workflow-hooks.md#reference) | none | no | Scripts run in Atlantis's clone before a command. See [Pre Workflow Hooks](pre-workflow-hooks.md). |
| post_workflow_hooks | [][WorkflowHook](post-workflow-hooks.md#reference) | none | no | Scripts run in Atlantis's clone after a command. See [Post Workflow Hooks](post-workflow-hooks.md). |
| inputs | [Inputs](#native-inputs) | none | no | Native inputs for the built-in steps: `var_files`, `vars`, `backend_config`, `env`, `extra_args`. |
| tool | string | `terraform` | no | Runs the built-in steps with `terraform`, [`terragrunt`](#terragrunt) or [`cdktn`](#cdk-terrain). |
| terraform_distribution | string | `--default-tf-distribution` | no | `terraform` or [`opentofu`](#opentofu). A project's `terraform_distribution` in `atlantis.yaml` takes precedence. |
| plan_requirements | []string | none | no | Requirements that must be satisfied before `atlantis plan` can be run. Currently the only supported requirements are `approved`, `mergeable`, and `undiverged`. See [Command Requirements](command-requirements.md) for more details. |
| apply_requirements | []string | none | no | Requirements that must be satisfied before `atlantis apply` can be run. Currently the only supported requirements are `approved`, `mergeable`, and `undiverged`. See [Command Requirements](command-requirements.md) for more details. |
| import_requirements | []string | none | no | Requirements that must be satisfied before `atlantis import` can be run. Currently the only supported requirements are `approved`, `mergeable`, and `undiverged`. See [Command Requirements](command-requirements.md) for more details. |
| allowed_overrides | []string | none | no | A list of restricted keys that `atlantis.yaml` files can override. The supported keys are `plan_requirements`, `apply_requirements`, `import_requirements`, `delete_source_branch_on_merge`, `repo_locking`, `repo_locks`, `policy_check`, `silence_pr_comments`, `inputs` and `tool`. |
| delete_source_branch_on_merge | bool | false | no | Whether or not to delete the source branch on merge. |
| repo_locking | bool | false | no | (deprecated) Whether or not to get a lock. |
| repo_locks | [RepoLocks](#repolocks) | `mode: on_plan` | no | Whether or not repository locks are enabled for this project on plan or apply. See [RepoLocks](#repolocks) for more details. |
| policy_check | bool | false | no | Whether or not to run policy checks on this repository. |
| autodiscover | AutoDiscover | none | no | Auto discover settings for this repo |
| silence_pr_comments | []string | none | no | Silence PR comments from defined stages while preserving PR status checks. Useful in large environments with many Atlantis instances and/or projects, when the comments are too big and too many, therefore it is preferable to rely solely on PR status checks. Supported values are: `plan`, `apply`. |

:::tip Notes

* If multiple repos match, the last match will apply.
* If a key isn't defined, it won't override a key that matched from above.
  For example, given a repo ID `github.com/owner/repo` and a config:

  ```yaml
  repos:
  - id: /.*/
    tool: terragrunt
    apply_requirements: [approved]
  - id: github.com/owner/repo
    apply_requirements: []
  ```

  The final config will look like:

  ```yaml
  apply_requirements: []
  allowed_overrides: []
  tool: terragrunt
  ```

  Where
  * `apply_requirements` is set from the `id: github.com/owner/repo` config because
    it overrides the previous matching config from `id: /.*/`.
  * `allowed_overrides` is set from the default config that always
    exists.
  * `tool` is set from the `id: /.*/` config and isn't unset
    by the `id: github.com/owner/repo` config because it didn't define that key.
:::

### RepoLocks

```yaml
mode: on_apply
```

| Key  | Type   | Default   | Required | Description                                                                                                                           |
|------|--------|-----------|----------|---------------------------------------------------------------------------------------------------------------------------------------|
| mode | `Mode` | `on_plan` | no       | Whether or not repository locks are enabled for this project on plan or apply. Valid values are `disabled`, `on_plan` and `on_apply`. |

### Policies

| Key | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| conftest_version | string | none | no | conftest version to run all policy sets |
| owners | Owners(#Owners) | none | yes | owners that can approve failing policies |
| approve_count | int | 1 | no | number of approvals required to bypass failing policies. |
| sticky_policy_approvals | bool | false | no | when true, policy approvals survive re-plans as long as no new policy output items (per `policy_item_regex`) are introduced. See [Sticky Policy Approvals](policy-checking.md#sticky-policy-approvals). |
| policy_item_regex | string | `(?s).+` | no | regex to extract comparable items from policy output for sticky approval tracking. Default matches entire output as one item. See [Sticky Policy Approvals](policy-checking.md#sticky-policy-approvals). |
| policy_sets | []PolicySet | none | yes | set of policies to run on a plan output |

### Owners

| Key         | Type              | Default | Required   | Description                                             |
|-------------|-------------------|---------|------------|---------------------------------------------------------|
| users       | []string          | none    | no         | list of github users that can approve failing policies  |
| teams       | []string          | none    | no         | list of github teams that can approve failing policies  |

### PolicySet

| Key                      | Type   | Default   | Required | Description                                                                                                                                               |
|--------------------------|--------|-----------|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| name                     | string | none      | yes      | unique name for the policy set                                                                                                                            |
| path                     | string | none      | yes      | path to the rego policies directory                                                                                                                       |
| source                   | string | none      | yes      | only `local` is supported at this time                                                                                                                    |
| owners                   | Owners | none      | no       | owners that can approve this specific policy set (merged with top-level owners)                                                                           |
| approve_count            | int    | inherited | no       | number of approvals required. Defaults to the top-level `approve_count` value                                                                             |
| prevent_self_approve     | bool   | false     | no       | whether the PR author can approve policies. Defaults to `false` (the author must also be in owners)                                                       |
| sticky_policy_approvals  | bool   | inherited | no       | overrides the top-level `sticky_policy_approvals` for this policy set. See [Sticky Policy Approvals](policy-checking.md#sticky-policy-approvals).         |
| policy_item_regex        | string | inherited | no       | overrides the top-level `policy_item_regex` for this policy set. See [Sticky Policy Approvals](policy-checking.md#sticky-policy-approvals).               |

### Metrics

| Key                    | Type                      | Default | Required  | Description                              |
|------------------------|---------------------------|---------|-----------|------------------------------------------|
| statsd                 | [Statsd](#statsd)         | none    | no        | Statsd metrics provider                  |
| prometheus             | [Prometheus](#prometheus) | none    | no        | Prometheus metrics provider              |

### Statsd

| Key    | Type   | Default | Required | Description                            |
| ------ | ------ | ------- | -------- | -------------------------------------- |
| host   | string | none    | yes      | statsd host ip address                 |
| port   | string | none    | yes      | statsd port                            |

### Prometheus

| Key      | Type   | Default | Required | Description                            |
| -------- | ------ | ------- | -------- | -------------------------------------- |
| endpoint | string | none    | yes      | path to metrics endpoint               |

### TeamAuthz

| Key     | Type     | Default | Required | Description                                 |
|---------|----------|---------|----------|---------------------------------------------|
| command | string   | none    | yes      | full path to external authorization command |
| args    | []string | none    | no       | optional arguments to pass to `command`     |
