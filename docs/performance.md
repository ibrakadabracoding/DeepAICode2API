# Performance Measurement and Optimization Plan (2026-11)

Measured data from the third batch of 50-account stress tests, along with the optimization plan derived from it.
Bottom line first: **the bottleneck is not in the code but in two "waits"** - waiting for verification emails (55%) and the serialized stage order.
With the three P0 changes, per-account end-to-end time can drop from **27.0s to about 15.5s**, doubling throughput.

> This document retains the **measurement data** (stage breakdown, benefit estimates, batch baselines); per-batch execution logs are no longer
> being added. For reusable rules, checklists, and failure boundaries, see `docs/lessons.md`;
> for the current acceptance baseline, see README §6.11.

---
## 1. Measurement Method

`register.Record` now includes `FinishedAt` (seconds, including fractional values):

```go
E2E = FinishedAt - CreatedAt     // real elapsed time for this account from mailbox creation to pool entry
```

- `CreatedAt` is written at the start of `runOne` (**excluding** time spent waiting in the queue for `waitTurn`);
- `FinishedAt` is written when `finish()` / `resumeWithKey()` ends;
- Each attempt is recorded in the ledger, so "first attempt" and "rerun" can be distinguished;
- Stage durations come from `Record.Timings` (seconds per stage, written by `Mark()`).

To reproduce:

```bash
./register.exe -count 50 -concurrency 5 -group stress-50c-$(date +%Y%m%d) \
  -result-dir result/stress50c
# then filter exports/register-ledger.jsonl by group
```
## 2. Comparison of Three Live Test Batches

| Batch | Accounts | Concurrency | Egress | Wall Clock | Pooled | Avg E2E per Account |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Batch 1 | 50 | 5 | 10 | 4m40s | 47 (-resume 49) | - |
| Batch 2 | 50 | 5 | 12 | 8m23s | 49 (-resume 51) | - |
| **Batch 3** | **50** | **5** | **12** | **4m39s** | **46 (-resume 49)** | **27.0s** |

> The first two batches had no instrumentation, only wall clock; from Batch 3 onward, per-account `FinishedAt` is available.
## 3. Third Batch: Per-Account End-to-End Rate

| Metric | Value |
| --- | --- |
| Sample | 50 accounts (all with E2E) |
| Mean | **27.0s** |
| Median | 25.3s |
| p90 / p95 | 33.3s / 35.0s |
| Min / Max | 21.0s / 37.7s |
| Wall clock | 279s (4m39s) |
| **Average rate** | **5.58s/account = 10.75 accounts/min = 645 accounts/hour** |
| Concurrency utilization | **4.83 / 5 = 96.6%** |

Concurrency utilization = `sum(E2E) / wall clock`. 96.6% means the 5 workers are **almost never idle**;
throughput is entirely determined by "per-account time ÷ concurrency" - there are only two ways to speed things up: **reduce per-account time** or **increase concurrency**.
## 4. Stage Breakdown (mean, as a share of 27.0s)

| Stage | Mean | Share | Nature |
| --- | ---: | ---: | --- |
| `verify` (waiting for verification email) | **14.8s** | **54.8%** | Pure waiting |
| `exists` (whether the email is already registered) | 3.3s | 12.1% | Network round trip (**can be skipped**) |
| `validate` (`/theos` liveness check) | 3.1s | 11.6% | Network round trip (required) |
| `enroll` (add to pool) | 2.0s | 7.3% | Network round trip (**includes duplicate probing**) |
| `register` (create account) | 1.2s | 4.4% | Network round trip |
| `login` | 1.1s | 4.1% | Network round trip |
| `session` | 0.5s | 2.0% | Network round trip |
| `mailbox` (create inbox) | 0.4s | 1.4% | Network round trip |

**Real timeline for a single account** (third batch of samples):

```text
13:48:52 mailbox ready
13:48:57 registration succeeded   (+5s)
13:49:02 API Key obtained         (+5s)   ← does not depend on email verification
13:49:16 email verification done  (+14s)  ← pure waiting, fills the entire middle segment
13:49:20 Key liveness check passed (+4s)
13:49:25 added to pool            (+5s)
```

Key observation: **the Key is obtained before verification completes**, so the verification wait can be fully overlapped with other stages.
## 5. Failure Analysis (Third Batch)

Of the 50 accounts, 4 failed to enter the pool, and **all of them were stuck at `/theos`**:

| Account Count | Stage | Error |
| ---: | --- | --- |
| 3 | `enroll` | `HTTP 400 Key liveness check failed: .../theos` (the gateway **probed it again** during pool entry) |
| 1 | `validate` | `unexpected EOF` |

Two patterns:

1. **Failures are concentrated in the same type of transient network error** and can be recovered by retrying - after two rounds of `-resume`, 49/50;
2. **Duplicate probing on the pool-entry path**: the pipeline has already performed a liveness check (`validate`), but the gateway's `enroll`
 probes `/theos` again on its own. This adds 2.0s and is also the direct cause of the 3 failures.

---
## 6. Optimization Plan

### P0-1 Stop re-probing `/theos` on enrollment (est. −2.0s/account, failure rate 8% → ~2%)

**Location**: `internal/server/server.go`, the `actionEnroll` branch of `handleAdminAccounts`

```go
al, err := s.clientFor(acct).Theos(ctx, acct)   // ← duplicate probe
if err != nil { /* reject with 400 */ }
```

**Plan**: The caller (the `register` pipeline) already carries `theos_balance` /
`theos_granted` in the payload and has just passed a liveness check. Change to:

- If the request has `"skip_probe": true` and includes `theos_balance` → **trust the caller's result, skip the probe**;
- Manual enrollment from the console still probes (a human may have entered the wrong key, so it's worth verifying);
- On probe failure, **retry once** (1s backoff); reject only if the retry also fails.

**Acceptance**: `enroll` stage mean ≤ 0.6s; `keyed` status drops to zero in `stress-*` batches.

### P0-2 Skip the existence pre-check for brand-new mailboxes (est. −3.3s/account)

**Location**: `internal/register/pipeline.go`, step 2 "Pre-check: does the email already exist"

**Plan**: A newly created CF temporary mailbox is **guaranteed** to be unregistered, so the pre-check only matters for "re-runs" and "user-specified mailboxes". Have the mailbox return a `Fresh bool` (`cfProvider.Create` returns true; `Inbox(email)` / `static` / `none` return false), and skip `exists` entirely when `Fresh` is set.

**Acceptance**: The `exists` stage disappears from new batches; the re-run path still keeps it.

### P0-3 Run the verification wait in parallel with later stages (est. −6.2s/account, −23%)

**Location**: The stage order in `runOne` in `pipeline.go`

**Plan**: After a successful registration, trigger the verification email immediately, then **do not block** - continue with login → key. Move `WaitForVerification` into its own goroutine and `join` it before `validate`:

```text
Now:    mailbox → register → login → key → verify(14.8s) → validate → enroll
After:  mailbox → register → login → key ┐
                              verify ────┴→ validate → enroll      (parallel)
```

The threading model is unchanged (one account still occupies one worker); the wait is simply hidden behind other network round-trips.

**Acceptance**: E2E mean ≤ 21s; the `verify` and `key` time ranges overlap in the logs.

### P1-1 Concurrency 5 → 10 (est. throughput +80%)

A utilization of 96.6% shows that 5 concurrent workers are already saturated; there are 12 egress routes, so `-concurrency 10` is still below the egress count. The precondition is that upstream doesn't trigger rate limiting - it's recommended to run 20 accounts first and observe the error rate in the `register` stage.

**Acceptance**: 50 accounts complete in ≤ 2m30s wall-clock; failure rate no higher than 5%.

### P1-2 Backoff retry for `/theos` failures (failure rate 8% → <2%)

**Location**: `pipeline.validate` and the `client.Theos` call sites

The existing `Settings.MaxRetries` only covers inference requests; probes have no retries. Add: retry twice on network errors / 5xx / 429 (1s, 3s backoff); do not retry on 401/403 (that means the key really is bad).

**Acceptance**: Failures of the `unexpected EOF` / `No exit node` / `TLS handshake timeout` kind occur ≤ 1 time in a 50-account batch.

### P2-1 Mail polling 1s → 300ms

Part of the 14.8s in `verify` is polling-granularity loss (0.5s/account on average). Changing to 300ms saves about 0.3-0.5s/account, at the cost of 3× the request volume to the CF Worker. Keep the default at 1s and make it a parameter.

### P2-2 Merge ledger / deliverable writes

Each account currently writes the ledger 3 times (each `Append` with its own `fsync`) plus 1 deliverable. Under high concurrency this is obvious IO amplification. Change to "batch append + one fsync per batch".

**Acceptance**: `Sync` calls in a 50-account batch drop from ~150 to ~5.

### P2-3 Surface rates in the console

`Timings` is already in the ledger; only the aggregated display is missing. Add to the "Register" page in the console: mean E2E per account for this batch, a stage-duration bar chart, and the failure-stage distribution. That way every change makes regressions visible.

---
## 7. Expected Benefits Summary

| Change | Per-order E2E | 50-order wall clock (concurrency 5) | Throughput |
| --- | ---: | ---: | ---: |
| Current state | 27.0s | 279s | 10.75 orders/min |
| + P0-2 | 23.7s | 245s | 12.2 orders/min |
| + P0-1 | 21.7s | 225s | 13.3 orders/min |
| + P0-3 | **15.5s** | **160s** | **18.8 orders/min** |
| + P1-1 (concurrency 10) | 15.5s | **80s** | **37.5 orders/min** |

In other words: without touching concurrency, code changes alone raise throughput by **75%**; raising concurrency to 10 then reaches **3.5×**.
## 8. Implementation Results (P0 / P1 / P2 All Delivered)

| Item | Status | Verification |
| --- | --- | --- |
| P0-1 No second probe on enrollment | ✅ | `enroll` phase 2.0s → **0.00s**; 10/10 enrolled in this batch, 0 failures |
| P0-2 Skip pre-check for fresh emails | ✅ | `exists` marked as `skipped (fresh)`, 10/10 |
| P0-3 Parallelize validation wait | ✅ | `validate` moved into the parallel window; `verify` and `validate` time ranges overlap within the same batch |
| P1-1 Concurrency does not exceed egress | ✅ | `CapConcurrency` unit tests, 4 cases; effective on both CLI and server |
| P1-2 Liveness check backoff retry | ✅ | `IsTransient` 16 cases, `theosWithRetry` 3 cases |
| P2-1 Adaptive polling | ✅ | Starts at 300ms, ×1.5 backoff up to the configured interval; covered by unit tests |
| P2-2 Batched ledger fsync | ✅ | 20 rows / 1s batches; `Ledger.Flush/Close` invoked by both CLI and server tasks |
| P2-3 Console rate metrics | ✅ | Registration page header shows per-account mean / p50 / p95 / phase durations |

### Measured Comparison (Before: 50 accounts @ concurrency 5 vs After: 50 accounts @ concurrency 10)

| Metric | Before (50 accounts / concurrency 5) | After (50 accounts / concurrency 10) |
| --- | ---: | ---: |
| Enrolled | 46/50 (49 after re-run) | **50/50** |
| Failed accounts | 4 | **0** |
| Wall clock | 279s | **233s** |
| Throughput | 10.75 accounts/min | **12.88 accounts/min (+20%)** |
| Per-account E2E mean | 27.0s | 31.4s (28.0s excluding outlier) |
| `exists` | 3.3s | **0 (`skipped (fresh)`)** |
| `enroll` | 2.0s | **0.00s** |
| `verify` | 14.8s | 20.3s |
| `validate` | 3.1s | 4.3s |
| Concurrency utilization | 4.83 / 5 = 97% | 6.73 / 10 = 67% |

**How to read these numbers**:

- **Definite gains**: 5.3s of removable overhead eliminated (`exists` 3.3 + `enroll` 2.0),
 enrollment failure rate dropped from 8% to **0**, throughput +20% (concurrency 5 → 10).
- **Per-account E2E did not decrease** (31.4s vs 27.0s): because `verify` (external email delivery) was
 5.5s slower this round, offsetting the 5.3s saved; excluding one 198s delivery outlier, it is 28.0s.
 Raising concurrency from 5 to 10 also increases per-account latency (egress/upstream contention, `validate` 3.1s → 4.3s).
- **Utilization of 67%** indicates that concurrency 10 is not yet saturated: the theoretical ceiling is `10 × 60 / 31.4 ≈ 19 accounts/min`,
 actual is 12.88; the gap comes from "5 waves × partial tail waves" and the startup interval spread by `MinInterval`.
 To speed up further, add egress (currently 12) rather than increasing concurrency.

Conclusion: **removable overhead is eliminated, failure rate is zero, and per-account latency is now entirely determined by email delivery**.
The next step can only yield gains from the email retrieval method (e.g., switching from polling to push), not from further gateway-side optimization.
## 10. P0-4: Move `verify` Off the Critical Path (Implemented)

The fifth batch of data makes the problem very clear:

| Batch | `verify` | Total of other stages | E2E | Throughput |
| --- | ---: | ---: | ---: | ---: |
| Third batch | 14.8s | 12.2s | 27.0s | 10.75 |
| Fourth batch | 20.3s | 11.1s | 31.4s | 12.88 |
| Fifth batch | **56.1s** | **11.1s** | 67.2s | 7.73 |

- The gateway side (`register` / `login` / `session` / `validate` / `enroll`) is nearly unchanged across the three batches, totaling **11~12s**;
- `verify` fluctuates wildly between 14.8s and 56.1s and is the only source of uncertainty;
- Moreover, it **does not affect quota**: all 14 accounts that timed out on verification in the fifth batch ultimately got 200 (filled in by delayed re-probing).

### Approach

Do not wait for verification when entering the pool; verification continues in the background:

```text
Now:    mailbox → register → login → session → validate ─┐
                              verify (blocking join) ──────┴→ enroll
After:  mailbox → register → login → session → validate → enroll
                              verify (background) ──────────→ backfill on success
```

- `enroll` carries `email_verified=false` (currently not disabled upstream);
- After background verification succeeds, backfill the account's `email_verified=true` via the admin interface;
- Requires a new background task pool + an `action=mark-verified`;
- The delayed wait window can be shortened to 45s (late-arriving emails no longer need to be waited for, since quota does not depend on them).

### Measured Results (Implemented)

| Metric | Fifth batch (blocking verification) | Sixth batch (P0-4) | Predicted |
| --- | ---: | ---: | ---: |
| Median per-account E2E | 52.5s | **12.3s** | ~11s |
| Mean per-account E2E (successful accounts) | 67.2s | **12.6s** | - |
| p90 / min | 107.6s / 36.4s | 15.6s / **9.1s** | - |
| Background verification backfill | - | **33/34** | - |
| Estimated throughput (concurrency 10) | 7.73 accounts/min | **~49 accounts/min** | ~45 |

Re-check with a 20-account confirmation batch (concurrency 10): 19/20 succeeded, median E2E **11.9s**, background verification backfilled 18/19 - consistent with the first batch.

Implementation highlights:

- `Enroller` gains a new optional interface `VerifiedMarker` (`MarkVerified(ctx, accountID, email)`);
- After entering the pool, `verifyBackfill` continues waiting for the email in a background goroutine **detached from the caller's ctx**; on success it clicks the link, writes a new ledger entry (Merge updates `verify` from `pending` to `ok`), then calls `MarkVerified` to backfill the account pool;
- All background writes operate on **record copies** to avoid racing with `finish()`/`notify()`;
- The only scenario that still waits synchronously: `/theos` returns `email_unverified`.
- The admin interface gains `action=mark-verified` (by id, falling back to email when missing, idempotent).

### Fixed: Registration POST Timeout Not Retried (Exposed by Sixth Batch)

```text
POST https://api.deepai.org/daily-time-sync/registration/ → 504 Gateway Timeout
```

Three changes:

1. **45s total budget**: `registerPostBudget` wraps the registration POST, failing fast (previously 3 internal retries × 60s client timeout ≈ 190s before declaring failure);
2. **Backoff can be interrupted by ctx**: the two `time.Sleep` calls in `do()` are replaced with `sleepCtx`; otherwise, even after ctx has already timed out, it would still sleep through 700ms/1400ms, rendering the budget meaningless;
3. **Login fallback**: when the registration POST fails (other than "already registered"), log in once with the same email and password - a 504 often means the account **has actually been created**, so a successful login continues, naturally idempotent.

Seventh batch measured (concurrency 10): **50/50 entered the pool, 0 failures, wall clock 82s**, median per-account E2E **11.7s**, throughput **36.6 accounts/min** (predicted ~45, the gap being 76% utilization).

### Fixed: CLI Exit Kills Background Verification

Background verification runs inside the CLI process and is terminated when the batch ends, causing `email_verified` in the account pool to be inaccurate (only 11/50 backfilled in the sixth batch). Added:

- `-verify-grace` (default 60s): wait at most this long after the batch ends;
- `Pipeline.PendingVerify()` / `WaitVerify(d)`: `WaitGroup.Add` is placed **before** starting the goroutine to avoid racing with `Wait`;
- After the wait ends, re-read records from the ledger so that summaries and deliverables reflect the true verification state.

Verification: a small batch of 5 accounts goes from `waiting for 5 background email verifications (up to 1m0s)…` to `5 emails verified`.

### Legacy Issue: Registration POST Has No Retry

The sixth batch (16/50) and the confirmation batch (1/20) got stuck at the same place:

```text
POST https://api.deepai.org/daily-time-sync/registration/ → 504 Gateway Timeout
```

- A single account wastes 188~219s (client timeout + internal retries), dragging the wall clock from ~60s to 430s;
- This call currently has **no retries at all**, and failure is fatal (the `register` stage is marked failed);
- Fix: back off and retry once on transient errors (1~3s); if upstream returns "already registered", take the existing `isAlreadyRegistered` branch to switch to login, naturally idempotent.

Expected benefit: failure rate 32% → <5%, and the batch wall clock is no longer dragged out by a single timed-out account.

Risks and mitigations:

- If upstream later makes "email unverified" a prerequisite for quota or inference, this change will break - an alert on the proportion of `email_verified` will surface this early;
- Background verification must be idempotent: clicking the link repeatedly for the same account has no side effects, and backfill is deduplicated by email.
## 11. Verifying the Upstream Cost of Clicks (HAR Measurement)

`deepai.org-2.har` (17:07 local time, one manual browser verification) gives the endpoint profile:

| Metric | Value |
| --- | --- |
| Request | `GET https://api.deepai.org/verify_email?key=<uuid>` |
| Response | `200 text/html`, server `nginx + Phusion Passenger` |
| `wait` (server-side processing) | **13.2 s** |
| ssl/connect / receive | 0.37 s / 0.33 s |
| Total | **13.9 s** |

In the same file, the fields under `/daily-time-sync/user/` are only
`pk / username / email / userprofile.api_key / userprofile2.plan / …`,
**with no email-verified field** - the upstream does not provide a pollable verification status.

Conclusion: this endpoint "works but is expensive," and concurrent requests will inevitably trigger Passenger worker timeouts (502
`Incomplete response received from application`). Therefore rate limiting is mandatory (currently: 2 concurrent globally,
backoff 2s/10s/60s), and **do not put it on the critical path** (already removed in P0-4).

Backfill capability: `register -reverify [-group <group>]`, re-fetch the link from the inbox → rate-limited click →
backfill `email_verified`. `stress-50i` / `stress-50j` / `stress-50k` all measured backfill to 100%.

**Boundary**: verification emails in temporary inboxes are retained for only a very short time. Backfilling within a few minutes after a batch ends still works,
but after a few hours the inbox is already empty - all 39 unverified accounts in the seventh batch timed out when backfilled afterward (no email within 25s).
So the correct approach is to **complete verification within the batch** (rate limiting exists precisely for this), not to remediate afterward.
## 12. Batch Stability Observations (Batches 6-10)

> This section serves only as a **regression baseline**: see `docs/lessons.md` §4 for the measurement methodology and reading conventions.

| Batch | Concurrency | Wall Clock | Pooled | E2E Median | p90 | Notes |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| 6 | 10 | 430s | 34/50 | 12.3s | 15.6s | Registration POST 504, no retries |
| 7 | 10 | 82s | 50/50 | 11.7s | 19.1s | Baseline after the fix |
| 8 | 10 | 228s | 50/50 | **11.9s** | 89.2s | Upstream slowed overall (verify endpoint failure) |
| 9 | 10 | 126s | 50/50 | **8.9s** | 13.4s | After verification rate limiting, verification/quota both 50/50 |
| 10 | 10 | 124s | 50/50 | **10.3s** | 13.8s | Config unchanged, stably reproduced |

Conclusions:

- **Per-order median E2E holds steady at 8.9-12.3s** - the critical path is now decoupled from upstream fluctuations;
 Batch 9 (verification rate limiting + normal upstream) achieved 8.9s / 45.5 orders/min, the current upper bound for this methodology;
- When upstream slows down, `register` / `login` / `session` / `validate` slow down in tandem
 (6.6 / 4.2 / 3.0 / 6.7s respectively in Batch 8, 2-5× those of Batch 7), manifesting as an elongated p90;
- Verification is the only step that can be interrupted entirely by an upstream endpoint failure (0/50 in Batch 8), but it is not on the critical path
 and does not affect quota, so this is an acceptable degradation.
## 13. What Not to Do

- **Do not remove `validate` to speed things up**: it determines whether an account is usable. Saving 3.1s in exchange for a bunch of
 zero-quota accounts is effectively pushing the cost to runtime.
- **Do not change `enroll` to a batch interface**: pool entry is the only write entry point for the state file,
 and batching increases rollback difficulty; the real cost of `enroll` is only 2.0s, and it is even lower after P0-1.
- **Do not mask per-account latency by increasing concurrency**: there are only 12 egress routes, and concurrency above 20 starts queueing,
 which manifests as a p95 spike rather than a throughput improvement.
## 14. Per-Account In-Flight Cap Comparison Load Test (2026-11-09)

**Problem**: The pool concurrency limit = `account count × maxInFlightPerAccount`, where the latter is a compile-time constant (`internal/server/common.go:132`, default **2**). 14 accounts = 28 slots, but **with client concurrency of 28 the success rate is only 48-70%**: when slots run out, the gateway doesn't queue - it switches accounts and retries, and once the retry budget (`max_retries`, default 3) is exhausted it immediately returns `429 rate_limit_error` (`All accounts are at capacity, please try again later`).

This round is measurement only, to decide whether bumping the constant from 2 → 3 is worth it, and which path is more cost-effective: "add accounts / tweak settings / change code".

### Method and Definitions

- Pool of **14 accounts** (`/health`: `accounts=14 ready=14`), model `glm-5.3-flash` (`theosPerRequest=0`, does not consume Theos), `stream=false`, `max_tokens=16`, with one system message;
- Client uses fixed concurrency `C` and total requests `N` (about 2 requests per concurrency slot, ensuring two full waves);
- Metrics: `ok` = 200; `429` counted separately, and only `error.type=rate_limit_error` is counted - this **is the gateway's own backpressure**, not upstream `409 session_busy` (which waits and retries internally, manifesting only as increased latency);
- `cap=3` variant: temporarily change the constant to 3, build a separate binary, **immediately `git checkout` to restore**, leaving no changes in the repo; the two rounds run **alternately** (cap2→cap3, then cap3→cap2) to cancel out upstream time-of-day drift;
- Restart the gateway between rounds (swap binary + reset round-robin cursor), use a copy of the state file, leave the real account pool untouched.

### 14.1 cap = 2 (current: 28 slots)

| Client concurrency | Requests | Success | 429 (capacity) | Wall clock | Success rate | P50 | P95 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 14 | 42 | 42 | 0 | 7.0s | **100%** | 1.18s | 3.97s |
| 20 | 40 | 40 | 0 | 5.1s | **100%** | 2.34s | 4.29s |
| 24 | 48 | 42 | 6 | 5.5s | 87.5% | 2.15s | 4.49s |
| 28 | 56 | 39 | 17 | 6.5s | 70% | 2.65s | 5.50s |
| 28 | 56 | 27 | 29 | 19.5s | 48% | 0.36s | 5.37s |

> The last two rows are two rounds of the same config; the difference comes from upstream time-of-day (the second round's wall clock of 19.5s is clearly slower); both rounds show large-scale rejections, indicating saturation is not incidental.

### 14.2 cap = 3 (42 slots)

| Client concurrency | Requests | Success | 429 | Wall clock | Success rate | P50 | P95 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 28 | 56 | **56** | 0 | 7.7s | **100%** | 2.09s | 5.65s |
| 28 | 56 | **56** | 0 | 11.7s | **100%** | 2.70s | 9.42s |

### 14.3 Settings-only change: `max_retries 3 → 16` (no code change)

| Client concurrency | Requests | Success | 429 | Wall clock | Success rate | P50 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 28 | 56 | 26 | 30 | 6.0s | 46% | **30ms** |
| 28 | 56 | 26 | 30 | 4.7s | 46% | 28ms |

> ⚠ The table above is the **pre-refactor** baseline. After 2026-11-09 the scheduling side has been changed to "scan the whole pool + bounded queueing for 2s" instead of "greedy 3 tries then 429", and 409 also switches accounts immediately - when re-testing with the scripts in this section, 429s should largely disappear; the success-rate improvement comes mainly from these two changes, not from bumping the constant from 2 to 3.

### 14.4 Conclusion

1. **The effective concurrency of cap 2 is not 28, but ~20** (≈1.4 per account): it starts leaking at 24, and at 28 only 48-70% remain;
2. **cap 3 has zero rejections at 28 concurrency** (two rounds of 56/56), with P95 in the same order of magnitude as cap 2 - what increases is capacity, not latency;
3. **Tuning `max_retries` doesn't help** (46%, and the P50 of 429s is only 30ms): 429 means insufficient slots, and rejected requests burn through the retry budget within tens of milliseconds. This is a pure capacity problem, not a retry-policy problem;
4. At 28 concurrency, cap2 cannot fill the theoretical 28 slots; there are two amplifiers in the code path:
 - A failed `acquireAccount` also counts against the retry budget (the `continue` at `common.go:227`); under saturation, greedy account selection gives up after trying only `max_retries` accounts;
 - Slots are returned only when the handler returns (`releaseBody`); when the client sends the next request immediately upon receiving a response, it hits the window of "response received, slot not yet returned".
 So the requirement for clients is: **back off immediately on `429 rate_limit_error`**; a retry storm only amplifies rejections.

Choose the lever by target concurrency:

| Target concurrency | Approach |
| --- | --- |
| ≤ 20 | No change needed (cap 2 is sufficient) |
| 24 ~ 28 | Constant `maxInFlightPerAccount 2 → 3` (requires rebuild), measured zero rejections |
| > 28 | **Add accounts** (+2 slots per account) is more stable than continuing to raise the constant: upstream per-account capacity (`409 session_busy`) is the hard constraint |

### 14.5 Reproduction

Two scripts, runnable directly under `scripts/` (you must first start the gateway as in §1; a copy is recommended for the real account pool).

```js
// scripts/ab-load.mjs — fixed-concurrency load test, outputs one line of JSON
// Usage: BASE=http://127.0.0.1:10087 KEY=sk-... CONC=28 TOTAL=56 node scripts/ab-load.mjs
const BASE = process.env.BASE || 'http://127.0.0.1:10087';
const KEY = process.env.KEY;
const CONC = Number(process.env.CONC || 28);
const TOTAL = Number(process.env.TOTAL || 56);
const MODEL = process.env.MODEL || 'glm-5.3-flash';

const body = JSON.stringify({
  model: MODEL, stream: false, max_tokens: 16, temperature: 0,
  messages: [
    { role: 'system', content: 'You are a helpful AI assistant.' },
    { role: 'user', content: 'Reply with exactly: OK' },
  ],
});

async function one() {
  const t0 = performance.now();
  try {
    const r = await fetch(BASE + '/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + KEY },
      body, signal: AbortSignal.timeout(180000),
    });
    const text = await r.text();
    let j = null; try { j = JSON.parse(text); } catch {}
    return { status: r.status, ms: performance.now() - t0, type: j?.error?.type || '' };
  } catch {
    return { status: 0, ms: performance.now() - t0, type: 'client_error' };
  }
}

const t0 = performance.now();
let next = 0;
const out = [];
const workers = [];
for (let w = 0; w < Math.min(CONC, TOTAL); w++) {
  workers.push((async () => {
    for (;;) {
      const i = next++;
      if (i >= TOTAL) return;
      out.push(await one());
    }
  })());
}
await Promise.all(workers);
const wall = performance.now() - t0;

const ms = out.map(r => r.ms).sort((a, b) => a - b);
const q = p => ms.length ? Math.round(ms[Math.min(ms.length - 1, Math.floor(ms.length * p))]) : 0;
const byStatus = {}, byType = {};
for (const r of out) {
  byStatus[r.status] = (byStatus[r.status] || 0) + 1;
  if (r.status !== 200) byType[`${r.status} ${r.type || '-'}`] = (byType[`${r.status} ${r.type || '-'}`] || 0) + 1;
}
console.log(JSON.stringify({
  conc: CONC, total: TOTAL, wall_s: +(wall / 1000).toFixed(1),
  rps: +(out.length / (wall / 1000)).toFixed(2), ok: byStatus[200] || 0,
  byStatus, p50: q(0.5), p95: q(0.95), p99: q(0.99), errors: byType,
}));
```

```bash
#!/usr/bin/env bash
# scripts/ab-arm.sh — start gateway → create temp Key → load test → stop gateway (one round)
# Usage: CONC=28 TOTAL=56 ./scripts/ab-arm.sh ./bin/deepaicode2api
set -euo pipefail
BIN=${1:?Usage: ab-arm.sh <binary>}
WORK=$(mktemp -d)
cp deepaicode2api-data.json "$WORK/state.json"    # copy: leave the real account pool untouched
"$BIN" -data "$WORK/state.json" -port 10087 >"$WORK/gw.log" 2>&1 &
GW=$!
trap 'kill $GW 2>/dev/null || true; rm -rf "$WORK"' EXIT
until curl -sf -m1 -o /dev/null http://127.0.0.1:10087/healthz; do sleep 0.5; done
KEY=$(curl -sS -X POST -H 'Content-Type: application/json' -d '{"name":"ab","group":""}' \
  http://127.0.0.1:10087/admin/api/keys | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
# When testing only max_retries, insert this line here (no rebuild needed):
# curl -sS -X POST -H 'Content-Type: application/json' -d '{"max_retries":16}' \
#   http://127.0.0.1:10087/admin/api/settings >/dev/null
BASE=http://127.0.0.1:10087 KEY="$KEY" CONC=${CONC:-28} TOTAL=${TOTAL:-56} node scripts/ab-load.mjs
```

```bash
# cap=3 variant: temporarily change the constant → build a separate binary → restore immediately (no changes left in the repo)
python - <<'PY'
p='internal/server/common.go'
s=open(p, encoding='utf-8').read()
open(p,'w',encoding='utf-8',newline='').write(s.replace(
    'const maxInFlightPerAccount = 2', 'const maxInFlightPerAccount = 3'))
PY
go build -o /tmp/gw-cap3 . && git checkout -- internal/server/common.go
go build -o /tmp/gw-cap2 .
git status --short        # should be empty
```

When re-running, alternate "cap2 → cap3 → cap3 → cap2", with `CONC=28 TOTAL=56` each round, and compare success rate and P95.
