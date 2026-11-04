# 7. Bounded metric labels and histograms

Date: 2026-10-30

## Status

Proposed

## Context

Atlantis exports metrics through tally to Prometheus or statsd. Three properties of the current metrics get worse as a multi-replica deployment grows. See issue #28.

- **Unbounded labels.** Project command metrics are labelled with `base_repo`, `pr_number`, `project`, `project_path`, `workspace` and `terraform_version` (`ProjectContext.SetProjectScopeTags`). Every pull request creates new series that never go away. tally's Prometheus reporter keeps each registered series for the life of the process. Each replica's memory and scrape size therefore grow with the number of pull requests it has served, and Prometheus pays for every series.
- **Summaries.** Timers are exported as Prometheus summaries, tally's default (`tallyprom.Options{}`). A summary's quantiles are computed per process and can't be combined. With several replicas there's no correct cluster-wide p95, and no basis for an SLO.
- **No load signal.** Nothing reports how busy a replica is. The chart's HPA scales on CPU, but Terraform runs are bursty and a replica waiting on providers looks idle. A replica can be at its `--parallel-pool-size` limit with low CPU.

Per-pull detail is still needed for debugging and audit. It belongs in logs, traces and the audit events of ADR 0006 (#23), all of which carry `repo`, `pull`, `project` and the trace ID.

## Decision

**1. Bounded labels.** Every metric label must have a value set fixed by the code or by configuration, never one taken from a pull request.

- Project command metrics use the labels `command`, `outcome` (`success`, `error` or `failure`) and `tool` (`terraform`, `terragrunt` or `cdktn`).
- `pr_number`, `project_path`, `workspace` and `terraform_version` are removed.
- `repo` and `project` are available only by opting in, through a server-side config key `metrics.labels: [repo, project]`. They are for small installations that want per-repo or per-project numbers and accept the cost. `pr_number` and `project_path` can't be re-enabled.

**2. Histograms for durations.**
- **Reporter:** the Prometheus reporter is created with `DefaultTimerType: HistogramTimerType`.
- **Long buckets,** for commands and projects: 1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600 and 7200 seconds.
- **Short buckets,** for VCS, Kubernetes API, S3 and forwarding calls: 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 and 30 seconds.
- **Statsd:** timers stay timers.

**3. Names and units.** New and changed metrics follow the Prometheus conventions:
- the `atlantis_` namespace (`--stats-namespace`);
- base units with a suffix (`_seconds`, `_bytes`);
- counters end in `_total`;
- ratios are 0–1 gauges.

Durations move to new names, for example `atlantis_project_command_duration_seconds`, rather than changing type under the old `execution_time` names. That way the old and new series never share a name.

**4. A load signal for scaling.** Each replica exports:
- `atlantis_project_commands_running` (gauge);
- `atlantis_project_command_pool_utilization` (gauge, running divided by `--parallel-pool-size`);
- `atlantis_operations_in_flight` (gauge, from the drainer).

The chart gains an optional HPA metric on pool utilization, served through prometheus-adapter as a Pods metric, alongside CPU. CPU remains the default because it needs no adapter.

**5. Transition.**
- **Existing counters that break the naming rule are renamed in the same release.** The cluster and plan risk counters added for Kubernetes-native Atlantis already have bounded labels, but lack the `_total` suffix:
  - `atlantis_cluster_routing_*`: `forward_success`, `forward_error`, `forward_ambiguous`, `local`, `plan_holder_wait`, `plan_holder_timeout` and `peer_cleanup_error`;
  - `atlantis_cluster_pull_lock_lost`;
  - `atlantis_plan_risk_error`.

  Each gains `_total`. The cluster gauges (`members`, `leader`, `api_healthy`, `identity_conflict`) already follow the conventions and keep their names. The chart's alert rules that use a renamed counter (`AtlantisForwardingErrors`, `AtlantisPlanRiskErrors`) change with it, and the chart's `promtool` tests cover them.
- **The first minor release with this change** emits only the new metrics by default. A server-side config key, `metrics.legacy: true`, also emits the old names, labels and summaries, so dashboards can be migrated.
- **The next minor release** removes the old metrics and the key. The release notes list each old metric and its replacement, and the docs gain a metrics reference page.

**Alternatives considered:**

- **Keep `pr_number` and rely on Prometheus `metric_relabel_configs` to drop it.** The series are still created and held in each replica's memory, and every operator has to know to do this.
- **Expire idle series in the reporter.** tally's Prometheus reporter can't unregister series, so this needs a custom reporter. The labels stay unbounded between expiries.
- **Native histograms only.** These need Prometheus 2.40+ with a feature flag, and newer exposition formats. Classic buckets work everywhere; native histograms can be added later.
- **Scale on CPU only.** It ignores the pool limit and network-bound work. The chart keeps it as the default, with pool utilization as an option.

## Consequences

- **Bounded cost.** Memory and scrape size stay constant regardless of how many pull requests a replica serves. Durations can be aggregated across replicas, and SLOs and HPA can use them.
- **Breaking for dashboards and alerts** that use the removed labels or the summary quantiles. `metrics.legacy` gives one release to migrate, and the release notes map old metrics to new ones.
- **Per-PR or per-project timing** now comes from logs, traces and audit events, unless `metrics.labels` opts in to repo and project labels.
- **Statsd** loses nothing: its reporter already dropped the tags, and its timers are unchanged.
- **Follow-ups (#28):**
  - add the remaining scaling and reliability metrics under these conventions;
  - add alerts for pool saturation and a nearly full data volume, with `promtool` tests in the chart CI.

  The chart's membership alert for HPA (P3) is already fixed by #44, independently of this ADR.
