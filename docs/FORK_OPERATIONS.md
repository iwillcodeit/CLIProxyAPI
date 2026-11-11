# Fork operations and deployment readiness

## Scope and baseline

This fork adds a place to maintain subscription-aware routing without changing
client endpoints or enrolling existing accounts again. No live routing change or
deployment has been performed. The first implementation adds release packaging
and a host-local release tool. The follow-up adds
[durable local affinity](FORK_AFFINITY.md) and
[explicit maintenance/draining](FORK_MAINTENANCE.md). The host observations below
still describe upstream 8.0.29.

The `fleet` branch started at upstream `v8.0.29`, commit
`5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3`, matching the inspected running
server. It now merges upstream `v8.0.67`, commit
`8bba7a77c6bcbfd993516d26606bfecaa4ff49d0`; the next fork release from this
baseline is `8.0.67-fleet.1`. Keep upstream `main` as the upstream tracking line.
Bring upstream release tags into `fleet` deliberately with a merge (never a
rebase of the published branch), after reviewing configuration and storage
compatibility; do not deploy upstream `main` automatically.

## Current evidence

The authenticated management API confirmed version 8.0.29, a loopback listener,
an external file-based OAuth directory, and session affinity enabled. The
2026-10-27 check reported `weighted-round-robin`, two active Claude credentials
and one active Codex credential.

Existing operator records describe a Linux amd64 no-plugin executable managed
by a systemd user service, separate configuration/state directories, and a
private HTTPS endpoint forwarded to its loopback port. A read-only SSH audit on
2026-10-27 confirmed the running systemd user service, its original versioned
executable path, matching on-disk/running executable checksums, Linux x86-64 and
Python 3.12. Configuration and credential directory permissions were checked;
the service was active with connections on its loopback listener. No restart or
deployment was performed. Recheck live activity before deployment. Keep hostnames,
keys, account identifiers and private host evidence outside this public fork.

The release tool's read-only adoption plan verified the installed binary without
creating its release root. Its systemd guard rejected the active live service.
All 19 release-operation tests also passed in an isolated temporary WSL directory;
these tests simulate activation and rollback, rather than restarting the live service.

## Replacement assessment

A build from the same upstream commit can use the existing configuration,
OAuth directory, client keys, management environment, static management panel,
working directory and listener port. Clients and the HTTPS forwarder need no
configuration change if those values stay the same. This compatibility is
established from the configuration and code, not from a completed host trial.
The fork baseline is now v8.0.67, so a replacement also carries every upstream
change since 8.0.29; review their configuration defaults before the first switch.

Two limitations prevent a promise of transparent replacement:

* Upstream `internal/api/server.go`, `Server.Stop`, closes the HTTP server immediately.
  Restarting it can terminate active streams and WebSockets. A systemd stop
  timeout does not add graceful draining to that implementation. The fork adds
  explicit admission/draining before stopping; normal shutdown stays immediate.
* `sdk/cliproxy/auth/session_cache.go` keeps affinity bindings in memory. A new
  upstream process loses them. Upstream prompt caches are not deleted by a proxy restart,
  but choosing a different account afterwards can prevent their reuse.

The first transition from the current process cannot import its bindings unless
an existing supported export mechanism is found. The current assessment found
none. The fork now checkpoints explicit and prefix affinity, including parent/fork
lineage and expiration, for subsequent restarts; see [its limits](FORK_AFFINITY.md).

## Recommended release layout

Use immutable release directories and one stable executable path:

```text
proxy/
  releases/<release-id>/cli-proxy-api
  current -> releases/<release-id>
```

Make a one-time, narrowly scoped systemd `ExecStart` override point at
`proxy/current/cli-proxy-api`. Preserve all original arguments, environment
files, working directory, restart policy and permissions. Keep configuration,
credentials, management assets and mutable state outside release directories.
Never overwrite an executable that a running process is using.

Publish a pinned Linux amd64 no-plugin artifact, build version/commit/date and
SHA256 checksum. Use distinct fork release identifiers. A dedicated fork
release workflow should build only required targets and must not rely on the
upstream DockerHub destination or credentials. The inherited release workflow
is disabled in this fork; the dedicated `fork-release` workflow is described below.

## Implemented release operations

`scripts/fork_release.py` uses Python 3.10+ and the standard library. Every command
defaults to a read-only plan; `--apply` performs the operation. It never rewrites
configuration, keys or OAuth files, or installs a service override. It stops a
running service only with explicit `--drain --apply` and authenticated evidence
that admission is closed and active work has finished.

* `adopt` verifies and copies the existing Linux executable into an immutable
  baseline release and creates `current`. This does not change the process
  already running from its original path. Supply the verified binary checksum,
  observed version and full 40-character source commit. Release manifests require
  full commit provenance; health responses may abbreviate that known commit.
* `stage` verifies an explicit archive SHA256, an allowlisted three-file archive,
  its manifest, executable architecture and binary checksum. It installs the
  release, including its license notice, without selecting it. Repeating an
  identical stage is harmless; replacing an existing release's contents is refused.
* `activate` requires an **already stopped**, loaded systemd user service whose
  `ExecStart` uses the managed `current/cli-proxy-api` path. A failed unit with
  no main/control process also qualifies for recovery; start limits are reset
  before starting a binary. The tool preserves a
  `previous` target, switches `current` atomically and starts the service. An
  authenticated loopback health check must report the expected version and
  commit; a failed candidate is stopped and the previous binary restarted and
  checked. If the candidate cannot be stopped, its link stays in place and the
  command fails for operator recovery. On maintenance-aware fork releases,
  `--drain` instead drains and stops the running service, keeps admission closed
  through startup/health checks, then resumes. A timeout aborts without cancelling work.
* `rollback` selects `previous` through the same checks. The service must be
  drained and stopped first, or use `--drain`. SIGKILL or power loss can interrupt an operation;
  `previous` is recorded before switching so a recovery target remains.

Stage operations can run while the proxy is busy. The first upstream-to-fork
activation requires an externally enforced idle maintenance window. Later
maintenance-aware builds support automatic drainage. The release tool
cannot export the upstream process's in-memory bindings; affinity-aware builds
restore their own checkpoints on later restarts. No live deployment has been
verified yet.

For the first adoption, run on the audited Linux host, substituting verified
values for the uppercase placeholders:

```sh
python3 scripts/fork_release.py adopt \
  --binary EXISTING_BINARY --sha256 EXISTING_BINARY_SHA256 \
  --version 8.0.29 --commit EXISTING_COMMIT --apply
```

Then prepare the one-time systemd override described above, preserving all
original arguments and environment. Do not reload/restart it while work is
active. For the first transition from upstream:

```sh
python3 scripts/fork_release.py stage \
  --archive RELEASE_ARCHIVE --sha256 ARCHIVE_SHA256 --apply

# After draining work, stop only the proxy's actual user service.
systemctl --user stop ACTUAL_PROXY.service
python3 scripts/fork_release.py activate --release RELEASE_ID \
  --service ACTUAL_PROXY.service --management-key-file PRIVATE_MANAGEMENT_KEY --apply

# During a subsequent maintenance window, if an explicit rollback is needed:
systemctl --user stop ACTUAL_PROXY.service
python3 scripts/fork_release.py rollback \
  --service ACTUAL_PROXY.service --management-key-file PRIVATE_MANAGEMENT_KEY --apply
```

Keep an independent SSH recovery channel. Before either stop, ensure clients
will not submit new work during the maintenance window. If startup or rollback
cannot be verified, inspect local service state; do not assume the endpoint is
healthy from a symlink alone.

For subsequent fork updates, replace the manual stop with `activate --drain`
or `rollback --drain`; see [maintenance and recovery](FORK_MAINTENANCE.md).

### Build and publish

After merging into `fleet`, manually dispatch `fork-release` on that branch with
a new version such as `8.0.67-fleet.1`. It builds the checked-out commit for Linux
amd64 with `CGO_ENABLED=0`, without refreshing catalogs from another repository.
It tests the release tool, smoke-stages the actual artifact, and publishes a
`fleet-vVERSION` release with the archive, manifest inside it, checksums and the
matching release tool. Existing tags are not overwritten; a failed publication
can leave a draft for inspection. Publishing never deploys the release.

## Deployment transaction

The eventual updater should default to a read-only plan and require an explicit
apply action. It should perform this sequence:

1. Verify host identity, architecture, service ownership, current binary and
   configuration compatibility. Acquire a deployment lock and remember the
   previous release target.
2. Download and verify the immutable artifact before touching the running
   service. Record its expected version and commit. Keep a private backup of
   the service definition and configuration with restrictive permissions.
3. Stage a health check on a separate loopback port with disposable state.
   Do not start two OAuth-refresh writers against the production credential
   directory or refresh copied production tokens during staging.
4. Stop admission of new inference work and drain active work using a verified
   mechanism. For the first upstream transition, enforce an idle window
   externally; a single idle connection snapshot is not a race-free
   guarantee. Abort rather than force interruption when work remains active.
5. Switch the symlink atomically, restart only the proxy service, and verify the
   expected binary, listener and authenticated management health.
6. Verify the unchanged external endpoint, rejection of unauthenticated calls,
   account presence, model listing, and a controlled inference request for each
   provider. Record success before deleting any previous release.
7. On failure, restore the previous symlink and restart the previous binary.
   Preserve the latest OAuth files: binary rollback must not automatically
   restore older refresh tokens. Restore configuration only when required and
   proven compatible with its current contents.

The service interruption should be limited to restart and health verification
after artifact staging, but its duration is unmeasured. Do not claim zero
downtime. A future persistent frontend with admission control and a draining
backend can support a more continuous handover; it also needs a single OAuth
refresh owner and durable affinity state.

## Subscription routing work to follow

Keep existing affinity, parent/fork lookup, priority, cooldown and failover
behavior. Apply subscription-aware ranking only to an initial selection with
no reusable affinity. Explicitly distinguish this from failure recovery.

Maintain a fresh per-account quota snapshot, combining passive response headers
with bounded reads of the OAuth usage endpoint. Prefer useful available capacity
whose weekly reset is approaching, with near-exhaustion margins, active session
weekly limits, capacity weights and deterministic fallback when data is stale
or unavailable. Concurrent initial selections must not all overcommit one
account solely because they saw the same snapshot.

Make hourly checks and window priming separately configurable. A zero utilization
percentage alone is not proof of an inactive window. Confirm reset state, pin
any priming request to the intended account, prohibit cross-account failover,
and verify the resulting window. Bound attempts, deduplicate per account and
persist enough state to prevent repeated priming after restarts. Schedule near
known resets as well as the hourly recovery scan. Test the actual provider
semantics before enabling priming; it does not increase weekly allowances.

## Acceptance before a live switch

Verify existing settings and accounts survive binary replacement and rollback;
active streams are drained or deployment is refused; repeated updates are
idempotent; invalid artifacts leave the live service unchanged; failed startup
recovers the old binary; secrets never appear in logs; and a quota-data outage
falls back without breaking sessions. Exercise affinity persistence across a
restart once that feature exists. Validate host-specific instructions privately
and keep observed results distinct from this proposed procedure.
