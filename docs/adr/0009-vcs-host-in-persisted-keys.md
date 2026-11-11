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

Every persisted key gets the VCS hostname as its first segment, introduced over two releases: expand, then contract.

**New keys.**
- **Project lock:** `<host>/<FullName>/<path>/<workspace>/<project>`. `models.Project` gains the host, which callers set from the repo; it is already known everywhere a project is built.
- **Clone and plan directories:** `repos/<host>/<FullName>/<pull>/<workspace>`.
- **S3 plans:** `<host>/<owner>/<repo>/<pull>/...`.
- **Job logs:** `<host>/<FullName>/<pull>`.
- **Working-dir lock:** `workingDirKey` becomes `repo.ID()`, and `pulllock` stops taking the legacy full-name Lease.

**Release N (expand).**
- **Locks** are written under both keys: the new Lease and a legacy Lease with the same holder, created and deleted together. `TryLock` treats either as held. Old replicas see the legacy Lease, so a lock taken by a new replica blocks them too.
  - A legacy Lease alone, from before the upgrade, counts as held by its recorded pull. `PullRequest.SameAs` compares hosts, so it only matches its own pull.
  - Unlock and listing find locks of both forms by label. The lock id in links stays the legacy key for this release, so links in existing comments keep working.
- **S3 plans** are written under both keys, and every object carries a new `vcs-host` metadata entry. They are read from the new key, falling back to the legacy one, and deleted under both.
  - A legacy object is used only if its `vcs-host` equals the pull's host and its `head-commit` matches. A legacy object without `vcs-host`, written before N or by an old replica during the rollout, can't be attributed to a host. It is refused with "run plan again".
  - Plans made before the upgrade therefore need one re-plan. Applying the other host's plan is worse.
- **Job logs** are written under the new key only. They are read from the new key, then the legacy one, and deleted under both. A log that is missing for a few minutes during a rollout is acceptable; a missing lock or plan is not.
- **Clone directories** move on first use. If a pull's new directory is missing and the legacy one exists, a replica renames it under the pull's working-dir lock. It does so only if the clone's `origin` remote points at the same host; otherwise it clones again and the user re-plans. That happens only for same-named pulls on two hosts, which were broken before. The janitor and the pending-plan finder scan both layouts.
- The working-dir key and the pull Lease change in this release. Pulls are routed to one owner, so a pull is never handled by an old and a new replica at the same time.

**Release N+1 (contract).** Writes go to new keys only, and every legacy artifact is migrated or invalidated, so N+2 doesn't depend on first use.
- Locks and S3 plans are written under the new key only. Reads still fall back to legacy keys.
- At startup, the leader converts legacy lock Leases left over: it creates the new Lease if missing and deletes the legacy one. The lock id in links becomes the new key.
- At startup, the leader deletes every legacy S3 plan object. Any usable one was written by N and already exists under the new key.
- At startup, each replica moves its own remaining legacy clone directories, under the pull's working-dir lock and with the same `origin` host check as on first use. A directory that fails the check is deleted.
- A pull whose plan is deleted rather than moved gets `DiscardedPlanStatus` for those projects and a PR comment asking for a re-plan. Its locks stay. Apply then refuses with "run plan again" instead of failing on a missing file.

**Release N+2.** The fallbacks and the legacy scanning are removed. At startup each replica still sweeps any legacy clone directory or S3 plan it finds, discarding those plans the same way. That covers an upgrade that skipped N+1 against the release notes: the cost is a re-plan, never a held lock with no plan behind it.

**Alternatives considered.**
- **Detect and refuse same-named pulls on two hosts.** It costs nearly as much, since every key site needs the check, and it leaves the setups that hit this unsupported.
- **Change all keys in one release with a startup migration.** During the rolling update, old replicas wouldn't see locks taken under new keys, so two pulls could hold one project. It also loses local plans whose replica restarts after the migration ran elsewhere.
- **Document it as unsupported.** Mirrors during migrations are common enough that silently shared state is a real risk.

## Consequences

- Same-named repos on two providers no longer share or block each other's locks, directories, plans or logs.
- Release N doubles lock Lease and S3 plan writes and deletes. Both are small next to a plan or apply.
- Upgrading needs N and N+1 in order. Skipping N leaves old replicas blind to new locks during the rollout.
- Plans made before N, or by an old replica during the N rollout, need one re-plan. So does a plan whose legacy clone directory fails the host check in N+1.
- Lock links in PR comments written before N+1 stop resolving after the leader's conversion. Discarding such a lock from the UI or `atlantis unlock` still works.
- External tools that read S3 plan keys or log paths directly must follow the new layout.
- The implementation is about one PR per key family. Tests:
  - two same-named pulls on different hosts;
  - old-key data found after upgrading;
  - a legacy S3 object with another host's or no `vcs-host` refused;
  - the N+1 startup sweep moving or discarding legacy plans;
  - a mixed old/new replica pair honouring each other's locks.
