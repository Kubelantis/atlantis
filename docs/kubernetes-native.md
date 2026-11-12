# Kubernetes-native Atlantis

This fork runs Atlantis as a highly available StatefulSet. Locks, pull status,
membership and leader election are all stored as Kubernetes objects, so it
needs no BoltDB or Redis. Each replica has its own volume for clones and plan
files; configure a shared plan store (`externalStores`) so any replica can apply.
The design and its trade-offs are in [ADR 0003](adr/0003-kubernetes-native-ha.md).

## Install

```bash
kubectl create namespace atlantis
kubectl -n atlantis create secret generic atlantis-vcs \
  --from-literal=ATLANTIS_GH_USER=my-bot \
  --from-literal=ATLANTIS_GH_TOKEN=... \
  --from-literal=ATLANTIS_GH_WEBHOOK_SECRET=...
helm install atlantis deploy/helm/atlantis -n atlantis \
  --set vcsSecretName=atlantis-vcs \
  --set atlantisUrl=https://atlantis.example.com \
  --set repoAllowlist='github.com/example-org/*'
```

Atlantis runs as a StatefulSet: each replica has a stable name and its own
volume for clones and plans (`persistence.*`). The chart installs the
`PullStatus` CRD. It also installs a namespaced Role
limited to Leases and PullStatuses, a cluster token Secret, a
PodDisruptionBudget, and a NetworkPolicy that limits the cluster port to
Atlantis pods. Replicas are spread across nodes and zones. Optional pieces are
an Ingress, an HPA, a ServiceMonitor, a PrometheusRule and OTLP tracing.

Without Helm, the server flags are:

```bash
atlantis server \
  --cluster-token=$ATLANTIS_CLUSTER_TOKEN   # plus POD_NAME, POD_NAMESPACE, POD_IP from the downward API
```

## How it works

| Concern | Mechanism |
| --- | --- |
| Project locks, global apply lock | `Lease` objects named `lock-*` / `cmdlock-*`. Creating one acquires the lock atomically. |
| Plan/apply status per pull | `PullStatus` objects (`kubectl get pullstatuses`), updated with optimistic concurrency. |
| Which replica handles a pull | Member Leases (`member-*`) plus rendezvous hashing. Webhooks are forwarded to the owner over `--cluster-port`. |
| Two replicas on one pull | Per-pull Lease (`pulllock-*`), re-entrant within a replica and renewed while commands run. A replica stops trusting it after half the lease duration without a renewal, before anyone else may take it over; it then cancels the pull's queued work and refuses to start further steps. |
| Where applies run | On the replica that made the plan (`PullStatus.spec.plannedBy`), because the plan files are on its volume. If that replica is restarting, the apply waits up to `--cluster-plan-holder-wait-seconds` (default 180) for it to return. Skipped when an external plan store is configured. |
| Lost forwarding replies | Each forward has a request ID; retries reuse it and the owner runs it once. If delivery cannot be confirmed and the owner is alive, the command is not re-run elsewhere and the pull request gets a comment. |
| Readiness | Depends only on local state (starting or draining), never on the shared API server, so an API outage cannot remove every replica from the Service. `atlantis_cluster_api_healthy` reports API reachability. |
| Job pages and websockets | Proxied to whichever replica has the job. |
| Housekeeping | The leader (`atlantis-leader` Lease) deletes Leases left by crashed replicas. |
| Rolling update / scale down | The terminating replica marks itself draining. Its pulls move immediately, `/readyz` fails, and running jobs finish before it exits. |

To inspect the cluster state:

```bash
kubectl -n atlantis get leases -l app.kubernetes.io/managed-by=atlantis -L atlantis.runatlantis.io/type
kubectl -n atlantis get pullstatuses -o wide
kubectl -n atlantis get lease atlantis-leader -o jsonpath='{.spec.holderIdentity}'
```

Nothing written to the API server contains VCS credentials: clone URLs and
pull request bodies are stripped before storing.

## Plans and job logs

By default each replica keeps its clones, plan files and job logs on its own
volume:

- applies are routed to the replica that made the plan;
- live job output is proxied to the replica running the job;
- when a pull request closes, every replica deletes its copies. A replica
  that was down at the time removes them later: its janitor deletes the local
  files of pulls that no longer have a `PullStatus` and were untouched for 24
  hours.

To make plans and completed job logs available to every replica, and keep
them if a replica or its volume is lost, store them in object storage (S3 or
any S3-compatible service):

```yaml
# server-side repo config (chart: externalStores.planStore / .logStore)
external_stores:
  plan_store:
    type: s3
    s3:
      bucket: my-atlantis
      region: eu-west-1
      prefix: atlantis/plans
      server_side_encryption: aws:kms   # AES256, aws:kms or aws:kms:dsse
      kms_key_id: alias/atlantis        # optional, for aws:kms*
  log_store:
    type: s3
    s3:
      bucket: my-atlantis
      region: eu-west-1
      prefix: atlantis/job-logs
      server_side_encryption: AES256
```

together with `--enable-external-stores`. Then:

- applies run on the pull's owner and download the plan, and are refused if
  the plan was made at another commit;
- completed job logs are uploaded when the job finishes and can be opened
  through any replica; running jobs still stream from the replica running
  them;
- both are deleted when the pull request closes.

Plan files and job output can contain secrets: keep server-side encryption
on, restrict the bucket to Atlantis's workload identity (for example IRSA via
`serviceAccount.annotations`), and add a lifecycle rule to expire objects of
pull requests that were never closed.

## Monitoring

To expose metrics, keep the `metrics.prometheus` block in the chart's
`repoConfig`, then enable `metrics.serviceMonitor`. The metrics are:

| Metric | Meaning |
| --- | --- |
| `atlantis_cluster_members` | Live replicas seen by this replica |
| `atlantis_cluster_leader` | 1 on the leader |
| `atlantis_cluster_routing_forward_success` / `_forward_error` / `_forward_ambiguous` / `_local` | Command routing outcomes |
| `atlantis_cluster_routing_plan_holder_wait` / `_plan_holder_timeout` | Applies that waited for a restarting plan holder, and waits that gave up |
| `atlantis_cluster_api_healthy` | 1 when the last membership sync with the API server succeeded |
| `atlantis_cluster_pull_lock_lost` | Pull locks this replica lost to another replica |
| `atlantis_plan_risk_assessed{tier}` / `atlantis_plan_risk_error` | Plan risk results |
| `atlantis_cmd_*`, `atlantis_api_*` | Existing Atlantis command metrics |

`metrics.prometheusRule.enabled=true` adds alerts for:

- no leader,
- degraded membership,
- forwarding errors,
- plan risk errors.

## Logging and tracing

Logs are JSON. Command logs include `repo`, `pull`, `replica`, `trace_id` and
`span_id`, so a log line can be followed to its trace.

With `--tracing-enabled` (chart: `tracing.enabled`, `tracing.endpoint`),
Atlantis exports OTLP/gRPC spans. A command produces this span tree:

```
POST /events                          (webhook, on the receiving replica)
└─ atlantis.cluster.forward           (if another replica owns the pull)
   └─ atlantis.internal               (on the owner)
      └─ atlantis.command.plan
         └─ atlantis.project.plan     (repo, pull, project, workspace, dir)
            ├─ atlantis.step.init
            ├─ atlantis.step.plan
            └─ atlantis.plan_risk
```

The exporter and sampler are configured with the standard `OTEL_*`
environment variables.

## Plan risk (TypeSafe Jev)

See [plan-risk.md](plan-risk.md).

## Limitations

- **Plans live on the replica that made them.** Restarts and rolling updates
  keep them (StatefulSet volume, applies wait for the replica). Losing the
  volume or scaling the replica away means re-planning, unless the S3 plan
  store (`external_stores`) is configured.
- **Fencing is best effort for a running terraform process.** A replica that
  loses its pull lock stops starting steps, but a `terraform apply` already
  running continues; the state backend's locking protects that window.
- **Stale clones.** A replica that loses ownership keeps its clones until it
  restarts. They live on `emptyDir`, bounded by `dataVolume.sizeLimit`.
- **Per-replica data.**
  - Drift detection results.
  - The index page's list of running jobs. Locks on the index page are
    cluster-wide.
- **Object size.** A `PullStatus` object is limited by the API server's object
  size (about 1.5 MiB), which is thousands of projects per pull.

## Roadmap

- **M5, done: BoltDB and Redis removed.** Kubernetes Leases and PullStatus
  resources are the only storage. `--locking-db-type` and the `--redis-*`
  flags are gone, and tests use a fake-client KubeDB that passes the same
  conformance suite as the real API server. There is no migration tool: the
  fork was never released with BoltDB or Redis state to carry over. Coming
  from upstream Atlantis, re-plan open pull requests after switching.
- Store drift results as a CRD and aggregate the job list across replicas.
