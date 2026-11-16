# Reverse-Engineering Findings Quick Reference

The content here comes from black-box observation of the DeepAI Code desktop client (Electron + local t3code server), plus `deepai.org.har` packet captures. **This file is a substitute for the HAR**: the HAR is large and contains my own credentials, so it isn't checked in; the findings are recorded here in text form for easy searching and diffing.

> Capture time: app version **0.3.20** (lane 4, released 2026-11-07).
>
> **Re-verified 2026-11-15 up to 0.3.37 (lane 6, released 2026-11-14T08:08Z).**
> The official `latest.yml` is already at 0.3.37 (this machine is still on 0.3.36). Unpacked `server.asar`
> from the downloaded installer and did a full diff: across 375k lines there are only 27 hunks,
> and **the only one touching the inference protocol surface is lane 5 → 6**
> (`DEEPAI_LANE_HEADERS["X-DeepAI-Code-Theos"] = "6"`). The endpoint set, the 5
> `X-DeepAI-Code-*` headers, instance-ID granularity, machine fingerprint, check-in / beacon shape, chat request body fields,
> and `/models` decoding are all unchanged (pi runtime is still 1.3.0). The remaining differences are long-session compression, tool-failure
> dedup / hints, Codex approval, SQLite indexing, and `thinkingLevelMap` adding `max` / `xhigh` for `deepai-cyber` /
> `muse-ember` (affects only the desktop dropdown, not the wire protocol).
> [Source-level + empirical]
>
> Lane thresholds (measured 2026-11-15): `/models` lane 6 → 200, 7 → 426;
> `/theos` lane 5 / 6 → 200 (`lane_version: 6`), 4 → 426;
> `/chat/completions` lane 5 / 6 → 200. **`/models` is still lenient toward old lanes** (lane 5 also returns 200),
> but the two catalogs differ: lane 6 collapses the `contextWindow` of all 27 models to **320,000**
> (lane 5 has each model's real limit: 1M / 991k /...). So lane probing must start from 6,
> otherwise you'll keep getting the old catalog - the gateway probes in this order as of 2026-11-15.
>
> **Re-verified 2026-11-14: the client is still 0.3.36 (`latest.yml` unchanged, sha512 identical to 11-13),
> but the upstream server changed in two places, neither of which is in the client code.**
> 1. **`/models` now enforces auth**: without an `Api-Key` it always returns 401
> `{"code":"auth_required","message":"Sign in to DeepAI to use DeepAI Code."}`;
> with any account's Key (lane 5 + machine fingerprint header) it returns 200. [Measured 2026-11-14]
> This invalidates the 11-13 conclusion that "the same catalog is returned with or without a Key" - the gateway's previous credential-free
> catalog refresh / lane probing will now inevitably get 401.
> 2. **The model catalog grew from 21 to 27** (11 free / 16 paid, none retired): newly added `muse-ember` (free),
> `kimi-k3.1-pro`, `gpt-6.1-terra`, `gemini-4-hydrogen`, `gemini-4-carbon`,
> `claude-sonnet-5-5`; `default` is still `glm-5.3-flash`. [Measured 2026-11-14]
>
> Lanes unchanged: `/theos` lane 5 → 200 (`lane_version: 5`), lane 4 / 6 → 426
> `lane_version_unsupported`, consistent with 0.3.36.
>
> **Re-verified 2026-11-13 up to 0.3.36** (0.3.36 is installed on this machine; the official update source `latest.yml` was published the same day at 05:42Z).
> Five rounds of white-box comparison, 0.3.20 → 0.3.30 → 0.3.32 → 0.3.35 → 0.3.36: **the only protocol-surface change is lane
> 4 → 5** (`DEEPAI_LANE_HEADERS["X-DeepAI-Code-Theos"] = "5"`, and `/theos` also returns
> `lane_version: 5`). The request header set, instance-ID granularity (once per process),
> `sha256(lowercase raw UUID)` machine fingerprint, check-in / beacon shape, endpoint set, chat request body fields,
> and model catalog decoding are all unchanged (pi runtime is still 1.3.0, MCP tool set is still 25).
> The other differences in 0.3.36 are not on the inference protocol surface: `/models` now sends an `Api-Key` (the catalog does not vary by account;
> measured that with or without a Key it returns the same 21 models), chat now forces `ensureCheckedIn` beforehand,
> login token exchange gained retries / certificate-error messages, and a new managed-host control plane
> `/deepai_code/connect/*` was added (device registration / remote hosts, see the end of §2).
>
> **Re-tested the §3.1 routing criterion on 2026-11-13: UA-based routing no longer holds.** With the same account and the same request body,
> the response shapes for `node` / `curl` / Chrome 156 UAs are completely identical (`model` always echoes
> `zai/<slug>`, `id` is always `gen-<32 hex>`, and none have `provider` / `service_tier` /
> `native_finish_reason` / `cost_details` / `market_cost`). The old
> "missing `provider` = proxy" and "`gen_` prefix = proxy" criteria are **all invalid**; see §3.1 for details.
>
> **Re-verified 2026-11-11 up to 0.3.35** (the official update source `latest.yml` was published the same day at 03:34Z).
> Conclusion from four rounds of white-box comparison, 0.3.20 → 0.3.30 → 0.3.32 → 0.3.35: **the wire protocol is unchanged, character for character** -
> the endpoint set, the 5 `X-DeepAI-Code-*` headers (lane still `4`), instance-ID granularity,
> `sha256(lowercase raw UUID)` machine fingerprint, and check-in / beacon shape are all identical.
> The `src/deepai/*` changes in 0.3.35 are only three, and none are on the Windows protocol surface:
> Linux uses `/etc/machine-id` for the machine fingerprint (corresponding to the releaseNotes "fix Linux free daily
> Theos not being granted"), Linux uses the undici proxy transport via `fetchDeepAI`,
> and image / text array handling on the pi side (both versions use pi runtime 1.3.0).
> Only the `v=` in the beacon needs to track the version. 0.3.17 (lane 2) is no longer accepted upstream:
> all `/deepai_code` endpoints return `426 lane_version_unsupported`.
> Upstream can change at any time; the code is authoritative for fields.
>
> ⚠ The download page and the update source can be out of sync: to determine the "latest client", look at `latest.yml`, not the download page
> (on 2026-11-10 the download page's 302 still pointed to 0.3.30, while the update source was already 0.3.32).
>
> On 2026-11-09 I did a white-box re-verification using the 0.3.20 `server.asar` (request headers / machine fingerprint /
> check-in / response criteria), and re-tested UA routing: **the inference response has been changed to the OpenRouter shape**,
> so the old criterion "`provider` present = proxy" is invalid - see §3.1.
>
> **Re-tested again on 2026-11-10: the shape changed again; the proxy branch no longer sends a `provider` field.**
> §3.1 has been fully rewritten based on that day's measurements; the old criteria (`provider == "DeepAI"`,
> `service_tier == "default"`, `reasoning_details[0].format == "openrouter-stealth-v1"`)
> are **all invalid**; using them to inspect accounts will yield the opposite conclusion.
## 0. Evidence Grading

Every conclusion in this document is tagged - **read the tag before the content**:

| Tag | Meaning | How to verify |
| --- | --- | --- |
| **[Source-code level] ** | White-box confirmation: based on reading the 0.3.37 client `app.asar` / `server.asar` (`src/deepai/*.ts`, bundled into `dist/binCli-*.mjs`) | Unpack the asar and check against the code; 100% reproducible |
| **[Measured] ** | Actually tested online, with date and sample size | Re-run using the commands in this document; conclusions with small sample sizes are noted |
| **[Inferred] ** | Generalized from the previous two categories, **not yet verified** | Run a real test before changing code accordingly |

Paragraphs without a tag are background information (endpoint lists, field names); the code is authoritative.
Pay special attention to tags on criteria-type conclusions ("if you see X, it means Y") - when upstream changes the response shape, old [Measured] results become outdated (§3.1 is an example).

---
## 1. Process and Data Directories [Source-level 0.3.37]

| Item | Value |
| --- | --- |
| Application form | Electron shell (`app.asar`) + standalone server (`server.asar`, approx. 200 MB) |
| Data directory | `T3CODE_HOME` or `~/.deepai-code`; state directory `<base>/userdata` |
| Credentials file | `<stateDir>/secrets/deepai-account.bin` |
| Credentials content | `{"apiKey":"…","username":"…","isPro":false}`, **plaintext JSON**, permissions 0600 |
| API Key format | UUID (`8-4-4-4-12`) |

See `DesktopSecretDirs` in `internal/deepai/helpers.go` for the automatic detection order.
## 2. Upstream Endpoints [Source-level 0.3.37]

Base URL `https://api.deepai.org` (can be overridden with `DEEPAI_API_ORIGIN`).

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/deepai_code/chat/completions` | Inference (OpenAI chat compatible) |
| POST | `/deepai_code/responses` | Inference (only for models with `api=openai-responses`) |
| GET | `/deepai_code/models` | Model catalog. **As of 2026-11-14, `Api-Key` is required**: without it, always 401 `auth_required`; with any account's Key, 200 (the catalog does not vary by account). Also serves as the lane probe path (§2.2) |
| GET | `/deepai_code/theos` | Quota / balance + **check-in challenge** |
| POST | `/deepai_code/web_search` | Web search |
| POST | `/deepai_code/find_service` | Service discovery |
| GET | `/deepai_code/sessions/list` | Session list |
| GET | `/deepai_code/sessions/get?uuid=…` | Session details |
| POST | `/deepai_code/sessions/delete` | Delete session |
| POST | `/deepai_code/sessions/save` | Session / preference upload (added in 0.3.20, cloud sync) |
| POST | `/deepai_code/session_turn` | Session turn reporting (202/204 treated as success) |
| POST | `/deepai_code/review_tool` | Action review for desktop auto mode (added in 0.3.35; 15s timeout, falls back to manual confirmation on failure. The gateway neither implements it nor is inference affected) |
| GET | `/deepai_code/desktop_auth/?state=…&port=…` | Browser login entry point |
| POST | `/deepai_code/desktop_auth/exchange/` | Exchange code for Key |
| GET | `/get_usage_summary` | Subscription and usage (root path, not under `/deepai_code`) |
| POST | `/speech_to_text` | Speech transcription (form field `voiceRecording`) |
| POST | `/chat_attachments/upload` | Attachment upload (requires lane header) |
| GET | `/chat_attachments/get?uuid=…` | Attachment metadata / download URL |
| GET | `/gravity-display-proxy` | Sponsored slot rendering proxy (root path, `User-Agent: DeepAI-Code`) |
| GET | `/gravity-imp` | Sponsored impression reporting (root path) |

### 2.1 Managed Host Control Plane (added in 0.3.36, not implemented by the gateway)

0.3.36 ships a complete set of capabilities in `src/managed/*` and `packages/shared/src/managed/*`
for "registering a machine as a managed host", with the entry point at `/deepai_code/connect/*`.
**It is unrelated to inference, and the gateway deliberately does not implement it**; it is listed here so that the next re-scan does not require re-unpacking.

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/deepai_code/connect/enroll/` | Device enrollment (body carries `label`, `kind`, and public keys `signing_key` / `exchange_key`) |
| POST | `/deepai_code/connect/poll/` | Poll enrollment result (`enrollment_id` + `device_secret`) |
| GET | `/deepai_code/connect/devices/` | List of enrolled devices |
| GET | `/deepai_code/connect/rooms/` | Joinable rooms (uplink field is `incoming`) |
| GET | `/deepai_code/connect/grant/` | Authorization status (uplink field is `grantStatus`) |
| - | `https://deepai.org/deepai_code/connect/approve/` | Manual confirmation page (`user_code` + `verification_uri`) |

Protocol highlights (source-level):

- The only common headers are `Content-Type: application/json` and `X-DeepAI-Protocol: 1`,
 with `redirect: error`, a 30s timeout, and a 256 KiB response body limit;
- Device identity consists of **two asymmetric key pairs**: Ed25519 (signing) + X25519 (exchange).
 The private key PEM is stored locally (file permissions 0600, optionally encrypted via platform secure storage), and the public key is sent to the upstream as base64;
- `enroll` carries no signature; after obtaining `device_secret`, every request carries a device proof:

  ```text
  message = "deepai-code-control-v1" \n deviceId \n generation \n METHOD \n target \n timestamp \n nonce \n sha256(body)hex
  ```

 It is signed with Ed25519 (base64) and placed in `X-DeepAI-Signature`, along with
 `X-DeepAI-Device` (deviceId), `X-DeepAI-Time` (seconds), and `X-DeepAI-Nonce`
 (24-byte base64url); `target` is the path beginning with `/` including the query;
- The `enroll` response is validated strictly: `verification_uri` must be exactly
 `https://deepai.org/deepai_code/connect/approve/`, `expires_in` ∈ [1,600], and `interval` is always 5;
- All errors are normalized into `ControlError{status, code, retryable, retryAfterMs}`:
 408/429/5xx are retryable, 401 → "device authorization expired or revoked", and `Retry-After` is clamped to 300s.

**Reasons for not implementing it**:

1. It serves the desktop app's "remote host / team-shared device" feature, whose ultimate capability is a TLS relay tunnel
 (`managed/{gateway,certificate,tls,host}.ts`), which does not go through the inference endpoints;
2. To make it usable, the gateway would have to keep a pair of device private keys long-term and emulate the desktop app's managed host lifecycle,
 adding a high-value credential and attack surface with zero benefit to the OpenAI / Anthropic compatibility layer;
3. The account pool, lanes, check-in, and inference chain do not depend on it - the upstream has not changed any existing endpoint because of it.

If it ever needs to be replicated in the future, what is needed is not a change to the inference client but a separate module: device identity storage +
signer + enroll/poll state machine (the message assembly rules in this section can be copied directly).

### 2.2 Catalog and Lane Probing: Two Things Share the Same Authenticated Path [Measured 2026-11-14]

The gateway has two "liveness probes that do not consume Theos": fetching the model catalog (`FetchModels`) and probing the lane
(`DetectLane` → `RefreshLane`), both of which hit `GET /deepai_code/models`.
This path **used to be credential-free** (only the lane header + machine fingerprint were checked), but as of 2026-11-14 it requires `Api-Key`:

| Request | Result |
| --- | --- |
| `GET /models`, lane header + machine fingerprint only | 401 `{"code":"auth_required","message":"Sign in to DeepAI to use DeepAI Code."}` |
| `GET /models` + any account's `Api-Key` | 200, 27 models, `default: glm-5.3-flash` |

Therefore:

- Both catalog refresh and lane probing borrow the credentials of an account from the account pool (`Server.probeAccount`:
 preferring an account that is "ready + has a machine fingerprint", then any enabled account with a Key); when the account pool is empty,
 catalog refresh directly returns an actionable error instead of throwing the 401 as "Key invalid";
- Proxy warm-up (`WarmUpstream`) treats **401 as success**: DNS/TCP/TLS and the proxy CONNECT
 are already through, so the purpose of warm-up is achieved; only 5xx / timeouts count as failure;
- Lane probe failure no longer affects correctness: the compile-time `LaneVersion=6` is still the default,
 and a real 426 will also trigger a probe (at which point the account pool necessarily has an account).

Lane thresholds (retested 2026-11-15): `/theos` lane 6 → 200 and returns `lane_version: 6`;
lane 5 also returns 200 (old clients still work); lane 4 → 426 `lane_version_unsupported`.
## 3. Request Headers [Source-level 0.3.37]

| Header | Value | Purpose |
| --- | --- | --- |
| `Api-Key` | Account API Key | **Authentication uses this header, not `Authorization`** |
| `X-DeepAI-Code-Theos` | `6` | Billing lane version (since 0.3.37); thresholds differ per endpoint, see "Lane version thresholds" below |
| `X-DeepAI-Code-Instance` | hex of a 16-byte random number (once per process) | Process instance identifier, sent with every JSON request |
| `X-DeepAI-Code-Machine` | `sha256(lowercase raw machine UUID)` | Issues the daily free quota; `machine_required` when the metering model is missing |
| `X-Session-Affinity` | any session id | Session stickiness (the body's `prompt_cache_key` has the same effect) |
| `X-DeepAI-Code-Check-In` | 32-hex nonce issued by the previous `/theos` | **`/theos` only**: check-in callback |
| `X-DeepAI-Code-Beacon` | `v=<version>;up=<seconds>;pi=<0\|1>` (`;focus=<0\|1>` only when focus is known) | **`/theos` only**: client heartbeat |

Ordinary HTTP headers (easy to overlook, but part of "looking like the desktop client"):

| Header | Measured value on desktop (Node fetch) |
| --- | --- |
| `User-Agent` | `node` (**not** a browser UA) |
| `Accept` / `Accept-Language` | `*/*` / `*` |
| `sec-fetch-mode` | `cors` |
| `sec-ch-ua*` | **not sent** |
| `Content-Type` | `application/json` (**also sent on GETs with no body**; uploads/transcription use multipart) |

> Re-verified item by item on 2026-11-09 with 0.3.20's `server.asar` (`src/deepai/api.ts`):
> the header literal in `requestDeepAIJson` unconditionally includes `Content-Type: application/json`,
> even when `/models`, `/theos`, `/get_usage_summary` are GET; `User-Agent: DeepAI-Code`
> appears **only** on one path, `/gravity-display-proxy`, while `/get_usage_summary` still uses
> undici's default `node`. The heartbeat `focus` is determined by window focus events and omitted
> entirely when unknown (`deepAIBeacon`), so the gateway only needs to send `v=…;up=…;pi=…`.
>
> Browser UA + `sec-ch-ua` are only needed in the **web registration** flow (to stay consistent
> with the web client); carrying them onto inference requests is equivalent to declaring
> "I'm not a client" on an `Api-Key` endpoint. The gateway's `deepai.Client.BrowserFingerprint`
> is exactly this switch: true for registration, false for inference.
>
> Re-verified on 2026-11-11 with 0.3.35's `app.asar` / `server.asar`: `DEEPAI_LANE_HEADERS` is still
> `{"X-DeepAI-Code-Theos":"4","X-DeepAI-Code-Instance":randomBytes(16).hex}`,
> the other 3 headers (`-Machine` / `-Check-In` / `-Beacon`) are unchanged literals;
> `User-Agent: DeepAI-Code` still appears **only** on `/gravity-display-proxy`;
> `/speech_to_text` still sends only `api-key` (no lane / machine headers).
>
> **Re-verified on 2026-11-13 with 0.3.36: `DEEPAI_LANE_HEADERS` changed only one literal -
> `X-DeepAI-Code-Theos: "5"`** (`DEEPAI_LANE_VERSION` is `Number()`'d from it);
> the instance number is still `randomBytes(16).hex` (once per process), the other 3 literals are unchanged,
> and the ordinary headers (`Content-Type` / `User-Agent: node` / `Accept` / `sec-fetch-mode`) are unchanged too.
> The `/models` request body code changed from `requestDeepAIJson("/models", null, …)` to
> including `account.apiKey` (measured 2026-11-13: the catalog is identical with or without the key).
>
> **Re-verified on 2026-11-14 with 0.3.37: `DEEPAI_LANE_HEADERS` again changed only one literal -
> `X-DeepAI-Code-Theos: "6"`**; the other 4 headers, the instance number (`randomBytes(16).hex`,
> once per process), the ordinary headers, and the `/theos` check-in / beacon branches are all unchanged.
> `deepAIBeacon`'s `v=` still comes from `version$2` (0.3.37); the new `catalogAccountId` in 0.3.37
> only reverts the catalog to the built-in snapshot when switching accounts, and does not change the request shape.

Key points:

- Lane version thresholds (**measured matrix 2026-11-13**, one request per cell; old rows kept for comparison):

 | Endpoint | lane 1 | 2 | 3 | 4 | 5 | **6** | 7 |
 | --- | --- | --- | --- | --- | --- | --- | --- |
 | `/models` (0.3.37, same day) | not tested | not tested | not tested | not tested | 200 | **200** | 426 |
 | `/theos` (0.3.37, same day) | not tested | not tested | not tested | 426 | 200 | **200** | not tested |
 | `/chat/completions` (0.3.37, same day) | not tested | not tested | not tested | not tested | 200 | **200** | not tested |
 | `/models` (0.3.36, same day) | 200 | 200 | 200 | 200 | 200 | 426 | 426 |
 | `/theos` (0.3.36, same day) | not tested | not tested | not tested | 200 | **200** | not tested | not tested |
 | `/chat/completions` (0.3.36, same day) | not tested | not tested | not tested | 200 | **200** | not tested | not tested |
 | `/models` (2026-11-11 snapshot) | 200 | 200 | 200 | 200 | 426 | 426 | - |
 | `/theos` (2026-11-11 snapshot) | 200 | 200 | 200 | 200 | not tested | 426 | - |
 | `/chat/completions` (2026-11-11 snapshot) | not tested | **426** | **426** | 200 | not tested | not tested | - |

 → The threshold has shifted right by one notch again: metadata endpoints **1-6**, inference endpoints **both 5 and 6 return 200**,
 7+ returns 426 `lane_version_unsupported` ("This version of DeepAI Code is no longer
 supported"). **The gateway sends `6` to match 0.3.37** (`deepai.LaneVersion`); the probe sequence starts at 6
 and guesses upward, then descends to 2 - **do not start from an old lane** (5 also returns 200, so you'd get pinned to the old catalog).
 Using `/models` as a "lane health check" is still unreliable (it is more lenient toward old lanes).
- `X-DeepAI-Code-Instance` is generated only once at process startup; the gateway can generate one per "process / egress proxy"
 (`clientFor` caches the Client per proxy) - **do not change the value per request**
 (the desktop client doesn't either).
- The machine fingerprint is the **quota allocation key**: when multiple accounts share one fingerprint, only the first gets the daily quota.
 This project therefore assigns each account its own UUID (see README §6.9).
- Raw UUID sources: Windows `Win32_ComputerSystemProduct.UUID` (falls back to `MachineGuid` on failure),
 macOS `IOPlatformUUID`, Linux `/sys/class/dmi/id/product_uuid` or `/etc/machine-id`.
- Firmware placeholder UUIDs (all zeros / all f / `03000200-…`) must be filtered out, otherwise the fingerprint is unstable.

### 3.1 Response Shape and "Stand-in Branch" Criteria [Latest measurement 2026-11-13: UA-based routing is gone, all old criteria invalid]

**Re-tested 2026-11-13 (0.3.36 era, single account, 3 runs)**: same account, same machine fingerprint,
same request body (`glm-5.3-flash`, checked in, non-streaming), only `User-Agent` changed:

| `user-agent` | status | `model` | `id` | `provider` | `service_tier` | `system_fingerprint` | `native_finish_reason` | `cost_details` / `market_cost` | `reasoning_details[0].format` |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `node` | 200 | `zai/glm-5.3-flash` | `gen-639e7bf2…` (32 hex) | none | none | `fp_3qkh4qwihw` | none | none | `stealth-v1` |
| `curl/8.9.1` | 200 | `zai/glm-5.3-flash` | `gen-f3b1884e…` | none | none | `fp_28knbcfnc3` | none | none | `stealth-v1` |
| Chrome 156 | 200 | `zai/glm-5.3-flash` | `gen-aa723a81…` | none | none | `fp_fu6cxxk30b` | none | none | `stealth-v1` |

Conclusion (three samples + one forced tool call via `tool_choice`):

1. **UA no longer routes**: the three UAs produce responses of the same shape, the same model echo, and latency in the same order of magnitude; the
 "Chrome UA gets you a swapped model" phenomenon is gone. The gateway still sends `user-agent: node` (harmless and closer to
 the desktop client), but don't treat "wrong UA" as a cause of pool failure anymore.
2. **`provider` / `service_tier` / `native_finish_reason` / `cost_details` /
 `market_cost` are now all absent**; `id` is uniformly `gen-<32 hex>`, `model` uniformly echoes
 `zai/<requested id>` (note the slug prefix changed from `z-ai/` to `zai/`).
 → The three old criteria "missing `provider` = stand-in", "`gen_` prefix = stand-in", "cost=0 = stand-in"
 are **all invalid** (real requests look like this now too).
3. `reasoning_details` shape: for plain text replies it's `{type:"reasoning.encrypted",
 data:"gwst1....", format:"stealth-v1"}`; for forced tool calls it's
 `{type:"reasoning.text", text:"…", format:"unknown"}`. The tool call itself is still
 standard OpenAI shape (`tool_calls` + `finish_reason: tool_calls`).
4. So **there is currently no reliable "response shape" criterion to distinguish a real model from a stand-in**. Whether an account
 is aligned can only be judged by admission conditions: whether check-in is complete, whether it's rejected with 426/403, and whether the model is
 obviously answering off-topic (see the fingerprint method in `docs/model-fingerprint-2026-11-10.md`).

> The 2026-11-10 record below is kept for history: it explains why the old criteria looked the way they did,
> and shows that every time upstream changes its routing implementation, the criteria must be re-tested wholesale.

**Re-tested 2026-11-10 (live behavior in the 0.3.32 era, single account, 3 runs)**: routing was still by UA,
but the stand-in branch **no longer sent `provider`**, and `model` no longer echoed the requested slug.
Same account, same machine fingerprint, same request body (`glm-5.3-flash`, checked in), only UA changed:

| `user-agent` | status | latency | response fingerprint | branch |
| --- | --- | --- | --- | --- |
| `node` | 200 | 5.5s | `id: gen-<epoch>-<rand>`, `model: z-ai/glm-5.3-flash`, `provider: Wafer` | **real model** |
| `Go-http-client/1.1` | 200 | 6.6s | `id: gen_<ULID>`, `model: reflection/beam`, no `provider` | stand-in |
| `Mozilla/…Chrome/156…` | 200 | 8.5s | same as `Go-http-client` | stand-in |

Full response shapes of the two branches (captured in the same session, can be diffed directly):

| Field | Real branch (`node`) | Stand-in branch |
| --- | --- | --- |
| `id` | `gen-1794289503-lKqhAJp…` (hyphen + epoch) | `gen_01M71ZSSD8EMJ6H…` (underscore + ULID) |
| `model` | echoes the requested slug: `z-ai/glm-5.3-flash` | **`reflection/beam`** (swapped slug, ignores the request) |
| `provider` | `Wafer` / `InferenceNet` / `Reka` /... | **field missing** |
| `service_tier` | `null` (field present) | **field missing** |
| `system_fingerprint` | `null` | **`fp_i144zcu3ag`** |
| `native_finish_reason` | present (`stop` / `max_output_tokens`) | **absent** |
| `reasoning_details[0].format` | real formats such as `reasoning.text` | **`unknown`** |
| `usage.cost_details.upstream_inference_cost` | >0 (measured `3.29e-05`) | `null` (both prompt / completion cost `0`) |
| `usage.market_cost` | field missing | **`0`** |
| top-level `generationId` | field missing | **present** (same value as `id`) |

Real / stand-in criteria (ordered by reliability, **2026-11-10 historical version, invalidated on 11-13**):

1. **`provider` field missing → stand-in**; has a value (`Wafer` / `InferenceNet` / `Reka` /
 `BaseTen` / `CoreWeave` / `Makora` / `Modal` / `Morph` / `Inceptron` /
 `OpenInference`...) → real model. **There is no longer a `provider == "DeepAI"` value**.
2. **`model` not echoing the requested slug → stand-in** (the real branch returns `z-ai/<requested id>`, the stand-in returns
 `reflection/beam`). This is the easiest to judge by eye and the most resistant to upstream tweaks.
3. `usage.cost_details.upstream_inference_cost`: >0 = real, `null`/`0` = stand-in
 (stealth routing isn't billed).
4. `id` prefix: `gen-` = real, `gen_` = stand-in; `usage.market_cost` present = stand-in.
5. `reasoning_details[0].format`: `"openrouter-stealth-v1"` is the **old** marker,
 now it's `"unknown"` - can only be used as weak evidence (some real-branch backends also give `unknown`).
6. `system_fingerprint` **cannot be used as a criterion at all**: in the real branch only vLLM-family backends provide it,
 while the stand-in now always gives `fp_…`; `service_tier` is likewise no longer usable (the stand-in simply omits it).

Other points (**2026-11-10 historical record, no longer applicable to live behavior after 11-13**):

- The deciding switch at the time was **UA**, unrelated to `Accept` / `Accept-Language` / `sec-fetch-mode`
 (measured at the time: `node` + `application/json` still hit the real branch);
- The stand-in branch was noticeably slower (single sample 5.5s vs 6.6s / 8.5s), but the sample size is too small - treat as reference only;
- The stand-in branch was more easily truncated by `finish_reason=length` at the time (reasoning consumes tokens but doesn't count toward content);
- The inference at the time was "inference must send `user-agent: node`"; **re-tested 2026-11-13: the three UAs show no difference**,
 the gateway continuing to send `node` is just to stay consistent with the desktop client, no longer a "must".

**Gateway link measurement (2026-11-09, old criteria)**: started the gateway locally (14 accounts,
each with its own machine fingerprint), hit `glm-5.3-flash` non-streaming 12 times: `200 × 12`,
`provider` rotated among `Wafer` / `InferenceNet` / `Reka` / `BaseTen` / `CoreWeave` /
`Makora` / `Modal` / `Morph`, each call 1.5-4.7s; in the logs all 14 accounts were hit once each
(fair round-robin). Now only the "fair round-robin" half of that conclusion still holds.
## 4. Error Semantics [Measured 2026-11-07 ~ 11-08]

| Status | body | Meaning | Gateway handling |
| --- | --- | --- | --- |
| 409 | `{"error":{"code":"session_busy"}}` + `Retry-After` | Capacity rejection | Wait per `Retry-After`, then **switch account and retry** |
| 401 | - | Authentication failure | Record failure, switch account |
| 403 + `email_unverified` | - | Email not verified | Treat as authentication failure |
| 403 + `social_login_required` | - | Account can only sign in with Google/Apple (interim 0.3.19 policy, **lifted on 11-09**, see §4.1) | **Terminal policy state**: mark as blocked; do not hit it again after switching accounts |
| 403 + `check_in_refused` | `DeepAI Code has restarted its check-in too many times today. Try again tomorrow.` | **Daily check-in count exhausted** | Do not change account state (inference still works, but falls into the fallback branch); do not restart check-in again today |
| 403 + `network_bound` | `Today's free Theos for your network have all been claimed. More become available at <RFC3339>` | **Network-level** free quota fully claimed | Wait until the time in the message as `refresh_at`; switching accounts usually doesn't help (same egress IP) |
| 403 + `theos_exhausted` | `You've used today's Theos. They refresh at <RFC3339>.` (body includes `refresh_at`) | **Account-level** quota exhausted (paid model quota used up / negative balance; measured on 2026-11-13 hitting `minimax-m3` with `balance=-7.58`) | That account cannot run metered models for the rest of the day; switch accounts or wait for `refresh_at`; free models are unaffected |
| 400 + `machine_required` | - | Metered model missing machine fingerprint | Do not retry; prompt to configure `DEEPAI_MACHINE_ID` |
| 426 + `lane_version_unsupported` | - | Lane version mismatch | Do not retry; prompt to upgrade the gateway |
| 400 + `invalid_request_error` | message = `This request is not one DeepAI Code sends.` (old) or `This request cannot be accepted.` (new as of 2026-11-13) | Request body is not in desktop-client shape | Retry once on the same account using the desktop field whitelist + adding system (both messages handled the same way, see §5.2) |
| 400 / 404 / 422 | - | Problem with the request itself | Do not retry |

`Retry-After` is clamped to a 30s upper bound (`deepai.RetryAfter`).

> `social_login_required` is an account policy newly introduced in 0.3.19: the upstream
> explicitly requires accounts to sign in with Google/Apple. In testing, accounts registered
> with email + password hit it on both `/theos` and the inference endpoints,
> and blocked accounts even get a 403 on `/theos` (the 403 body still includes `check_in`).
>
> `network_bound` is the **network-level quota limit** observed in 0.3.19 testing: the daily
> free Theos are not "one per machine, independent of each other"; rather, the same egress IP
> shares a single daily pool. Once the pool is claimed, accounts that didn't get a quota
> can still pass `/theos` (`balance=granted=0`), but calling metered models returns
> 403 `network_bound`; free models (`theosPerRequest=0`) are unaffected.
> The time in the message is the next distribution time and can be used directly with `Retry-After` semantics.
> In testing, of the same batch of 14 accounts (same direct-connect IP), only 5 got 200 Theos, and the other 8 waited until
> `2026-11-07T17:14Z` for a refresh. After binding the account pool to the registration proxy pool by account (12 egresses),
> the number of accounts getting 200 Theos rose to 10 - egress diversification can bypass the single-IP network-level limit.
## 4.1 `social_login_required` Lifted [Verified 2026-11-09]

The "all email/password accounts return 403" behavior in 0.3.19 was a **temporary policy**; retesting on 11-09 shows it has been relaxed: the same batch of accounts has recovered on both `/theos` and the inference endpoint (hitting upstream directly, with a temporary machine fingerprint per account):

| Account (email/password registration) | Source | `/theos` | chat |
| --- | --- | --- | --- |
| `pwvjkhiq1hwz@lmoir.com` | 11-09 HAR | 200, 200 Theos | 200 `"42"` |
| `zupxzenogljb@lmoir.com` | 11-09 HAR | 200, 200 Theos | 200 `"42"` |
| `bharatarelli203@gmail.com` | 11-07 HAR | 200, 50 Theos | 200 `"ok"` |
| `lizi451203@gmail.com` | 11-07 HAR | 200, 50 Theos | - |
| `zhuhaoyi181@gmail.com` | 11-07 desktop client | 200, 200 Theos | - |
| `yvbeghihag6wvjd@lmoir.com` | 11-07 HAR | 200, 50 Theos | - |
| `8gzfvzkw2s@privaterelay.appleid.com` | 11-07 HAR | 200, 200 Theos | - |

Clarifications:

- The error code `social_login_required` **is still valid**, and the gateway's `blocked` terminal-state handling is unchanged
 (if upstream tightens restrictions again, no code changes are needed);
- "Email-registered account = unusable" **must not** be written as a pre-pool-entry criterion again. The
 `LoginType.SocialOnly()` precheck in the registration flow is a separate matter: it blocks cases where "the email genuinely has no password session"
 (i.e., the account can only sign in via a social account), and is unrelated to upstream's inference restrictions on email accounts;
- Conclusion: the email registration flow is usable again, and Google sign-in is no longer the only option.
## 4.2 Google/Apple Sign-In: Callback Boundaries [Tested 2026-11-09]

DeepAI has two social sign-in implementations, **both of which must be completed in the browser** - the server cannot do it on your behalf:

1. **allauth redirect**: `GET /accounts/google/login/?process=login`
 - 302 to `accounts.google.com/o/oauth2/v2/auth`;
 - `client_id=465558899080-…apps.googleusercontent.com` (public);
 - `redirect_uri=https://deepai.org/accounts/google/login/callback/`
 (registered as fixed on Google's side; the code is only ever sent to deepai.org);
 - `state` is bound to deepai.org's `sessionid` cookie (issued together with the 302 response);
 - `code → token` requires **DeepAI's client_secret**, which we cannot obtain.
2. **GIS frontend button**: the desktop login page loads `accounts.google.com/gsi/client`,
 and the JS carries the same `google_client_id`;
 - the browser obtains the `id_token` directly, and `aud` must be DeepAI's client_id;
 - Google only issues it on the authorized origin (deepai.org), so it cannot be forged or signed on someone else's behalf.

The only feasible and already-implemented approach is: after sign-in completes in the browser, DeepAI redirects back to the local machine at
`http://127.0.0.1:<port>/auth-callback?code=…&state=…`, and then the gateway
`POST /deepai_code/desktop_auth/exchange/` exchanges it for an API Key (`internal/server/login.go`,
the console's "Browser Sign-In"). A Google account can also be signed in on the desktop first, then imported via `action=import` to bring in the credentials.
## 5. Known Pitfalls [Tested 2026-11-07 ~ 11-08]

- **Do not follow upstream 302s**: An invalid Key redirects to the login page; following it disguises an authentication failure as a 200.
 `deepai.Client`'s `CheckRedirect` returns `ErrUseLastResponse`; this is intentional.
- The upstream places the chain of thought in `reasoning`; the OpenAI ecosystem commonly uses `reasoning_content`,
 so the gateway adds a copy (it does not overwrite if one already exists).
- The default model field in the `models` response is `default` (not `default_model`).
- The `balance` / `granted` returned by `/theos` are **strings**, not numbers.
- `/theos` also returns `lane_version` (a number) and `check_in:{nonce,seconds}`;
 the client uses "whether `theosPerRequest` exists in the catalog" to determine whether the catalog belongs to the current lane.
- Each item in the `models` catalog now includes `theosPerRequest` (the Theos unit price as a string),
 `theosPerSecond` tiers are limited to `none/low/medium/high`; `gpt-oss-120b` does not exist
 (the client actively filters it out).
- In SSE, the same event may have multiple `data:` lines; per the spec, concatenate them with `\n` before parsing.
## 5.1 Check-in [Source-level 0.3.37 + empirical testing]

- `/theos` is the check-in channel: both success (200) and rejection (403) carry
 `check_in:{nonce,seconds=600}`.
- The desktop client caches the nonce; the next `/theos` call carries `X-DeepAI-Code-Check-In: <nonce>`
 along with the heartbeat `X-DeepAI-Code-Beacon: v=<version>;up=<seconds>;pi=<0\|1>` (`focus` is optional
 and only appears when the window focus state is known).
- Since 0.3.30, `pi` is also **optional**: when `piActive === null` (this process has not yet forwarded
 any inference), the whole segment is omitted, so a two-segment heartbeat like `v=0.3.37;up=12` is valid.
 The gateway always sends `pi=<0|1>`, which is equivalent to the desktop client's "has forwarded" state; upstream does not care about the format.
- Re-check-in is triggered at `seconds/2` (300s by default); check-in is ensured before inference (failure does not block).
- The inference request itself **does not carry** check-in / beacon; it only carries the lane, instance, machine fingerprint, and `Api-Key`.

### Check-in is the admission condition for the "real model branch" [Empirical 2026-11-08, single-sample re-verification 2026-11-10]

> ⚠ The table below is the raw record from the **2026-11-08** batch of measurements, where "aligned" was judged by the response
> characteristics **at that time** (`system_fingerprint` present / `provider` absent). The criteria have been rewritten twice in §3.1,
> and neither of those characteristics holds today; the **sample sizes and direction of the conclusions** in the table are still valid, but treat the field descriptions as historical.
> To re-run the health check, use today's criteria from §3.1 (`provider` missing / `model` echoes the slug / `id` prefix).

Read this together with §3.1 (UA routing): the conclusion of the 2026-11-08 health check round was that **both the `node` UA and a completed check-in**
must be satisfied to get the real model (table below). But a single sample on 2026-11-11 showed that when check-in is refused, it can still be the
real branch (see the next subsection); therefore treat this as the "main path", not a necessary and sufficient condition.

### Check-in refused ≠ account is broken [Empirical 2026-11-11, single sample]

Repeatedly hitting `/theos` within a short period (about 6 consecutive calls during the 2026-11-11 probe) triggers the daily check-in rate limit:

```text
GET /deepai_code/theos lane=4 → 403
{"error":{"message":"DeepAI Code has restarted its check-in too many times today. Try again tomorrow.",
          "type":"check_in_refused","code":"check_in_refused"}}
```

- Recovery time is `refresh_at` (measured locally as resetting at `00:04Z` the next day); no need to swap accounts or log in again.
- **After a refused check-in, inference still goes through the real branch**: the same account shows `provider=InferenceNet`,
 `usage.cost_details.upstream_inference_completions_cost>0`, `id: gen-<epoch>-<rand>`.
 That is: `check_in_refused` only blocks "checking in again today"; it does not mean the account is invalid, nor does it necessarily drop to the substitute
 (the 08-version table above measured "no check-in at all", which does not contradict this, but the two cannot substitute for each other).
- Therefore, on the account-pool side, only `CheckInRefusedLock` is needed to stay silent until tomorrow (already implemented);
 **health-check/probe scripts must be rate-limited** (≥5 minutes per call recommended), otherwise a single full health check will burn through
 a batch of accounts' daily check-in quota.

| Observation | Result (14 account-pool accounts, free model `glm-5.3-flash`) |
| --- | --- |
| Only fetch `/theos` (do not send back the nonce) | **0/14 aligned**, all 14 fall to the substitute backend |
| Fetch nonce + send back (full check-in) | **14/14 aligned** |
| Full check-in but all 14 accounts **share the same new machine fingerprint** | 10-12/14 aligned (the rest fall to the substitute branch) |
| Via the gateway (each account's own machine fingerprint + check-in before inference) | **30 requests / 14 accounts, 30/30 aligned** |

- Judgment is remembered by **(account, machine fingerprint)**: changing the machine fingerprint is equivalent to a cold start, and check-in must be redone;
- The substitute branch is slower (3-9s vs 1-2s after alignment); **note that since 2026-11-10 the substitute writes
 `model` as `reflection/beam` (it no longer echoes `z-ai/glm-5.3-flash`)**,
 which is more useful than speed (see §3.1);
- The gateway's `EnsureCheckIn`: when a nonce has been obtained but **not yet sent back**, it immediately makes another `/theos` call
 (`ackedNonce` remembers the one already sent back); only after that does it fall back to the `seconds/2` renewal cadence.
- Cold start still has a **one-time** window: when an account has no challenge yet, the first inference only has time to fetch the nonce,
 so this one falls to the substitute branch, and alignment starts from the second time (in the measured 12 requests, only the 1st was the substitute).
- **Check-in is a limited resource**: `403 check_in_refused` means the day's "restart check-in" count is exhausted,
 and you must wait until tomorrow. So do not use scripts to repeatedly run "fetch nonce + send back"; the gateway's cadence
 (one probe at startup + renewal every `seconds/2`) is safe. Rate-limited accounts can still run inference,
 they just continue to fall to the substitute branch.
## 5.2 Request Body Shape [Measured 2026-11-07, copy re-verified 2026-11-13]

Upstream rejects request bodies that "DeepAI Code would not send" with `400 invalid_request_error`:

- Every chat request from the desktop client carries a **system message** and **tools**;
- A bare request (no system) is accepted only when **this process has already checked in** (`/theos` has returned `X-DeepAI-Code-Check-In`); otherwise it returns 400. **As measured on 2026-11-13, the same gate now has two different messages**:

  ```json
  {"error":{"message":"This request is not one DeepAI Code sends.","code":"invalid_request_error"}}
  {"error":{"message":"This request cannot be accepted.","type":"invalid_request_error","code":"invalid_request_error"}}
  ```

 The new message has narrower trigger conditions (same account, `lane=5`, same body, matrix):

 | UA | Machine fingerprint | body missing system | body with system |
 | --- | --- | --- | --- |
 | `node` | This machine's real fingerprint (the one used by HAR / the desktop client) | **400 cannot be accepted** | 200 |
 | `node` | Random new fingerprint | 200 | 200 |
 | `curl/8.x` | This machine's real fingerprint | 200 | 200 |

 → The old message means "bare requests are rejected unless you check in"; the new message appears to mean "when the client machine is already bound and the UA is node, the desktop shape is enforced." **The gateway is deployed on the same machine as the desktop client by default, so the machine fingerprint is this machine's real fingerprint, which makes this new message the normal path**: a bare request (no system) gets 400 the first time, and 200 after adding system (measured on a local temporary instance, 2026-11-13). Either way, the gateway should treat it as a "request body shape rejection": `deepai.HTTPError.BodyRejected` **matches both messages** (the old implementation recognized only the first one and would pass the 400 straight through to the caller). Adding a system message also passes validation, but that changes the sampling conditions - fingerprint-style detection requires "able to send + conditions unchanged", so check in first;
- Every reasoning model carries `reasoning_effort`: `deepAIPiModels` sets `thinkingLevelMap:{off:"none"}` for it, so "thinking off" sends `"none"`, and a selected level sends that level's value; the translation layer (Anthropic's `thinking`/`output_config.effort`, Responses' `reasoning.effort`) must map it to `reasoning_effort`; dropping it silently degrades to the upstream default;
- Fields outside the whitelist (`user` / `n` / `presence_penalty`, etc.) may be rejected on strict nodes;
- Gateway fallback strategy: still pass through as-is first; only when that 400 is hit, reorder once according to the `model/messages/tools/tool_choice/max_tokens/temperature/stream_options/seed/…` whitelist and fill in the system message (`deepai.ChatBodyFallback`, keeping an existing system as-is). Starting 2026-11-15, one more layer: when the message is replaced by a new sentence that no one recognizes, treat it as a shape rejection as long as it is `400 + code=invalid_request_error` (if it still fails after fixing the shape, return the second error);
- **All six levels of `reasoning_effort` are accepted**: on 2026-11-15, `deepai-cyber` / `muse-ember` / `glm-5.3-flash` were each tried with `low/high/xhigh/max`, all returning 200. The `thinkingLevelMap` in 0.3.37 exposes `max` / `xhigh` only for the first two models (desktop dropdown); this is not a server-side restriction.
## 5.3 Fingerprinting and Tool Calls: Hard Constraints on the Gateway Side [Empirical + Source-Level 0.3.37]

- **Linux machine fingerprints only recognize systemd machine-id**: On Linux, `detect()` in 0.3.37 only reads
 `/etc/machine-id` → `/var/lib/dbus/machine-id`, and requires **32 lowercase hex characters and not all zeros**;
 `/sys/class/dmi/id/product_uuid` is no longer in the client logic (it cannot be read without root, and firmware placeholder values can also
 produce unstable fingerprints). The gateway previously read product_uuid first; placeholder values would cause the fingerprint to diverge from the desktop client, and this has been aligned.
- **`tool_result` must be sent before the text in the same user message**: OpenAI requires `role=tool`
 to immediately follow the assistant with `tool_calls`; Anthropic clients often put `tool_result` and subsequent explanations
 in the same user message. If text is sent first in the original order, the upstream will reject it or treat the tool result as a new user turn.
- **Parameter fragments for parallel tool calls may be interleaved**: OpenAI streaming `tool_calls[].index` distinguishes calls,
 and fragments are not guaranteed to arrive in index order; once an Anthropic `content_block` is closed, nothing can be appended.
 The gateway buffers parameters by index and sends `input_json_delta` in order all at once at the end (or when switching blocks).
- **Parallel `function_call`s in Responses must be merged**: If multiple consecutive `function_call` items
 each become a separate assistant message, `function_call_output` will no longer immediately follow the corresponding
 `tool_calls`; the gateway merges them into multiple `tool_calls` in a single assistant message.
- **The final `response.output` in Responses streaming must be sorted by `output_index`**: Tool items are stored in
 a map, and iterating directly makes the output order of parallel tools random.
## 6. Reproduction Steps

```bash
# Read desktop credentials and probe (without starting the gateway)
go run ./cmd/probe -usage

# Run a single conversation using the default model in the directory
go run ./cmd/probe -prompt "reply OK" -stream

# Query quota directly
go run ./cmd/probe -usage
```
