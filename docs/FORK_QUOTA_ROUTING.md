# Initial Claude account selection by subscription quota

Opt in without changing existing bindings:

```yaml
routing:
  strategy: weighted-round-robin
  session-affinity: true
  quota-aware-initial: true
```

The default is `false`. Home routing is excluded. This setting hot reloads. New
bindings use the highest eligible credential priority as before. Explicit session,
parent/fork and LCP matches remain authoritative, including across routing reloads.
Unavailable bindings and execution retries retain the configured failover strategy.
With subagent inheritance disabled, new child bindings are quota-ranked independently.
No-session requests, API keys, custom upstreams and non-Claude credentials retain
the configured strategy. A mixed candidate set also falls back to that strategy.

## Observations

The service reads `GET https://api.anthropic.com/api/oauth/usage` at startup and
hourly per Claude OAuth credential, with additional reads 30 seconds before and
5 seconds after known resets (15-second scheduling granularity). Failed reads retry
after one minute. Requests honor per-account/global proxy settings, reject redirects,
and bound encoded and decoded responses to 1 MiB. There is at most one read per
credential and four reads globally. No inference, token refresh or credential-file
write is performed by these reads. **Reading usage does not start a session window.**
Optional window priming is described below.

Snapshots are held only in RAM, keyed by credential ID, registration epoch and
credential version. Replacement/removal discards old observations and cancels the
old read; enabling the setting starts cold. Changes to proxy settings also cancel
old reads. Shutdown, disable and Home transitions cancel outstanding reads; config
changes are reconciled within 15 seconds. In accordance with the upstream timeout
policy, there is no post-connect request deadline. A stalled read occupies one of
the four slots until completion or lifecycle/config cancellation, without blocking
inference or account selection.

The parser reads three kinds of limit and ignores every other key and `limits[]` kind:

* **Session (5h) window**, from `five_hour` and the `limits[]` entry with `kind: session`.
  * `five_hour` null or absent, with no session entry, means the account has
    **no session limit**. That is never a constraint and never primed. Verified
    $200 plans now report this.
  * A `five_hour` object with a null `resets_at`, or a session entry with
    `is_active: false`, means the limit exists but is inactive.
  * A future reset means the window is active.
  * If the two sources disagree, the higher utilization and the scheduled reset win.
* **Shared weekly window**, from `seven_day` or the `weekly_all` entry. It is
  required: a response without it is rejected.
* **Model-scoped weekly caps**, from legacy `seven_day_opus` / `seven_day_sonnet`
  and `weekly_scoped` entries. A cap applies when its normalized
  `scope.model.display_name` (or `id`, when set) is contained in the resolved
  upstream model key, ignoring case: `Fable` matches `claude-fable-5-2`.

OAuth utilization is a percentage; passive response-header utilization is a fraction.
Newer complete 5h/shared-weekly header pairs can augment a fresh OAuth snapshot for the
same credential version. The ambiguous `7d_oi` headers are not interpreted as a model
cap. Model aliases use their resolved upstream model for the applicable weekly caps.

## Ranking and fallback

Every candidate must have comparable observations no older than 75 minutes and no
elapsed applicable reset. Otherwise the configured strategy selects normally.
Non-Claude models likewise fall back. A Claude model with no matching scoped cap is
ranked on the shared weekly window. Account priority and weighted-strategy
exclusions remain in force. No quota decision can move an existing session.

For comparable candidates, the policy computes:

```
remaining = min remaining fraction over shared weekly, matching scoped weekly,
            and the session window only while it is active
horizon   = hours until the soonest reset among the applicable weekly windows,
            clamped to [0.25, 168]; a null weekly reset counts as 168
score     = max(0, remaining - 0.02) * weight / horizon / (1 + pending)
```

The weekly allowance is use-it-or-lose-it, so the weekly reset drives the choice:
an account whose unused weekly allowance expires sooner is preferred. An active
session window can only lower `remaining`; an inactive or absent one is ignored.
The formula keeps a 2% advisory reserve. With the three sampled accounts, using a
Fable model 2026-11-11 17:00 UTC:

| Account | Shared weekly | Fable cap | Session | remaining | horizon | score |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 100%, resets in 36h | 5% | inactive | 0 | 36h | 0 |
| 2 | 93%, resets in 94h | 3% | none | 0.07 | 94h | 0.00053 |
| 3 | 0%, resets in 52h | 0% | none | 1.00 | 52h | 0.0188 |

Account 3 is chosen. All scores at zero fall back to the
configured strategy. Ties use credential ID for reproducibility. Credential weight
expresses relative plan capacity (default 1); set it appropriately for unequal
plans. The API exposes utilization rather than an absolute spendable token count,
so this is a scheduling heuristic, not a token-capacity guarantee.

`pending` counts atomic, account-wide reservations for initial selections, shared
across models and routing reloads. The first execution result releases its reservation;
an abandoned result expires after one minute. These reservations reduce bursts of
new sessions onto a single account while keeping the chosen sessions sticky.
Observations/reservations are advisory and do not override upstream cooldowns.
There is no active-session migration, durable quota snapshot or extra-usage activation.
Automatic window priming requires the separate opt-in below.

## Verification

Run the relevant suites with the repository Go toolchain:

```sh
go test -race ./internal/auth/claude ./internal/config ./internal/watcher/diff ./sdk/cliproxy/auth ./sdk/cliproxy
go build -o /tmp/cliproxy-quota ./cmd/server
```

Tests exercise quota/reset/weekly ranking, weights and priorities, inactive and
malformed windows, stale/replaced observations, passive signals, existing explicit/
parent/fork/LCP bindings and unavailable matches, retry fallback, simultaneous
selection, reservation expiry/reload, bounded polling, cancellation and stale results,
read-only OAuth requests, redirects and response-size limits. No live credentials or
inference are needed.

## Optional window priming

```yaml
routing:
  session-affinity: true
  quota-aware-initial: true
  claude-window-priming: false # Set true only after validating an inactive window.
```

The default is off. When enabled, each successful scheduled usage read can trigger
one minimal Haiku message for that exact account only when a session limit exists
but is inactive (a `five_hour` object with a null reset, or a session limit with
`is_active: false`), the observation is at most one minute old, and shared weekly
utilization is below 98%. Accounts without a session limit (`five_hour` null or
absent and no session entry) are never primed. Zero utilization with an active reset,
missing data, expired windows and unavailable state do not authorize inference. Normal monitoring is hourly, with the
same additional reset checks described above.

Before reserving an attempt, the usage worker uses normal serialized OAuth identity
preparation (which may persist missing account/device metadata), then reads usage
for that exact credential version. Preparation errors consume no inference attempt.
The service invokes the registered Claude executor once with
`claude-haiku-4-5-20251001`, `max_tokens: 1`, and “Reply with a dot.” It bypasses
manager selection, affinity and retry/failover loops; the exact credential epoch
and version must still match. Existing executor payload rules, OAuth shaping,
proxies and usage reporting still apply, so configured payload overrides can change
the final request. A failed attempt never switches accounts or models. This
consumes subscription quota and does not increase any weekly allowance.

Only one priming job runs at once. It holds server background admission through
inference, a follow-up OAuth usage read, and cleanup. Maintenance refuses new jobs
and waits for admitted jobs. Shutdown, disable, Home mode, credential/proxy changes
cancel jobs; configuration is reconciled within 15 seconds. Changing `auth-dir`
requires restart. No post-connect timeout is introduced. The result is considered
confirmed only when a fresh usage response contains a future session reset.
An unconfirmed/failed result or maintenance refusal schedules another usage check
after five minutes. Accounts waiting for the single priming slot are rechecked
as soon as it frees, rather than waiting for their next hourly read.

Before inference, a private write-ahead reservation is atomically persisted in
`AUTH_DIR/.claude-window-priming.state`. SHA-256 subscription keys use the account
UUID when available, otherwise auth ID; tokens and plaintext account IDs are not
stored. At most two attempts are permitted in each five-hour episode, at least five
minutes apart, including across restarts. Confirmation preserves suppression through
the later of the episode end and observed reset. Clock rollback blocks attempts;
corrupt, public, missing-owner or unwritable state disables priming while ordinary
routing continues. State is bounded to 1,024 records / 1 MiB.

A lifetime exclusive `.claude-window-priming.state.lock` prevents two processes from
priming the same store concurrently. Orderly shutdown removes the lock. A crash
leaves priming disabled: stop the owning process, then remove **only the `.lock`
file**, preserving `.state`, before restarting. Never delete a lock owned by a
running process. The reserve/write path targets the existing Linux/WSL deployment;
filesystems must support atomic rename and directory sync.

### Validate before activation

Automated tests use fake usage responses and executors; they do not establish
Anthropic billing-window semantics. Before enabling live, obtain a fresh real
inactive-window response, send exactly one pinned Haiku request with the existing
executor, then verify that the same account acquires a future five-hour reset and
that this is the shared window relevant to Opus. Preserve the read/attempt/reset
evidence without credentials. A successful message alone is insufficient evidence.
The 2026-11 samples show an inactive session as a `five_hour` object with a null reset
plus `is_active: false`, while explicit null now means no session limit. If the
endpoint changes this representation again, keep priming disabled and update the
detector against that evidence. This implementation has not yet completed that live
inactive-window experiment.
