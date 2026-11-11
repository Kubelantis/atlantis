# 9. VCS host in persisted keys

Date: 2026-11-09

## Status

Proposed

## Context

One server can serve several VCS providers, and the same `owner/repo` can exist on two of them: mirrors during a migration, or an org that uses one name everywhere. Pull numbers are per repo, so GitHub PR #5 and GitLab MR #5 of `org/repo` can be open at once (issue #18).

Steps 1 and 2 of #18 put the host into lock owner checks (#32) and into in-memory keys and the pull Lease (#37). These persisted keys still leave it out:

- **Project locks.** The key is `models.GenerateLockKey`, `<FullName>/<path>/<workspace>/<project>`. It is stored as a Lease whose name is a hash of the key (`kubedb.lockName`), and it is also the lock id in UI and API links. Pulls with the same name on two hosts compete for one lock. Since #32 they no longer share it, but they still block each other.
- **Clone and plan directories,** `repos/<FullName>/<pull>/<workspace>` on each replica. Same-named pulls share a directory: one checkout replaces the other, and an apply can find the other pull's plan file. The working-dir locker deliberately keys on `FullName` (`workingDirKey`) so the two pulls at least wait for each other. `pulllock` also takes a legacy full-name Lease for the same reason.
- **The S3 plan store,** `<owner>/<repo>/<pull>/<workspace>/<dir>/<file>`, shared by all replicas, with the same collision. Closing one pull deletes the other's plans.
- **Job logs** in the file and S3 log stores, under `<FullName>/<pull>`.
- **The janitor** keeps a clone directory while a pull with that name and number is open on any host.

Nothing today establishes which host a stored plan came from. `PullStatus` records no plan digest. `ExpectedPlanHash` is computed from the plan file on disk after `S3PlanStore.Load` has downloaded it, so it only guards against the file changing between load and apply. `Load` checks the object's `head-commit` metadata, but mirrored repos on two hosts share commit SHAs. So an apply can run the other host's plan. Upstream Atlantis has the same keys.

These are persisted formats. Atlantis runs several replicas and updates them one at a time, so for a while old and new replicas run side by side. An old replica only knows the old keys.

## Decision

Every persisted key moves into a new, versioned key space that includes the VCS hostname. The change ships over three releases: expand, contract, then removal of the legacy reads.

**New keys.** Each key starts with the segment `@v2`. The first segment of a legacy key is always an owner name, and no supported provider allows `@` in one: GitHub, GitLab, Bitbucket, Gitea and Azure DevOps all exclude it. So a key's first segment says which format it is, never its depth. Prefixing the host alone could collide: a legacy GitLab repo `gitlab.example.com/org/repo` (a group whose path contains dots) and `org/repo` on host `gitlab.example.com` would map to the same path.
- **Project lock:** `@v2/<host>/<FullName>/<path>/<workspace>/<project>`. `models.Project` gains the host, which callers set from the repo; it is already known everywhere a project is built.
- **Clone and plan directories:** `repos/@v2/<host>/<FullName>/<pull>/<workspace>`. Plan files under `--share-plan-dir` aren't migrated: that flag is experimental and will be removed.
- **S3 plans:** `@v2/<host>/<owner>/<repo>/<pull>/...`.
- **Job logs:** `@v2/<host>/<FullName>/<pull>`.
- **Working-dir lock:** the local `workingDirKey` becomes `repo.ID()`.

**Release N (expand).**
- **Locks** are written under both keys: the new Lease and a legacy Lease with the same holder, created and deleted together. `TryLock` treats either as held. Old replicas see the legacy Lease, so a lock taken by a new replica blocks them too.
  - A legacy Lease alone, from before the upgrade, counts as held by its recorded pull. `PullRequest.SameAs` compares hosts, so it only matches its own pull.
  - Unlock and listing find locks of both forms by label. The lock id in links stays the legacy key for this release, so links in existing comments keep working.
- **Pull Leases.** `pulllock` keeps taking the legacy full-name Lease alongside the host-scoped one, as it does since #37. Routing can briefly disagree during membership changes, and replicas still touch legacy state, so the legacy Lease is what keeps two replicas off the same legacy artifacts. Same-named pulls on two hosts keep waiting for each other until the legacy Lease goes in N+2, as they do today.
- **S3 plans** are written under both keys, and every object carries a new `vcs-host` metadata entry. They are read from the new key, falling back to the legacy one.
  - A legacy object is used only if its `vcs-host` equals the pull's host and its `head-commit` matches. A legacy object without `vcs-host`, written before N or by an old replica during the rollout, can't be attributed to a host. It is refused with "run plan again".
  - Legacy objects are deleted one at a time, and only when their `vcs-host` matches the pull. The legacy pull prefix is never deleted as a whole, because it can hold the other host's plans. Objects without `vcs-host` are left to the migration sweep.
  - Plans made before the upgrade therefore need one re-plan. Applying the other host's plan is worse.
- **Job logs** are written and read under the new key only. Logs carry no host, so a legacy log can't be attributed to a pull and isn't read or deleted per pull. Logs from before the upgrade show as missing, and the migration sweep removes them.
- **Every legacy delete checks the host.** Legacy artifacts can belong to either host's pull, so none is deleted by name alone:
  - a legacy lock Lease is deleted only by the pull recorded as its holder, compared with `SameAs`, which includes the host;
  - a legacy S3 object only if its `vcs-host` matches, as above;
  - a legacy clone directory, when a pull closes or the janitor runs, only if its `origin` remote is on the pull's host. The janitor keeps it while a matching pull is open on that host.
- **Clone directories** move on first use. If a pull's new directory is missing and the legacy one exists, a replica renames it under the pull's working-dir lock and pull Leases. It does so only if the clone's `origin` remote points at the same host; otherwise it clones again and the user re-plans. That happens only for same-named pulls on two hosts, which were broken before. The janitor and the pending-plan finder scan both layouts.

**Release N+1 (contract).** Writes go to new keys only, and a migration sweep moves or invalidates every legacy artifact, so N+2 doesn't depend on first use.
- Locks and S3 plans are written under the new key only. Reads still fall back to legacy keys, with the checks above.
- **When the sweep runs.** Each replica's membership Lease carries a `keys-version` annotation. The leader starts the sweep only once every member reports N+1, so no replica still writes legacy keys. It repeats the sweep periodically until a full pass finds nothing, then records completion in a ConfigMap.
- **Per pull,** the sweep holds the pull's Leases, so no command runs on it at the same time. It checks `LostPullLock` before every write or delete and stops if the Lease is gone.
- **Locks.** Each legacy lock Lease gets its new Lease if missing, and the legacy one is deleted. The lock id in links becomes the new key.
- **S3 plans.** A legacy object is deleted only after its new-key copy is confirmed to exist with a matching `vcs-host` and `head-commit`. Otherwise the plan is discarded and the object deleted.
- **Clone directories.** Each replica moves its own remaining legacy directories, with the same `origin` host check as on first use. A directory that fails the check is deleted, and its plans are discarded.
- **Logs.** Legacy log keys are deleted.
- **Discarding a plan** sets `DiscardedPlanStatus` for those projects and posts a PR comment asking for a re-plan. The status is written with a conditional update, and only if the project still has no plan under the new key, so a fresh plan is never marked discarded. Its locks stay. Apply then refuses with "run plan again" instead of failing on a missing file.

**Release N+2.** The fallbacks, the legacy scanning and the legacy pull Lease are removed. At startup the leader checks the completion record. If it is missing, because an upgrade skipped N+1 against the release notes, the leader runs the same sweep before replicas take pulls. The cost is a re-plan, never a held lock with no plan behind it.

**Alternatives considered.**
- **Detect and refuse same-named pulls on two hosts.** It costs nearly as much, since every key site needs the check, and it leaves the setups that hit this unsupported.
- **Change all keys in one release with a startup migration.** During the rolling update, old replicas wouldn't see locks taken under new keys, so two pulls could hold one project. It also loses local plans whose replica restarts after the migration ran elsewhere.
- **Document it as unsupported.** Mirrors during migrations are common enough that silently shared state is a real risk.

## Consequences

- Same-named repos on two providers no longer share or block each other's locks, directories, plans or logs.
- Releases N and N+1 double lock Lease and S3 plan writes and keep the second pull Lease. All are small next to a plan or apply.
- Same-named pulls on two hosts keep waiting for each other until N+2.
- Upgrading needs N and N+1 in order. Skipping N leaves old replicas blind to new locks during the rollout.
- Plans made before N, or by an old replica during the N rollout, need one re-plan. So does a plan whose legacy clone directory fails the host check in N+1.
- Job logs written before N are no longer shown.
- Lock links in PR comments written before N+1 stop resolving after the sweep converts their lock. Discarding such a lock from the UI or `atlantis unlock` still works.
- External tools that read S3 plan keys or log paths directly must follow the new layout.
- The implementation is about one PR per key family. Tests:
  - two same-named pulls on different hosts, including a legacy name that looks like a host;
  - old-key data found after upgrading;
  - a legacy S3 object with another host's or no `vcs-host` refused and not deleted by that pull;
  - the N+1 sweep waiting for N replicas, moving or discarding legacy plans, and not discarding a fresh plan;
  - a mixed old/new replica pair honouring each other's locks.
