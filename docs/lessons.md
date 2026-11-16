# Engineering Lessons Learned (Ten Batches of Load Testing, 2026-11)

This document turns the ten batches of 50-account load testing from 2026-11 (register → login → fetch Key → validate → enroll → inference)
**from execution logs into reusable rules**. The README no longer keeps a per-batch running log; it retains only conclusions and
acceptance baselines. Raw numbers are in `docs/performance.md`, and the change history is in `docs/changes.md`.

> **Note 2026-11-10**: The email registration path (`internal/register`, `cmd/register`,
> `/admin/api/register`, and the console's "Batch Register" page) has been removed entirely. References in this document to
> `internal/register/*` under "code location" are historical records, used only to explain the decisions made at the time;
> the current proxy pool is in `internal/proxy`, and the account-pool write entry point is `internal/server/enroll.go`.
>
> Cleanup on the same day: all HAR captures (`accounts/*.har` and `deepai.org*.har` in the repository root)
> have been deleted; credentials that were not committed to the repository have been folded into the encrypted state file (in disabled state, see `changes.md` §35).
> File names such as `deepai.org-2.har` mentioned in this document serve only as evidence indexes; the files themselves are no longer in the repository.

Each lesson follows the format **Symptom → Root cause → Rule → Code location**, with failure boundaries given at the end.
Before changing code related to registration / scheduling / probing, go through the checklist in §5 first.

---

## 1. How to Split the Critical Path

### 1.1 Only stages that "determine whether an account is usable" belong on the critical path

- **Symptom**: In batches 3-5, per-account end-to-end time went 27.0s → 31.4s → 67.2s; all the variance came from
 `verify` (waiting for the verification email / clicking the verification link). On the gateway side, `register` / `login` /
 `session` / `validate` together stayed stable at 11-12s.
- **Root cause**: `verify` is an **uncertain wait on an external service**, and it does not affect usability:
 `/theos` only looks at `Api-Key`; an account with an unverified email can still run inference and still has a 200 quota.
- **Rule**: The critical path = stages where "if you can't get it, the work is wasted" (`register` / `login` /
 `session` / `validate` / `enroll`). All other waits (email, verification click, quota re-probe)
 should be moved to the background with backfill.
- **Code location**: `internal/register/pipeline.go` (`validate` before join,
 `go p.verifyBackfill(...)` after `enroll`), `internal/register/pipeline_verify.go`.
- **Benefit**: Per-account E2E median 52.5s → **12.3s** (batch 6), and it has since remained stable at 8.9-12.3s.

### 1.2 Background tasks must not be tied to the caller's ctx or the process lifetime

- **Symptom**: Three separate incidents with the same root cause -
 1. In the console's "Refresh All Quotas", closing the page caused `r.Context()` to be canceled, and half of the 653 accounts were never probed;
 2. When a CLI batch ended, the process exited and took the background verification goroutines with it, so in the deliverables
 `email_verified` was all false;
 3. Background verification finished only after the batch ctx was canceled, and its write-back raced with `finish()`.
- **Root cause**: Background tasks borrowed a request-scoped ctx.
- **Rule**: Background tasks should use `context.Background()` + their own timeout; write-back should operate only on a copy of the record;
 before the process exits, it must explicitly wait (with an upper bound), or hand the task to a long-running process (the server).
- **Code location**: `verifyBackfill`'s independent `WithTimeout`; `cloneRecord`;
 CLI `-verify-grace` (default 60s) → `Pipeline.WaitVerify`; server-side quota tasks
 are not bound to `r.Context()`.

### 1.3 No "second probe" on the critical path

- **Symptom**: During enrollment, `/theos` was called again (2.0s), with an 8% failure rate,
 and at the moment of enrollment the upstream often returned `balance=0`, so probing was pointless anyway.
- **Root cause**: "Validation" and "writing to the account pool" were coupled in a single stage.
- **Rule**: Confirm the same fact only once, in one stage. Enrollment only writes state; quota is left to
 background re-probing (see §3.3). Repeated probing only amplifies upstream jitter and the failure surface.
- **Code location**: `internal/server/register.go` `enrollRegistration`
 (the only write entry point; it does not probe), `allowanceFromEnroll` (enrollment carries a snapshot).

---

## 2. Upstream Call Discipline

### 2.1 Every upstream POST needs an overall budget, and backoff must be interruptible by ctx

- **Symptom**: In batch 6, 16/50 accounts were stuck on **504 Gateway Timeout** from `POST /daily-time-sync/registration/`;
 each account waited 188-219s for nothing, and the whole batch took 430s wall-clock.
- **Root cause**: The default 120s timeout + backoff using `time.Sleep` (after a timeout it still had to sleep 700ms+1400ms).
- **Rule**: Give upstream POSTs an **overall budget smaller than the global timeout** (45s for registration),
 and always use cancelable `sleepCtx` for backoff, so batches can fail fast without dragging out the tail.
- **Code location**: `internal/register/web.go` `registerPostBudget`, `sleepCtx`.

### 2.2 A 504 does not mean failure - fall back to login

- **Symptom**: A substantial portion of accounts that got a 504 had actually **already been created**; only the response was lost.
- **Rule**: After a registration failure, do not declare it dead immediately; try login first: if login succeeds, continue to fetch the Key,
 which gives you idempotent semantics. If the upstream returns "already registered", also take the login branch.
- **Code location**: `isAlreadyRegistered` in `internal/register/` (`pipeline_batch.go`)
 + the branch in `pipeline.go` that switches to login.

### 2.3 Heavy endpoints must be rate-limited: measure endpoint cost before setting concurrency

- **Symptom**: In batch 8, email verification was **0/50**: 17 timeouts, 16 `unexpected EOF`,
 2 `502 Incomplete response`. During the same period, manual verification in a browser was normal at `201 / 4.8s`.
- **Root cause**: The HAR profile for `GET /verify_email?key=…` (`deepai.org-2.har`) is
 `nginx + Phusion Passenger`, with **wait 13.2s** and 13.9s total. Ten background
 goroutines clicking at the same time saturated the upstream worker pool - **the endpoint was not broken; we rate-limited the upstream ourselves**.
- **Rule**: Before increasing concurrency, measure the server-side latency of a single call; for any endpoint with
 "multi-second CPU/worker occupancy", use a global concurrency gate + long backoff, never put it on the critical path, and never do per-account concurrency.
- **Code location**: `internal/register/reverify.go` `verifyClickSlots` (global 2),
 `verifyRetryDelays` (2s / 10s / 60s; no retry on 4xx), `verifyEmailThrottled`.
- **Benefit**: Verification went from 0/50 → 49/50 within the batch, with `-reverify` filling in 1 more, for 50/50 total
 (first round 8m04s, retry round <1m).

### 2.4 Retries must exist at only one layer

- **Symptom**: When a heavy endpoint failed, both the inner and outer layers retried 3 times each → one business action hit the upstream 9 times.
- **Rule**: Low-level functions should perform only a **single attempt**; retry policy should be centralized in the single outer layer;
 retry sequences should be tiered by endpoint cost (fast endpoints at the 700ms level, 13s-level endpoints at 2s/10s/60s).
- **Code location**: `VerifyEmail` single attempt + outer `verifyEmailThrottled` for unified retries.

### 2.5 Route by error type instead of retrying blindly

- **Rule**:
 - Retryable: `5xx`, `timeout`, `unexpected EOF`, `TLS handshake`, egress errors;
 - Not retryable: `4xx`, `social_login_required`, `machine_required`,
 `lane_version_unsupported` - these are policy/protocol decisions, and retrying only wastes time;
 - Needs waiting: `409 session_busy` + `Retry-After`; switch accounts or wait as instructed.
- **Code location**: Error semantics in `internal/deepai` + `internal/retry`,
 `IsTransient` / `IsSocialLoginRequired`.

### 2.6 Egress failure ≠ bad account

- **Symptom**: `No exit node` was tied to a single egress, and all 17 accounts had 0 quota;
 after switching egress, all recovered to `balance=200`.
- **Rule**: Check egress first, then delete accounts. Proxy binding is modifiable metadata
 (`assign-proxy`, which by default only fills empty values); re-registration is not needed.
- **Code location**: `internal/register/proxy.go`, `assign-proxy` in `/admin/api/accounts`.

### 2.7 Request headers are part of the protocol: the UA determines which model the upstream gives you

- **Symptom**: With the same Key, the same machine, and the same body, the model fingerprint measured through the gateway was
 `muse-wonder` (Meta family), while direct client testing gave `glm-5.3-flash`. `Accept` /
 `Accept-Language` / `sec-fetch-mode` were all tried; changing only `User-Agent` reproduced it.
- **Root cause**: The upstream routes by `User-Agent` to different backends - `node` (desktop Node fetch)
 goes to the real model; `Go-http-client/1.1` or a Chrome UA is switched to another backend, while the response's
 `model` still says `z-ai/glm-5.3-flash`, so validating only `model` will never catch it.
- **Rule**: **Every header on inference requests must match the desktop client**. The registration path (web UA +
 `sec-ch-ua`) and the inference path (Node fetch default headers) must be two separate sets.
- **Criterion (updated 2026-11-09)**: The upstream now forwards requests to an OpenRouter-like router;
 the real branch's `provider` is the **actual backend name** (`InferenceNet` / `Wafer` / `Reka` /
 `BaseTen` / `CoreWeave` / `Makora` / `Modal` / `Morph` / `Inceptron`...),
 so the old criterion "`provider` present and no `system_fingerprint`" would misclassify all real branches
 as substitutes. Now only check whether **`provider` equals `"DeepAI"`**: equal = substitute, not equal = real.
 > ⚠️ **Fully invalidated as of 2026-11-13**: In the 0.3.36 era, responses for the three UAs are completely identical; `provider` /
 > `service_tier` / `cost_details` no longer appear; `id` is uniformly `gen-<32 hex>`,
 > and `model` is back to `zai/<slug>`. "Judging substitutes by response shape" currently has no reliable criterion; use model fingerprints for account validation.
 > See `docs/recon.md` §3.1. The two cross-checks below are likewise historical only.
 - Cross-checks: `service_tier` (real `null` / substitute `"default"`),
 `usage.cost_details.upstream_inference_cost` (real >0 / substitute `0`),
 `reasoning_details[*].format == "openrouter-stealth-v1"` (substitute);
 - `system_fingerprint` **is no longer a criterion**: among real branches, only vLLM-family backends provide it;
 - Both branches return `model` as `z-ai/glm-5.3-flash`, so it **cannot** be used for judgment;
 - The substitute on 2026-11-09 was the stealth channel of `crouton-alpha` (on 11-08 it was `muse-wonder`):
 the backend changed but the mechanism did not. See `docs/recon.md` §3.1.
- **Code location**: `deepai.Client.BrowserFingerprint` (`internal/deepai/client.go`,
 `NewClient` / `NewClientWithProxy` = inference form, `NewClientWithFingerprint`
 = registration form), `desktopInferenceHeaders`.
- **Another admission condition**: A `node` UA alone is not enough; you must also **complete check-in** (fetch
 nonce from `/theos` and send it back). Measured: fetch-only without sending back = 0/14 aligned; full check-in = 14/14 aligned;
 the decision is remembered per (account, machine fingerprint); changing the fingerprint is equivalent to a cold start.
 > These two numbers were judged on 2026-11-08 by "has `system_fingerprint`, no `provider`";
 > the response shape changed on 11-09, so the new criterion (`provider != "DeepAI"`) needs re-testing.
- **Check-in is a limited resource**: `403 check_in_refused` ("restarted its check-in too many
 times today") means the day's restart count is exhausted and you must wait until tomorrow. Do not write scripts that repeatedly "fetch nonce + send back"
 as a health check; a rate-limited account can still run inference, it just keeps landing on the substitute branch.
- **Cost**: `user-agent: node` is the default for Node fetch (retested 2026-11-13: the UA no longer
 routes; keeping it only matches the desktop client, see `docs/recon.md` §3.1).

---

## 3. Quota and Scheduling

### 3.1 Machine fingerprints must be unique per account

- **Symptom**: Before fingerprints were filled in for 21 accounts, those with `balance>0` were **0/21**; after filling them in, **20/21**
 (the only 0 was a desktop-imported account whose daily quota had already been consumed).
- **Root cause**: The upstream issues free Theos **only once per day** per `X-DeepAI-Code-Machine`;
 when fingerprints are shared, accounts in the pool cancel each other out.
- **Rule**: Generate an account-level UUID at registration time and persist it with the record/account pool (`Account.MachineID`);
 reuse it throughout inference and `/theos`; only historical/desktop-imported accounts fall back to the process-level fingerprint.
- **Code location**: `deepai.NewMachineUUID`, `Account.MachineID` (shared by inference and
 `/theos`), `/admin/api/accounts {"action":"assign-machine"}`
 (fills empty values only). **Do not** remove this fallback logic for convenience.

### 3.2 `balance=0` means "unknown", not "no quota"

- **Symptom**: At the moment a new account is enrolled, `/theos` often returns `balance=0`, and a re-probe a few minutes later shows 200;
 the old implementation cached this 0 as fact, and the account was skipped by the scheduler for a long time.
- **Rule**: Zero quota + snapshot older than `QuotaStaleAfter` (15 minutes) → treat as unknown,
 allow rescheduling; stop as soon as enrollment/background re-probe gets a result.
- **Code location**: `internal/pool/pool.go` `QuotaStaleAfter` / `HasQuota`;
 `internal/server/register.go` `quotaReprobeDelays` (45s / 2m / 6min).

### 3.3 Re-probing needs three layers + global slots + in-flight deduplication

- **Symptom**: Early on, "Refresh All Quotas" could only be clicked manually; 653 accounts took 11 minutes,
 and a single HTTP request could not finish it; conversely, a full concurrent scan is an obvious risk-control signature.
- **Rule**: Three paths share the mechanism -
 1. **Enrollment re-probe**: 45s / 2m / 6min, stop as soon as a result is obtained;
 2. **On-demand re-probe**: when inference selects an account with a "stale snapshot", at most once every 15min;
 3. **Background guard**: one batch every 5 minutes (≤24 accounts), selecting only those whose `refresh_at` has passed /
 have never been probed / have not been probed for over 6h;
 4. Shared in-flight deduplication table + **4 global probe slots**.
- **Code location**: `internal/server/probe.go` (`probeWorkers`, `QuotaGuardInterval`,
 `QuotaGuardBatch`, `quotaNeedsRefresh`, `ReprobeStale`).

### 3.4 Probe failures must not change scheduling state

- **Symptom**: A brief upstream hiccup marked accounts as `error` / incremented `Failures`, and the account pool was misjudged.
- **Rule**: Re-probe failures should only be logged and must **not** change `Status` / `Failures` (do not call
 `SetAccountAllowance`); only real inference failures should affect scheduling counts.
 The exception is a terminal policy state (`social_login_required`), which must be persisted as `blocked`,
 otherwise the scheduler will keep retrying an account that will never succeed.
- **Code location**: `internal/server/probe.go` `probeAsync` (the error branch returns directly;
 only policy errors go through `BlockAccount`) vs `MarkAccountResult`.

### 3.5 Long tasks must be batched + have progress semantics

- **Rule**: Do not design admin APIs as "run N hundred accounts in one go". Use
 `stale_only` / `limit` / `remaining` so callers can page through; push each batch's probed `checked_at`
 to now, so the next batch naturally picks accounts that have not been probed.
- **Code location**: `POST /admin/api/checkin` (`maxCheckinBatch`),
 the console automatically pages by `remaining`.

### 3.6 Egress is both capacity and a quota boundary

- **Symptom**: After spreading 14 accounts across 12 egresses, accounts with quota went from 5 → 10.
- **Root cause**: The daily free Theos is issued **per network (egress IP)**
 (`network_bound`, see `docs/recon.md`).
- **Rule**: Add egress before adding accounts; align the concurrency cap with the number of egresses
 (`CapConcurrency`), otherwise you only raise p95 instead of increasing throughput.

---

## 4. Observability and Acceptance Criteria

### 4.1 No per-account instrumentation means no optimization

- **Rule**: Every account must have `CreatedAt` / `FinishedAt` / per-stage `Timings`,
 with `E2E = FinishedAt - CreatedAt` (**excluding** queue time).
 A batch without per-account data only shows wall-clock time and cannot be attributed.
- **Code location**: `internal/register/types.go` `Record.Mark` / `E2ESeconds`,
 `pipeline_batch.go` writes `FinishedAt`; the console registration page shows per-account mean / p50 / p95 / stage timings.

### 4.2 Look at the median and the tail, not just the mean

- **Symptom**: In batch 8, the upstream slowed down overall; **the median 11.9s barely moved**, but p90 rose from 19.1s
 to 89.2s, and the mean rose to 25.4s (dragged up by a few 120s timeouts).
- **Rule**:
 - **Median = architectural stability** (whether it is decoupled from upstream jitter);
 - **p90 / max = dependency health** (which upstream endpoint has a problem);
 - The mean is only for total/throughput accounting; do not use it to compare batches.

### 4.3 Utilization determines the next action

- **Rule**: `utilization = sum(E2E) / wall-clock`.
 - Utilization > 90%: the bottleneck is per-account latency → optimize stages;
 - Utilization < 75%: the bottleneck is queueing/tail waves/egress → add egress or adjust concurrency;
 - When per-account latency is already determined by external services, further gateway-side optimization has negative returns.

### 4.4 Only re-running the same configuration counts as a control

- **Symptom**: Batch 4 was 233s / batch 5 was 388s with identical configuration; 100% of the difference came from email delivery.
- **Rule**: Any conclusion of "X% speedup" must be based on **≥2 batches with the same configuration**, and must distinguish
 "code benefit" from "upstream jitter"; batches within an upstream failure window can only be used to verify **degradation behavior**,
 not to evaluate performance.

### 4.5 Acceptance criteria must be written separately

- **Rule**: Count enrolled / quota / verified separately, and annotate reruns separately
 (write `49/50 → +1 → 50/50`), do not combine them into one "success rate".
 The three numbers have completely different physical meanings: enrolled = schedulable, quota = can run inference, verified = long-term stability only.

### 4.6 Load-test the registration plane and the data plane separately

- **Rule**: 100% registration only means accounts can be created; you must also run a separate data-plane round
 (e.g. 60-120 requests / concurrency 6-12, measuring rps / p50 / p90 / account coverage),
 otherwise "the account pool is full but inference is all 429/403" will only be discovered in production.

---

## 5. Pre-Change Checklist

| # | Check item | Consequence if not met |
| --- | --- | --- |
| 1 | New upstream calls have an overall budget and can be interrupted by ctx | An account waits 2 minutes for nothing; wall-clock is dragged out by one account |
| 2 | New waits are not on the critical path (or are proven necessary) | Per-account E2E doubles and fluctuates with external services |
| 3 | New background tasks have an independent ctx + write-back uses a copy | Races / data loss / lost on process exit |
| 4 | Heavy endpoints have a global concurrency gate | You rate-limit the upstream into 502/EOF yourself (0/50) |
| 5 | Retries exist at only one layer and are routed by error type | Requests amplified 3-9×, triggering risk control |
| 6 | Zero-quota/failure decisions do not accidentally affect scheduling | The account pool has "accounts but they are unusable", requiring manual error clearing |
| 7 | Per-account timing and stage instrumentation exist | Cannot attribute; can only guess |
| 8 | Acceptance numbers are split by enrolled/quota/verified | Inflated success rate, masking real regressions |
| 9 | State file/ledger changes still preserve redaction and `0600` | Passwords and Keys leaked (see README §6.12) |
| 10 | When batches run in parallel, egress count ≥ concurrency | p95 spikes, throughput does not increase |

---

## 6. Boundaries: When These Lessons Become Invalid

- **The upstream makes email verification a prerequisite for quota/inference**: `verify` must return to the critical path,
 but keep rate limiting (§2.3), and use the `email_verified` ratio for alerting to detect it early;
- **Temporary email retention is very short** (measured: the inbox is empty after a few hours):
 therefore "complete verification within the batch" is a hard constraint; `-reverify` is only a fill-in measure, not a fallback;
- **`/theos` semantics change** (e.g. zero quota is no longer automatically refreshed): the "0 = unknown" rule in §3.2
 becomes a misjudgment, and a `refresh_at` check must be added to re-probing as a fallback;
- **The upstream tightens social login** (since 0.3.19, email/password accounts get `403 social_login_required`):
 this is a **policy** failure; any retry/egress switch is ineffective, and it must be identified and marked `blocked`.
 Note that it is **phase-based**: retesting on 2026-11-09 showed the restriction had been lifted (see `recon.md` §4.1),
 but error-code handling must be retained - policy can tighten again at any time; do not hard-code "email accounts are usable" as a criterion.
