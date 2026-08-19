# Error Classifier: Open Issues & Proposed Fixes

**Status**: All three issues are **implemented** (2026-08-18). Issue 3 was
discovered during review of issues 1 and 2: the IP reputation tracker was never
receiving classified errors, so the degradation mechanism the classifier feeds
was dead code. It is now wired up — see Issue 3 for what changed and what to
watch after deploy.

**Context**: A production log showed `454 Relay access denied` being reported as
`error="TLS negotiation failed"`. The root cause was a pattern in
`internal/delivery/error_classifier.go`: mapping SMTP codes to human-readable
messages *without looking at the response text*, even though many codes are used
for several unrelated conditions in the wild. Three instances of this pattern
were fixed in commit `e158a70` (codes 452, 454, 553). This document covers the
remaining instances: Issues 1 and 2 (same root cause, now fixed) and Issue 3
(found while reviewing Issue 2's impact, now also fixed).

---

## Issue 1: Every 421 is classified as greylisting — **FIXED**

### Behavior before the fix

`classifySMTPCode()` (`error_classifier.go`) returns `ErrorGreylist` /
`"Greylisting detected"` for **every** SMTP 421, regardless of response text.

### Why that's wrong

- Real-world 421s are mostly **rate limiting or load shedding**, not
  greylisting. Example: Gmail's `421 4.7.0 Try again later` is throttling of
  suspicious/high-volume traffic.
- Actual greylisters (Postgrey, postscreen-style setups) mostly reply with
  **450 or 451** and say so in the text (e.g. `450 4.2.0 Greylisted, please
  try again later`). Those responses are currently classified as plain
  `ErrorTemporary` — so the label is wrong in both directions: 421s that
  aren't greylisting get the label, and 450/451s that are greylisting don't.

### Impact

Cosmetic, but caller-visible. In v2.0 `ErrorGreylist` has **no behavioral
difference** from `ErrorTemporary`: both map to `status: "temp_fail"`
(`delivery.go`, status-mapping switch) and both are retryable
(`IsRetryable()`). The retry scheduler that gave greylist a special
fast-retry path was removed in the v2.0 refactor; the doc comment on the
`ErrorGreylist` constant ("requires aggressive fast retry") is a v1.x relic.

What callers *do* see is the JSON `error` field. A caller that reads
`"Greylisting detected"` and retries quickly (the textbook response to
greylisting) against a server that just rate-limited it will make things
worse.

### Fix (implemented)

1. `classifySMTPCode()` returns `ErrorGreylist` only when the response text
   indicates greylisting (`greylist`, `graylist`, `grey-list`, `gray-list`,
   `grey listed`, `gray listed`, via `isGreylistResponse()`) — checked for
   **any 4xx code**, not just 421. The greylist text check runs after the reputation keyword check,
   since an explicit blocklist reference is the more specific signal.
2. Plain 421s fall through to `ErrorTemporary`. The 421 case in
   `classifyTemporaryError()` now reads
   `"Service not available (rate limiting or server shutdown)"`.
3. The stale doc comment on the `ErrorGreylist` constant was rewritten (the
   fast-retry claim is gone; it now documents text-based detection and the
   v2.0 equivalence with `ErrorTemporary`).

Risk: low. `status` stays `temp_fail` either way; only the `error` message and
the debug-log `category` change.

---

## Issue 2: Reputation keyword scan runs before the 2xx success check, and over-matches on 4xx — **FIXED**

### Behavior before the fix

`classifySMTPCode()` calls `isReputationError(response)` **before** the
`code >= 200 && code < 300 → nil` success check. `isReputationError()` matches
keywords (`blocked`, `blacklist`, `rbl`, `dnsbl`, `spamhaus`, …) at **any**
SMTP code.

### Why that's wrong

Two separate problems:

**a) Ordering (theoretical).** A 2xx response containing a keyword like
"blocked" would be classified as `ErrorReputation` instead of returning nil
(success). In practice `ClassifyError()` is only invoked on failure paths in
`delivery.go`, so this is latent — but the ordering is backwards and cheap to
fix.

**b) Weak keywords on 4xx (also latent — see Issue 3).** The bare keyword
`blocked` matches routine deferrals such as `451 Temporarily blocked, try
again later` — classic greylisting/throttling phrasing. That response is then
classified `ErrorReputation` and reported to the caller as
`"IP reputation/blacklist error"`, which is misleading in exactly the way the
`454` mislabel was.

An earlier draft of this document claimed such responses also record a
reputation strike against the source IP. Review showed that path is **dead
code** in the current tree — see Issue 3. So both halves of this issue were
message-level bugs, not behavioral ones.

### Fix (implemented)

1. The 2xx success check now runs first in `classifySMTPCode()`, before any
   keyword scan. A nil guard was also added at the call site
   (`mapSMTPError()`, `delivery.go`): after the reorder, a 2xx code reaching
   the error path would make `ClassifyError()` return nil, and the previous
   unconditional `classified.Message` dereference would have panicked.
   Unreachable in practice (`mapSMTPError` is only called with a non-nil
   error), but the guard is one line.
2. `isReputationError()` now splits the keyword list by signal strength:
   - **Strong** (match at any code): `spamhaus`, `rbl`, `dnsbl`, `blacklist`,
     `poor reputation`, and vendor names (`proofpoint`, `cloudmark`,
     `barracuda`). Blocklist operators frequently deliver listings via 4xx as
     well as 5xx, so these keep matching on 4xx.
   - **Weak** (require a 5xx code): `blocked`, `rejected for policy reasons`.

Risk: low at the time it was applied — with the reputation tracker unwired
(Issue 3), the split changed only the `error` message and debug-log `category`
on some 4xx deferrals. Now that Issue 3 is fixed, the split is load-bearing:
it decides which responses degrade a source IP. Sanity-check the strong/weak
lists against real bounce logs after deploy (see Issue 3's operational note).

---

## Issue 3: IP reputation tracking was dead code — the tracker never received classified errors — **FIXED**

### What was found

Both production call sites of the reputation tracker passed `nil` for the
`*DeliveryError` parameter:

```go
// delivery.go — both call sites, before the fix
d.reputationTracker.RecordDeliveryAttempt(sourceIP, result.Status == "delivered", nil, deliveryInfo)
```

`RecordDeliveryAttempt` (`ip_reputation.go`) only marks an IP degraded when
`err != nil && err.Category == ErrorReputation`, and `MarkIPDegraded` has no
other caller. With `nil` always passed:

- **No IP was ever marked degraded** — blacklisted source IPs stayed in
  rotation indefinitely.
- The recovery path (`MarkIPRecovered`) was equally unreachable, since nothing
  was ever degraded.
- `strela_ip_reputation_degraded` metrics, degradation alerts (webhook), and
  the `GetHealthyIPs` rotation filter were all inert, despite being
  documented and configurable via `[reputation]`.

The classifier's `ErrorReputation` category only ever influenced the JSON
`error` message and debug logs. `mapSMTPError` classified the error correctly
but discarded everything except the message string.

### The decision: wire up, not remove

The feature is documented (CLAUDE.md, config.toml.example), has a config
section, metrics, and an alert webhook — operators reasonably believe it
works. And with the Issue 2 keyword split in place, the main hazard of turning
it on (routine deferrals degrading healthy IPs) is addressed. So the fix
completes the wiring rather than deleting the feature.

### The fix (implemented 2026-08-18)

1. `DeliveryResult` gained an unexported `classifiedErr *DeliveryError` field
   (not part of the JSON API response).
2. `mapSMTPError` stores the classifier verdict on the result instead of
   discarding it. The indeterminate path (`status: "unknown"`) intentionally
   leaves it `nil` — an unknown outcome must not count as a reputation strike.
3. Both `RecordDeliveryAttempt` call sites pass `result.classifiedErr` instead
   of `nil`.

Covered by `TestMapSMTPError_CarriesClassifiedError` and
`TestRecordDeliveryAttempt_ReputationErrorDegradesIP`
(`delivery_util_test.go`), which pin the full path from SMTP error to degraded
IP, including the negative case (a `451 Temporarily blocked` deferral must not
degrade).

### Operational note for deploy

This activates behavior that has never run in production: source IPs can now
actually be marked degraded and **removed from rotation**, and alert webhooks
will fire. After deploying, watch `strela_ip_reputation_degraded` and the
degraded-IP list; if legitimate IPs get pulled, the keyword lists in
`isReputationError()` are the tuning knob.

---

## Reference: the pattern already fixed (commit `e158a70`)

For comparison, the same blind code→message mapping was fixed for:

| Code | Was always labeled | Also used in the wild for |
|------|--------------------|---------------------------|
| 454  | "TLS negotiation failed" (RFC 3207) | Postfix deferred relay denial (`defer_unauth_destination`) |
| 452  | "Insufficient system storage" (RFC 5321) | Recipient-count limits (Postfix "too many recipients"), rate limiting |
| 553  | "Invalid mailbox name" | Sendmail-style relay denial — which also made `ShouldDeactivateEmail()` deactivate valid addresses |

The fix in each case: sniff the response text for the alternative meanings
first, fall back to the RFC-standard label. Issues 1 and 2 above are the
remaining instances of the same root cause: **the classifier trusting the SMTP
code alone when the code is ambiguous in practice.**
