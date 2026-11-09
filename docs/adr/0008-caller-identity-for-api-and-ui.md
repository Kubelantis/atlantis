# 8. Caller identity for API and web UI actions

Date: 2026-11-09

## Status

Proposed

## Context

Atlantis applies infrastructure changes with privileged cloud credentials. For comment commands it knows who acted: the VCS user who wrote the comment, from a signed webhook. For the API and the web UI it doesn't (issue #24):

- **API.** Callers authenticate with one shared secret, `--api-secret`, sent as `X-Atlantis-Token`. Every caller looks the same.
- **Web UI.** Access is open, or protected by one shared basic-auth user (`--web-basic-auth`, `--web-username`, `--web-password`). The UI can discard a plan (delete its lock) and enable or disable the global apply lock. The pull request comment for a discard says only "via the Atlantis UI".
- **Access log (#41).** It records each state-changing UI or API request with the actor `api`, the shared web user, or `anonymous`. None of these names a person.
- **Audit events (ADR 0006).** They reserve `actor.source` `api` and `ui`, with `verified: false`, until this decision gives them real names.

In Kubernetes, the UI is usually published through an Ingress. Teams already put single sign-on in front of internal tools there, with oauth2-proxy or the Ingress controller's external auth. These pass the signed-in user to the backend in a header, such as `X-Auth-Request-User` or `X-Forwarded-User`. API callers are usually CI jobs and scripts, each of which can hold its own secret.

A header naming the user is only as trustworthy as the hop that sets it. Anyone who can reach the Atlantis pod directly can send the same header. The pod's immediate peer is the only address Atlantis can check; `X-Forwarded-For` can be forged too.

API commands carry no user today (`command.Context.User` is empty). That is deliberate. The user feeds the `atlantis_user` variable in workflows, `USER_NAME` for the external team allowlist, and policy-owner checks. A token name in that field would be read as a VCS user.

## Decision

Both identities are optional. With neither configured, Atlantis behaves as today.

**Named API tokens.**
- A new flag, `--api-secrets-file`, names a file, normally mounted from a Kubernetes Secret, that maps names to the SHA-256 hashes of tokens:

  ```yaml
  ci-prod: 3f9b…   # sha256 of the token, hex
  drift-bot: 7c21…
  ```

- A request's `X-Atlantis-Token` is hashed and compared with every entry in constant time. A match names the caller.
- Names match `^[a-z0-9][a-z0-9._-]{0,62}$`. Duplicate names or hashes, an unreadable file, or a malformed line stop the server at startup.
- `--api-secret` keeps working beside the file, with the actor `api`.
- The file is read at startup. Rotating a token means updating the Secret and rolling the pods, the same as `--api-secret` today. Reloading on change can be added later without changing the format.

**Trusted proxy identity for the web UI.**
- `--web-trusted-user-header` names the header (for example `X-Auth-Request-User`).
- `--web-trusted-proxy-cidrs` lists the networks the proxy connects from (for example the Ingress controller's pod network). Atlantis refuses to start if the header is set without CIDRs.
- The header counts only when the TCP peer of the request (`RemoteAddr`, never `X-Forwarded-For`) is inside those CIDRs. From any other address it is ignored and removed before handlers run. The access log then records the attempt at `warn`.
- This names the user; it doesn't authenticate them. The proxy decides who gets in. Deployments should allow ingress to the Atlantis web port only from the proxy, with a NetworkPolicy; the docs will show this with oauth2-proxy.
- When both a trusted user and basic auth are present, the trusted user is the actor.

**What the identity is used for.**
- A new `command.Context.Actor`, separate from `User`, carries `source` (`vcs`, `api`, `ui` or `system`), `name` and `verified`:
  - a named token or a trusted proxy user is verified;
  - the shared `--api-secret`, the shared basic-auth user and anonymous access are not.
  - `User` stays empty for API commands, so workflows, the team allowlist and policy owners never mistake a token name for a VCS user.
- The access log (#41) records the actor's name and source in place of `api` or the shared user.
- Audit events (ADR 0006) fill `actor` from it.
- A UI discard comment names the user when the actor is verified: "discarded via the Atlantis UI by `alice`".

**Alternatives considered.**
- **OIDC login in Atlantis itself.** It would also authenticate users, but adds a session store, callbacks and an identity-provider client to a server that runs several replicas. Single sign-on at the Ingress already exists in most clusters.
- **A shared secret from the proxy instead of CIDRs.** oauth2-proxy and the common Ingress controllers can't add a fixed secret header without custom configuration. Network checks fit how they are deployed. A secret can be added later as a second condition.
- **Plain tokens in the file.** Hashes mean a leaked file or a debug dump doesn't reveal usable tokens. The tokens are random, so a fast hash is enough.
- **Setting `User` for API commands.** Rejected for the reasons above; `Actor` keeps identity and VCS permissions apart.

## Consequences

- API and UI actions record who did them when operators configure named tokens or a trusted proxy. Investigations no longer stop at "the shared token".
- Nothing changes for deployments that set neither flag.
- A misconfigured CIDR list, too wide, lets anyone inside it claim any user. The docs must say to keep it to the proxy's pods and to pair it with a NetworkPolicy.
- Per-token permissions, such as a token that may plan but not apply, aren't part of this. Names make them possible later.
- Implementing it touches the API middleware, the request logger, `command.Context`, the lock controller's discard comment, and the Helm chart, which mounts the tokens file from a Secret. Audit events (#23) follow once ADR 0006 is accepted.
