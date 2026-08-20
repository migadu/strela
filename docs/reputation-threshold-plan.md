# Plan: Threshold Degradation + Classifier Tightening

**Status:** Planned (not yet implemented)
**Author:** Design discussion, 2026-08-19; revised same day after code review
(dropped the message-scoped veto, broadened IP-scoped signals, specified
locking/cleanup details, corrected hot-reload claim)
**Scope:** `internal/delivery/error_classifier.go`, `internal/delivery/ip_reputation.go`, `internal/config/config.go`, docs

## Problem

A single 5xx reputation-classified failure marks a source IP as degraded and
pulls it from rotation for **every** destination for `degraded_retry_hours`
(default 48h). Gmail's per-message content block —

```
550-5.7.1 ... this message is likely unsolicited mail. To reduce the amount
550 5.7.1 of spam ... this message has been blocked. ...
```

— trips this via the weak keyword `blocked` in `isReputationError`
(`error_classifier.go`). That is a **per-message content decision**, not an
IP-reputation event, yet it degrades the whole IP pool. The result is the
observed alert: "1 degraded source IP(s) — delivery capacity is reduced."

Both root causes are addressed by two independent, composable changes.

## Part A — Tighten the classifier (`error_classifier.go`)

The weak, 5xx-only keywords (`blocked`, `rejected for policy reasons`) match
message-scoped rejections. Require IP-scoped evidence before classifying as
reputation.

- Add `hasIPScopedSignal(resp)` → matches: `your ip`, `ip address`,
  `sending ip`, `the ip`, `listed`, `reputation`, `ptr`, `rdns`,
  `reverse dns`, `blocklist`, `blacklist` — **or** the response text contains
  the actual source IP literal used for the attempt (the tracker knows which
  IP it sent from; providers like Outlook quote it: `banned sending IP
  [x.x.x.x]`).
- A weak keyword counts as reputation only when `hasIPScopedSignal(resp)`.
- **No message-scoped veto.** An earlier draft additionally required
  `!hasMessageScopedSignal(resp)` (rejecting `this message`, `unsolicited`,
  `content`, ...). That misfires on Gmail's *genuine* IP-reputation block
  (S3140): `550-5.7.1 Our system has detected an unusual rate of unsolicited
  mail originating from your IP address ... mail sent from your IP address
  has been blocked` — it contains both `your ip` and `unsolicited`, so the
  veto would suppress the one Gmail response that IS an IP-reputation event.
  The per-message content block already fails the IP-scoped requirement on
  its own (it has no IP language), so the veto adds nothing but false
  negatives.
- **Strong keywords** (`blacklist`, `poor reputation`, `rbl`, `dnsbl`,
  `spamhaus`, `proofpoint`, `cloudmark`, `barracuda`) are unchanged — always
  reputation, at any code.
- Known substring quirk (accepted, comment it): `listed` also matches
  `delisted` — rare in a rejection line and harmless with the Part B
  threshold in front of it.

Effect: Gmail's `550 ... this message has been blocked` no longer classifies
as reputation. Note this is **not a caller-visible change**: for `smtpCode >
0` the response status is code-based (`delivery.go` status mapping), so the
550 was already returned as `hard_bounce` even when classified reputation —
Part A only stops the IP-degradation side effect. Good rollout property.

### Confidence signal for tiering

Return match confidence so the tracker can distinguish immediate vs.
threshold-gated degradation:

- Internal check returns `(isRep bool, strong bool)`.
- Add `ImmediateDegrade bool` to the `DeliveryError` struct, set from `strong`.
- Strong keyword → `ImmediateDegrade = true`.
- Tightened-weak keyword → `ImmediateDegrade = false` (threshold-gated).

## Part B — Corroboration threshold (`ip_reputation.go`)

Reputation is statistical: a genuinely bad IP fails repeatedly across many
recipients/providers, while a one-off content block does not. Do not degrade on
a single ambiguous (non-strong) reputation error.

- Add a pre-degrade strike tracker to `IPReputationTracker`:
  `repStrikes map[string][]time.Time`, guarded by the existing `rt.mu`.
- In `RecordDeliveryAttempt`, when `err.Category == ErrorReputation`:
  - `err.ImmediateDegrade == true` (strong keyword) → `MarkIPDegraded` now
    (preserves today's behavior for high-confidence listings).
  - otherwise → append `now`, prune entries older than the window, and call
    `MarkIPDegraded` **only** when the count `>= DegradeFailureThreshold`; clear
    that IP's strikes after degrading.
- **Locking (TOCTOU)**: append + prune + threshold check + strike clear must
  be a single critical section under `rt.mu.Lock()` — but `MarkIPDegraded`
  acquires `rt.mu` itself, so it cannot be called while holding the lock.
  Decide under the lock (returning a `shouldDegrade` bool), unlock, then call
  `MarkIPDegraded` — and only from the goroutine that actually crossed the
  threshold (clearing strikes inside the critical section guarantees exactly
  one crosser). Otherwise two concurrent weak failures at threshold−1 both
  cross and double-fire the degrade webhook/metric.
- On successful delivery, clear that IP's strikes (in addition to the existing
  `MarkIPRecovered` path). Note the current success path only acts when the
  IP `wasDegraded`; strike clearing needs a write lock on every success that
  has a strike entry, not just degraded IPs.
- Prune stale strikes in the existing `Cleanup()` path so the map cannot grow
  unbounded. **Sweep the whole `repStrikes` map**, dropping entries whose
  newest timestamp is older than the window — `Cleanup()` currently iterates
  `degradedIPs` only, and strike entries exist precisely for IPs that were
  *never* degraded (e.g. 1–2 strikes, then the IP rotates out of use).

Effect:

- One spammy message → Gmail 550 content block → (already not reputation after
  Part A; even if it were) 1 strike, below threshold → IP stays in rotation.
- IP genuinely on a DNSBL → many reputation errors quickly → threshold crossed
  → degraded. Strong-keyword listings still degrade on the first hit.

## Config (`internal/config/config.go` + `SetDefaults`)

New fields on **`ReputationConfig`** (TOML section `[reputation]`), defaults
set in `SetDefaults()` next to `DegradedRetryHours`:

```go
DegradeFailureThreshold int `toml:"degrade_failure_threshold"` // reputation strikes before degrading (default: 3; 1 = pre-change one-shot behavior)
DegradeWindowMinutes    int `toml:"degrade_window_minutes"`    // rolling window for counting strikes (default: 30)
```

- Read via the existing `rt.config` pointer (same pattern as
  `DegradedRetryHours`). **Not hot-reloadable**: `reload.go` has no handling
  for the `[reputation]` section, and the tracker holds `&cfg.Reputation`
  from the startup config — these fields require a restart. Document them as
  restart-only (adding `[reputation]` to reload is out of scope here).
- Strong-keyword hits bypass the threshold regardless of these values.
- Setting `degrade_failure_threshold = 1` restores the current one-shot
  behavior.
- **Window trade-off**: threshold 3 assumes ≥3 sends through the *same
  source IP* within the window. With a sizeable rotating pool and modest
  volume, a weak-signal listing may never trip — hence the 30-minute default
  (not 10). Acceptable residual risk: genuine DNSBL listings are caught
  immediately by strong keywords; the threshold only gates ambiguous
  weak-signal cases.
- Document both fields in `config.toml.example`.

## Tests

`error_classifier_test.go`:
- Real Gmail `550` per-message block string → **not** reputation
  (→ `hard_bounce`).
- Real Gmail S3140 IP-block string (`... unsolicited mail originating from
  your IP address ... has been blocked`) → **is** reputation,
  `ImmediateDegrade == false`. This is the regression test for the dropped
  message-scoped veto — it contains both IP-scoped and message-scoped words.
- Outlook S3150-style string (`... banned sending IP [192.0.2.1] ...`) →
  reputation (covers `sending ip` / source-IP-literal matching).
- Spamhaus / DNSBL string → reputation with `ImmediateDegrade == true`.
- IP-scoped `550 ... your IP ... blocked` → reputation, `ImmediateDegrade == false`.

New `ip_reputation_test.go`:
- `threshold-1` weak strikes do not degrade; the `threshold`-th does.
- Strikes older than the window are pruned and do not count.
- A strong-keyword hit degrades on the first attempt.
- A successful delivery clears accumulated strikes.
- Concurrency: N goroutines record a weak reputation failure for the same IP
  at threshold−1 → exactly **one** degrade event fires (catches the TOCTOU).
- `Cleanup()` drops stale strike entries for IPs that were never degraded.
- Run with `-race`.

## Docs

- Update the reputation section in `CLAUDE.md`; note the two new fields are
  **restart-only** (do not list them under hot-reloadable settings — see
  Config section above).
- Update `config.toml.example` with the two new fields.

## Scope / non-goals

- No per-destination (source-IP × recipient-domain) reputation scoping.
- No MX-host exclude filter.
- No config-driven response-text exclude list.

These can be layered on later; the threshold + classifier tightening is the
primary fix. The default threshold of 3 intentionally changes the current
one-shot degradation behavior.
