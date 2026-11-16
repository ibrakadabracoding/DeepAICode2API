<div align="center">

<img src="docs/assets/logo.png" alt="DeepAICode2API" width="128" height="128">

# DeepAICode2API

Reverse-engineers the upstream model service of the local **DeepAI Code desktop app** into a
**Go gateway compatible with both the OpenAI and Anthropic protocols**.

It reuses the API key written by the desktop app after login, so no separate key application is needed; implemented purely on the Go standard library,
a single ~10 MB binary with zero external dependencies.

[![ci](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml/badge.svg)](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml) [![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev) [![deps](https://img.shields.io/badge/deps-0%20(stdlib)-00ADD8)](go.mod) [![tests](https://img.shields.io/badge/tests-616%20cases-brightgreen)](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml)
[![protocol](https://img.shields.io/badge/protocol-OpenAI%20%7C%20Anthropic-6b4fbb)](#3-api-reference) [![docker](https://img.shields.io/badge/docker-ready-2496ED?logo=docker&logoColor=white)](docker-compose.yml) [![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Ask DeepWiki](https://img.shields.io/badge/Ask%20DeepWiki-blue.svg?labelColor=000000&logo=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAACXBIWXMAAAPoAAAD6AG1e1JrAAACIUlEQVRYw+2XP2gUQRTGv7d3JhYWQawkIBaCVWzETos0gmAnVoKNTSoLK0E7EQQrC20sBVFBtLPQRgTBtBaKjYgIQUSxiJq7fT+LvCGPJZdNLt4dQj5Y3u7sznzfzLw/s9IO/icANiniCujEfWdsQgBLxAbsbraPZcmBfcAt4DNwKQsZNfEUsAB8YRV12LfA6ZHuedg7AO7ed/c+UBcbQhbiu+6wXIM6EnZWkszMJe2Ke0n6I2la0v51hFeSMLN6OwIK6jJwEBdUYb2MAxRSz6sY4geiahFgLe1Hwq6YWe3us8AN4FQQU4QM64SPY69Xyr43fADgNjAPXHb3peSsD4FDQ0VLEjAHvIhBPS6AHvA1bBO9JPAXcHFbIRtJ5yzwIQZ9CZwAusDJENIkLqsG8DpNyIZJwSUk9wLHgakcesC1JCCjiHm1kYDNOEjptCxpSVK/8f73OFLxPLAYM3oCzEX7MXf/lJbc/8kWpA4HgQdpYI9IWAbeh83whi84cHXL+58EPBoQhnmm94EzwE3gZ2p/Dhzdbhg+TaRr01x7ftb4/jBwFzjXrCvDpmLW9crVLNeR9CaapoGemb2TdCERW1tNaBNQ8jktwmozqwtpqRNtdWAjARYk39IS12ZWRX7vmJmA71nQZgi36gN7gCvAj5xc3P0jcH7kx7Ik5IC73wsh14GZsZySow5002l4Jr0b6+m4eSyvpAn8lEzsx2QHo8RfUrlN+uPq4ksAAAAASUVORK5CYII=)](https://deepwiki.com/2951461586/DeepAICode2API)

</div>

> ℹ️ Version 0.3.19 once narrowed DeepAI Code inference to accounts signed in with **Google/Apple** (email/password accounts got
> `403 social_login_required`). **Retesting on 2026-11-09 showed the restriction has been lifted**: email-registered accounts
> again return 200 on both `/theos` and the inference endpoints (see `docs/recon.md` §4.1). The gateway still keeps the `blocked`
> terminal-state handling, so no code changes are needed if the policy tightens again.

| Capability | Description |
| --- | --- |
| Dual protocol | OpenAI `/v1/chat/completions`, `/v1/responses`; Anthropic `/v1/messages` |
| Protocol alignment | Lane 6 + `X-DeepAI-Code-Instance` + `/theos` check-in; distinguishes `social_login_required` / `machine_required` / `lane_version_unsupported` |
| Account pool governance | Grouped round-robin, failover to another account on failure, capacity backoff, per-account quota scheduling, automatic skipping of policy-blocked accounts |
| Account onboarding | Browser login (Google / Apple / email + password), desktop credential import, manual API key; machine fingerprint and proxy assigned automatically on pool entry |
| Console | Single-page embedded UI: accounts / keys / logs / stats / settings |
| Security | Key authentication, admin passphrase, same-origin write protection, optional AES-256-GCM encryption of state files |
| Regression coverage | 616 test cases (468 top-level + 148 subtests) / 14.7k lines of tests; `go test -race` + gofmt / vet / build / vulncheck all green; CI covers 10 packages with tests (plus 5 without) + frontend syntax and smoke checks (23) |

```text
┌──────────────┐  /v1/chat/completions   ┌────────────────┐   Api-Key + Theos headers   ┌─────────────────────────────┐
│ OpenAI SDK   │  /v1/responses          │ DeepAICode2API │  X-DeepAI-Code-Machine │ https://api.deepai.org      │
│ Claude Code  │  /v1/messages           │   (this gateway)    │ ─────────────────────▶ │ /deepai_code/chat/completions│
│ pi / Cline   │  /v1/models             │                │  ◀───────────────────── │ /deepai_code/responses      │
└──────────────┘  SSE streaming / non-streaming       └───────┬────────┘      OpenAI-compatible SSE    └─────────────────────────────┘
                                                  │ auto-read
                                                  ▼
                          ~/.deepai-code/userdata/secrets/deepai-account.bin
                          {"apiKey":"…","username":"…","isPro":false}
```

---
## 1. Reverse-Engineering Findings

DeepAI Code uses an **Electron + local t3code server** architecture (app version **0.3.37**, lane 6;
item-by-item wire protocol comparison across 0.3.20 → 0.3.30 → 0.3.32 → 0.3.35 → 0.3.36 → 0.3.37:
**0.3.37 only bumped the lane (5 → 6); all other request headers / fingerprints / check-in / endpoints are unchanged**).
The component that does the real work is not the desktop shell but the t3code server inside `resources/server.asar`;
it wraps upstream capabilities into a local loopback HTTP service, which is then called by Pi (the coding agent).

### 1.1 Key Facts

| Stage | Reverse-Engineering Result |
| --- | --- |
| App form | Electron shell (`app.asar`) + standalone server (`server.asar`, ~200 MB) |
| Data directory | `T3CODE_HOME` or `~/.deepai-code`; state directory is `<base>/userdata` |
| **Credential storage** | `<stateDir>/secrets/deepai-account.bin`, **plaintext JSON** (file mode 0600) |
| Credential contents | `{apiKey, username, isPro}`; `apiKey` is in UUID format |
| **Inference entry** | `POST https://api.deepai.org/deepai_code/chat/completions` |
| Responses entry | `POST https://api.deepai.org/deepai_code/responses` (only for `api=openai-responses` models) |
| Model catalog | `GET https://api.deepai.org/deepai_code/models` |
| Quota | `GET https://api.deepai.org/deepai_code/theos` |
| Usage | `GET https://api.deepai.org/get_usage_summary` |
| Speech-to-text | `POST https://api.deepai.org/speech_to_text` (field `voiceRecording`) |
| Web search | `POST https://api.deepai.org/deepai_code/web_search` |
| **Authentication** | Request header `Api-Key: <apiKey>` (not `Authorization`) |
| Protocol header | `X-DeepAI-Code-Theos: 6` (billing lane version; 6 since 0.3.37; inference endpoints accept both 5 / 6, `/models` is relaxed to 1-6, see `docs/recon.md` §3) |
| Process instance | `X-DeepAI-Code-Instance: <32-hex>` (randomized once per egress proxy and sent with all JSON requests through that proxy; on desktop it is once per process) |
| Check-in | The `/theos` response carries `check_in:{nonce, seconds}`; the next `/theos` call sends back `X-DeepAI-Code-Check-In` and `X-DeepAI-Code-Beacon: v=…;up=…;pi=0\|1` (`focus` is appended only when the focus state is known) |
| **Machine fingerprint** | `X-DeepAI-Code-Machine: sha256(raw machine UUID)`, used to issue free Theos per day; returns `machine_required` when the metering model is missing |
| Session stickiness | `X-Session-Affinity` (the `prompt_cache_key` in the body is also used by upstream) |
| Capacity rejection | `409 {"error":{"code":"session_busy"}}` + `Retry-After` |
| Account policy | `403 social_login_required`: a temporary restriction in 0.3.19, lifted on re-test 2026-11-09; the gateway still treats it as a terminal `blocked` state (see `docs/recon.md` §4.1) |
| Browser login | `GET /deepai_code/desktop_auth/?state=…&port=…` → local `/auth-callback?code=…` → `POST /deepai_code/desktop_auth/exchange/` |

### 1.2 Upstream Is Itself OpenAI-Compatible

The `chat/completions` request / response is fully consistent with OpenAI, and the response additionally carries
OpenRouter-style fields such as `provider`, `reasoning`, `reasoning_details`, and `cost`
(`id: gen-…`, `usage.cost_details.upstream_inference_*`, `native_finish_reason`).
Note that `provider` is the **real inference backend name** (`InferenceNet` / `Wafer` / `Reka` /...),
and **the substitute branch now omits the `provider` field** (the old `provider: "DeepAI"` is gone).
To distinguish real from substitute, check simultaneously whether `provider` is missing, whether `model` echoes the requested `z-ai/<id>`,
and whether the `id` prefix is `gen-` or `gen_` (see `docs/recon.md` §3.1 for the full criteria table).
Therefore this gateway does not need complex protocol restoration; the focus is on:

1. **Credential import**: read from the desktop plaintext credential file and deduplicate;
2. **Account pool**: multi-key rotation, failover to another account, wait-and-retry on `session_busy`;
3. **Model governance**: dynamic catalog caching, aliases, fallback models;
4. **Protocol adaptation**: translation for Anthropic clients and Responses clients;
5. **Observability**: request logs, token / cost statistics, quota probing, console.

### 1.3 How the Machine Fingerprint Is Derived

Desktop logic (`src/deepai/machineId.ts`):

- Windows: `(Get-CimInstance Win32_ComputerSystemProduct).UUID`; when the firmware UUID is a placeholder, fall back
 to the registry `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`;
- macOS: `IOPlatformUUID` from `ioreg`;
- Linux: `/sys/class/dmi/id/product_uuid`, then `/etc/machine-id`;
- Finally take `sha256(lowercased raw value)`; the raw value is never uploaded.

This gateway replicates the same logic. In containers / environments without SMBIOS, you can explicitly override it with
`DEEPAI_MACHINE_ID=<raw UUID>`.

---
## 2. Supported Models

The model catalog is **entirely delivered by the upstream**: the gateway only caches and aliases it, so adding or retiring models upstream requires no code changes.
The table below is the **live catalog** exported from the online `/v1/models` on 2026-11-14 (27 models: 11 free / 16 paid).
`Theos/request` comes from the upstream `theosPerRequest` (a string); `0` means free. The Console's "Model Catalog" page reads the same data.
(The `crouton-alpha` in the previous snapshot has been retired upstream; `gpt-oss-120b` is actively filtered by clients, and the gateway does not display it either.
 Compared with the 2026-11-13 snapshot, this round adds 6 and retires none: `muse-ember` (free), `kimi-k3.1-pro`,
 `gpt-6.1-terra`, `gemini-4-hydrogen`, `gemini-4-carbon`, `claude-sonnet-5-5`.)

| ID | Name | Context | Max Output | Theos/request | Notes |
| --- | --- | ---: | ---: | ---: | --- |
| `deepai-cyber` | DeepAI Cyber | 320,000 | 131,072 | **0** | Free, text-only |
| `deepseek-v4.1-flash` | DeepSeek V4.1 Flash 1017 | 320,000 | 131,072 | **0** | Free (the upstream dropdown is `DeepSeek V4.1 Flash 1017`; the desktop app rewrites the old name to it) |
| `glm-5.3-flash` | GLM 5.3 Flash | 320,000 | 131,072 | **0** | Free, default model |
| `glyph-cluster` | Glyph Cluster | 320,000 | 131,072 | **0** | Free, text-only |
| `gpt-6-luna` | GPT-6 Luna | 320,000 | 128,000 | **0** | Free |
| `mimo-2.6-flash` | MiMo 2.6 Flash | 320,000 | 131,072 | **0** | Free |
| `mimo-2.6-pro` | MiMo 2.6 Pro | 320,000 | 131,072 | **0** | Free |
| `minimax-m3.1-flash` | MiniMax M3.1 Flash | 320,000 | 131,072 | **0** | Free |
| `muse-ember` | Muse Ember | 320,000 | 131,072 | **0** | Free (added 2026-11-14) |
| `qwen-3.8-flash` | Qwen 3.8 Flash | 320,000 | 128,000 | **0** | Free |
| `solar-mini-4` | Solar Mini 4 | 320,000 | 131,072 | **0** | Free, text-only |
| `minimax-m3` | MiniMax M3 | 320,000 | 131,000 | 2 | Paid, text-only |
| `glm-5.3` | GLM 5.3 | 320,000 | 131,072 | 3 | Paid, text-only |
| `kimi-k3` | Kimi K3 | 320,000 | 131,072 | 3 | Paid |
| `kimi-k3.1-pro` | Kimi K3.1 Pro | 320,000 | 131,072 | 3 | Paid (added 2026-11-14) |
| `gpt-6.1-terra` | GPT-6.1 Terra | 320,000 | 128,000 | 5 | Paid (added 2026-11-14) |
| `muse-spark-1.3` | Muse Spark 1.3 | 320,000 | 131,072 | 6 | Paid |
| `gemini-4-hydrogen` | Gemini 4 Hydrogen | 320,000 | 131,072 | 8 | Paid (added 2026-11-14) |
| `claude-sonnet-5-5` | Claude Sonnet 5.5 | 320,000 | 64,000 | 10 | Paid (added 2026-11-14) |
| `gemini-4-argon` | Gemini 4 Argon | 320,000 | 131,072 | 10 | Paid |
| `gemini-4-carbon` | Gemini 4 Carbon | 320,000 | 131,072 | 10 | Paid (added 2026-11-14) |
| `gpt-6.1-sol` | GPT-6.1 Sol | 320,000 | 128,000 | 10 | Paid |
| `claude-opus-5-5` | Claude Opus 5.5 | 320,000 | 64,000 | 15 | Paid |
| `muse-wonder` | Muse Wonder | 320,000 | 131,072 | 15 | Paid |
| `qwen-3.8-max-prime` | Qwen 3.8 Max Prime | 320,000 | 131,072 | 15 | Paid |
| `claude-fable-5-2` | Claude Fable 5.2 | 320,000 | 64,000 | 20 | Paid |
| `gpt-6-astra` | GPT-6 Astra | 320,000 | 128,000 | 50 | Paid, only `/v1/responses` |

Notes:

- Only `deepai-cyber` / `glm-5.3` / `glyph-cluster` / `solar-mini-4` / `minimax-m3` are text-only
 (`input: [text]`); all others accept image input (`input: [text, image]`); all support reasoning (`reasoning`);
- **The Lane 6 catalog normalizes `contextWindow` to 320,000** (since 0.3.37; Lane 5 still uses each model's
 real limit of 1M / 991k / 512k...). This is the new client's compression budget; the gateway passes it through as-is from upstream;
- **Starting 2026-11-14, the upstream `/deepai_code/models` requires `Api-Key`**: without a key, it always returns 401
 `auth_required` (the previous conclusion that "with or without a key, the same catalog is returned" is no longer valid). The gateway's catalog refresh and
 lane probing therefore both borrow credentials from one account in the account pool; see §10.1 for operational pitfalls;
- `gpt-6-astra` has `api` set to `openai-responses` and must go through `/v1/responses`;
 calling it via `/v1/chat/completions` will be rejected by upstream;
- Paid models are metered by `Theos/request` and deducted from the account's daily quota / balance; after the quota is exhausted, upstream returns 403 (see §6.9);
- If upstream also delivers `theosPerSecond` (per-second billing), the Console displays it in the "Per-second Billing" column,
 and it can also be read from the `deepai` metadata in `/v1/models`;
- **The online `/v1/models` is authoritative**: the repository does not contain state files (`deepaicode2api-data*.json`
 are runtime artifacts that contain upstream keys; they are gitignored and have been cleaned up); if this table and `/v1/models` disagree,
 first click "Refresh Model Catalog" in the Console, then re-export in the format of this table.

Built-in aliases (overridable in the Console; 13 total, all pointing to the default free model `glm-5.3-flash`):

- `deepai` / `deepai-code` / `default`
- `gpt-4` / `gpt-4o` / `gpt-4o-mini` / `gpt-4.1` / `gpt-3.5-turbo`
- `claude-3-5-sonnet` / `claude-sonnet-4` / `claude-3-haiku`
- `deepseek-chat` / `glm-4-flash`
- The `deepai/<id>` and `openai/<id>` prefixes are stripped automatically (`deepai:` / `openai:` have the same effect)

Unrecognized model names **by default** fall back to the "fallback model" → the catalog default model, ensuring any client works out of the box;
after **Strict Model Mode** is enabled in the Console, it instead returns `404 invalid_request_error` and lists similar
catalog IDs, with no more silent substitution (be sure to enable it for model fingerprint / channel verification scenarios).

---
## 3. API Reference

### OpenAI-Compatible

| Method | Path | Description |
| --- | --- | --- |
| GET | `/v1/models` | Model catalog (including `deepai` metadata) |
| POST | `/v1/chat/completions` | Streaming / non-streaming; supports tools, images, `prompt_cache_key` |
| POST | `/v1/responses` | Pass-through for `openai-responses` models; automatic translation for all other models |
| POST | `/v1/audio/transcriptions` | Forwards audio transcription to upstream (webm / mp4) |
| POST | `/v1/audio/translations` | Same as above (alias) |

### Anthropic-Compatible

| Method | Path | Description |
| --- | --- | --- |
| POST | `/v1/messages` | Streaming / non-streaming; supports `tool_use`, `thinking`, images |
| POST | `/v1/messages/count_tokens` | Local estimation (not provided by upstream) |

Claude Code only needs:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:10087
export ANTHROPIC_API_KEY=sk-...        # gateway key; may be left empty when no key has been issued
export ANTHROPIC_MODEL=glm-5.3-flash
```

### DeepAI Native Capability Extensions

| Method | Path | Description |
| --- | --- | --- |
| GET | `/v1/deepai/theos` | Free quota / balance for the current account |
| GET | `/v1/deepai/usage` | Subscription and usage summary |
| POST | `/v1/deepai/web_search` | Upstream web search (`{query, num_results}`) |
| POST | `/v1/deepai/find_service` | Upstream service recommendations |

### Admin APIs (Accounts / Pool Enrollment)

| Method | Path | Description |
| --- | --- | --- |
| POST | `/admin/api/accounts` | `action=enroll`: validate liveness and add to the account pool |
| POST | `/admin/api/checkin` | Quota probing. Optional `id` / `group` / `stale_only` / `limit`; the response includes `total` / `probed` / `remaining`, processed in batches |
| POST | `/admin/api/login/start` | Starts a browser login; returns `session_id` / `port` / `loopback` / `expires_in` |
| POST | `/admin/api/login/poll` | Polls the login result (local callbacks are collected automatically; `status=pending\|ok\|error\|expired`) |
| POST | `/admin/api/login/callback` | **Completes login by pasting the callback URL** (`{session_id, callback}`). Required for Docker / remote deployments: when the browser lands on `127.0.0.1:<port>/auth-callback?code=…&state=…` and the page won't open, just paste the full URL from the address bar back here |

### Console

| Path | Description |
| --- | --- |
| `/` | Embedded single-page console (no external dependencies) |
| `/ping` | `pong` |
| `/health` | Account / model / machine fingerprint summary; three counters `ready` / `degraded` / `blocked` (definitions in the last paragraph of §6.4) |
| `/metrics` | Prometheus text-format metrics (same auth level as `/admin/api/*`): request volume / status code classes, **per-endpoint latency histograms**, **check-in duration / blocking / timeouts**, **upstream error code distribution**, **shape fallback counts**, in-flight requests, account pool distribution, goroutines / heap |
| `/admin/api/version` | Current version + comparison against the latest release (the server queries GitHub on your behalf, cached for 6h; when offline, only the current version is returned, without affecting the console) |
| `/admin/api/*` | Accounts, keys, logs, statistics, settings, quota, login |

> Every response echoes back `X-Request-Id` (valid incoming values are echoed as-is), and the access log is one
> JSON object per line (`event=access` with `request_id` / `status` / `ms` / `bytes`), which can be filtered directly with `jq`.

---
## 4. Quick Start

```bash
# 1) Build (output goes to bin/; you can also run go build -o deepaicode2api.exe .)
make build

# 2) Start directly (automatically imports desktop credentials + fetches the model catalog)
./bin/deepaicode2api

# 3) Open the console
#    http://127.0.0.1:10087/

# 4) Add an account (console)
#    "Browser Login" signs in to upstream with Google / Apple / email + password;
#    you can also use "Import Local Desktop" or "Manual API Key"
```

> **`.env` is now loaded by the gateway at startup** (previously it was loaded lazily only on some paths,
> so `DEEPAI2API_MASTER_KEY` written to `.env` did not take effect for the main process).
> The state file contains the upstream API Key and account passwords; on Windows, `0600` does not set ACLs,
> so encryption should be enabled on first deployment. **Stop the running gateway first**, then run:

```bash
printf 'DEEPAI2API_MASTER_KEY=%s\n' "$(openssl rand -hex 32)" >> .env
./bin/deepaicode2api -seal -yes   # seal existing plaintext (including append-only historical logs)
./bin/deepaicode2api              # every later save is encrypted automatically
```

`-seal` prints the key fingerprint (first 8 characters of sha256) for verification and checks that each file no longer contains plaintext;
**the master key must be kept together with backups; if lost, the account pool cannot be recovered**.
If you only change the environment variable without running `-seal`, the main file will be automatically rewritten as ciphertext on the next save,
but historical log lines will remain plaintext (the reader is compatible with mixed formats and will not report an error).

Example startup log (messages translated from Chinese):

```text
State file: …/deepaicode2api-data.json
Machine fingerprint: a1b2c3d4e5f6… (used by upstream to grant daily free Theos)
Imported account desktop-… (~/.deepai-code/userdata/secrets/deepai-account.bin)
Model catalog updated: 18 models, default glm-5.3-flash
DeepAICode2API dev started: http://127.0.0.1:10087/
```

### Usage Examples

```bash
# OpenAI style
curl http://127.0.0.1:10087/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}'

# streaming
curl -N http://127.0.0.1:10087/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

Python OpenAI SDK:

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:10087/v1", api_key="sk-...")
print(client.chat.completions.create(
    model="glm-5.3-flash",
    messages=[{"role": "user", "content": "hello"}],
).choices[0].message.content)
```

---
## 5. Command-Line Parameters and Environment Variables

| Parameter | Environment Variable | Default | Description |
| --- | --- | --- | --- |
| `-host` | `DEEPAI2API_HOST` | `127.0.0.1` | Listen address |
| `-port` | `DEEPAI2API_PORT` | `10087` | Listen port |
| `-data` | `DEEPAI2API_DATA_PATH` | User config directory | State file |
| `-deepai-dir` | `DEEPAI2API_DATA_DIR` | Auto-detect | Desktop data directory |
| `-admin-password` | `DEEPAI2API_ADMIN_PASSWORD` | Empty | Admin password |
| `-allow-insecure` | `DEEPAI2API_ALLOW_INSECURE` | `false` | Allow non-loopback listening without setting an admin password |
| `-import` | - | - | Import credentials only and exit |
| `-no-import` | - | - | Do not auto-import on startup |
| `-seal` | - | - | Seal existing plaintext in state/stats/logs as ciphertext and exit (requires `DEEPAI2API_MASTER_KEY`) |
| `-yes` | - | - | Confirm irreversible operation with `-seal` |
| `-version` | - | - | Print version |

Other environment variables:

| Variable | Description |
| --- | --- |
| `DEEPAI_API_KEY` | Directly add an upstream account (optional) |
| `DEEPAI_MACHINE_ID` | Override the raw UUID of the machine fingerprint (required for container deployment) |
| `DEEPAI2API_MASTER_KEY` | Master key for state file encryption (64-character hex / base64, 32 bytes); leave empty for plaintext; write it in `.env` and it will be loaded automatically on startup |
| `DEEPAI_API_ORIGIN` | Override upstream address (self-hosted testing) |
| `DEEPAI2API_ADMIN_PASSWORD` | Admin password (console / admin API) |
| `PROXY_POOL` | Inference proxy pool (comma- or newline-separated full URLs; assigned by account round-robin when added to the pool) |
| `PROXY_POOL_FILE` | Proxy pool file (one per line) |
| `DEEPAI2API_ENV_FILE` | Specify `.env` path |
| `T3CODE_HOME` / `DEEPAI_CODE_HOME` | Desktop data directory |

### Diagnostic Tools

```bash
go run ./cmd/probe -usage                 # credentials + model catalog + quota
go run ./cmd/probe -prompt "hello"        # make one real call
go run ./cmd/probe -prompt "hi" -stream   # streaming call
go run ./cmd/probe -key <api-key>         # validate a specific key directly
```

---
## 6. Account Onboarding and Pool Governance

> **The email registration flow has been removed** (2026-11-10): the console "Bulk Registration" page,
> `/admin/api/register`, `cmd/register`, and the `internal/register` package have all been deleted.
> Account onboarding now supports only three methods: browser login, desktop import, and manual API Key; pool governance
> (grouping, proxy pool, machine fingerprint, quota re-probing) is unaffected. Historical design and measurements remain in
> `docs/changes.md` / `docs/lessons.md`; the code no longer provides this capability.

### 6.1 Account Onboarding Methods

| Method | Entry Point | Description |
| --- | --- | --- |
| Browser login | Console "Browser Login" / `POST /admin/api/login/start` | The DeepAI login page supports Google / Apple / email password; the local `127.0.0.1` callback exchanges for a Key, and on pool entry a machine fingerprint and proxy are assigned automatically |
| Desktop import | Startup parameter `-import` / Console "Import Local Desktop Client" | Reads the desktop `deepai-account.bin` |
| Manual API Key | `POST /admin/api/accounts {"action":"manual"}` | Enter only the Key, then use `assign-machine` / `assign-proxy` to complete it |

For the complete browser login flow, see `docs/recon.md` §4.2. Google-side login must be completed in the browser:
the upstream OAuth `redirect_uri` is fixed at deepai.org, and exchanging the code for a token requires its
`client_secret`; the server cannot do this on your behalf.

### 6.2 Pool Grouping and Polling

- Accounts have a `group` (default `default`);
- API Keys also have a `group`; after a request comes in, polling occurs only within the corresponding group;
- Keys not bound to a group use all groups;
- Admins can specify a group when creating a key in the console.

This lets one gateway serve multiple batches / tenants at the same time without cross-account mixing.

### 6.3 Proxy Pool

- Inference and pool entry share the same proxy pool: `PROXY_POOL` (comma / newline-separated full URLs) or
 `PROXY_POOL_FILE` (one per line);
- During browser login pool entry, an egress is automatically assigned by round-robin; existing accounts can be bound at any time:
 `POST /admin/api/accounts {"action":"assign-proxy"}`
 (by default only fills empty values; `"reassign":true` overwrites, `"clear":true` rolls back;
 can scope by `"id"` or `"group"`);
- Inference calls upstream using the proxy bound to the account (`Server.clientFor`); admin APIs only show
 `host:port` and do not leak proxy credentials;
- Diverse egress is valuable: daily free Theos is granted **per network (egress IP)**
 (see `network_bound` in `docs/recon.md`); after spreading 14 accounts across 12 egresses,
 accounts with quota increased from 5 to 10.

### 6.4 Machine Fingerprint (Why One per Account)

Upstream grants free Theos **once per day** per `X-DeepAI-Code-Machine`, so:

- Each account generates an independent machine UUID on pool entry (`deepai.NewMachineUUID`),
 and `/theos` and inference use it throughout (`Account.MachineID`);
- Only when there is no account-level fingerprint does it fall back to the process-level `MachineID()` (desktop import / legacy accounts);
- With a shared fingerprint, multiple accounts on the same machine cancel each other's quota - this was the root cause of
 `granted=0` in the old pool;
- Existing accounts can be backfilled with one command (only fills empty values, does not overwrite existing fingerprints):

```bash
curl -s -X POST http://127.0.0.1:10087/admin/api/accounts   -H "X-Admin-Password: $DEEPAI2API_ADMIN_PASSWORD"   -d '{"action":"assign-machine"}'
# {"ok":true,"assigned":20}
```

After backfilling, quotas are automatically re-probed asynchronously. The scheduler also prioritizes accounts with quota (`hasQuota`);
accounts known to have `balance=0` are used only as a fallback when the entire group has no quota.

### 6.4.1 Automatic Quota Re-probing

Upstream grants quota once per day according to `refresh_at`. Previously, you could only manually click "Refresh All Quotas";
653 accounts took 11 minutes at a time, and half would be interrupted because the page was closed (`r.Context()` canceled).
Now three layers of automatic mechanisms cover this:

| Layer | Trigger | Scope | Failure Handling |
| --- | --- | --- | --- |
| Pool-entry re-probe | 45s / 2min / 6min after pool entry | New accounts | Trust successful results |
| On-demand re-probe | Inference selects an account whose "snapshot has expired" | Only the accounts used, at most once every 15min | Does not change `Status` / `Failures` (a brief upstream hiccup should not cause scheduler misjudgment) |
| Background daemon | One batch every 5 minutes (≤24) | `refresh_at` has passed / never probed / not probed for over 6h | Same as above |

The three paths share one in-flight deduplication table and **4 global probe slots**: when snapshots for 653 accounts expire at the same time,
a burst of traffic will not stack up hundreds of parallel `/theos` calls (that is the form most likely to trigger risk control).
Those that cannot get a slot are picked up in the next round.

`/admin/api/checkin` now uses batching + progress semantics:

```bash
# probe only accounts in group g2 with stale quota, at most 50 per batch
curl -s -X POST localhost:10087/admin/api/checkin   -H "X-Admin-Password: $DEEPAI2API_ADMIN_PASSWORD"   -d '{"group":"g2","stale_only":true,"limit":50}'
# → {"ok":true,"total":120,"probed":50,"remaining":70,"results":[…]}
```

The console "Refresh All Quotas" automatically pages by `remaining` until the entire pool has been probed;
`/health` additionally provides `quota_fresh` (probed within 15 minutes) and `quota_due` (the daemon will deduplicate and probe).

Definitions of the three account counts in `/health` (`ready` / `degraded` / `blocked`):

| Field | Meaning |
| --- | --- |
| `ready` | Disabled accounts do not count; `blocked` does not count (will not self-heal); `error` does not count only when **the error is still fresh** (within 10 minutes) |
| `degraded` | `error` and the error is within 10 minutes - the console pill additionally shows "N recent failures" |
| `blocked` | Blocked by upstream policy (`social_login_required`, etc.) and requires manual handling |

`status=error` only means "the last request failed" and does not mean the account is broken: the scheduler skips only `blocked`,
so `error` accounts still participate in polling as usual; if there is no further failure for longer than `deepai.ErrorStaleAfter` (10 minutes),
the background 30s inspection changes it back to `ok`.

Transient failures (EOF / reset / timeout / 429 rate limiting / 5xx / capacity rejection / check-in throttling) do not even mark `error`;
they only increment `failures` and `last_err`; **daily quota exhausted** (`Too many requests today`)
is the exception - it will not self-heal that day, so it is marked as `error` and counted in `degraded` until the next day's `refresh_at`.

### 6.5 Measured: Quota Comparison Before and After Backfilling Machine Fingerprints

For the same pool (21 accounts: 1 desktop import + 20 legacy email accounts, 11 proxies),
run `/admin/api/checkin` once before and once after backfilling with `assign-machine`:

| Metric | Before Backfill | After Backfill |
| --- | --- | --- |
| Accounts with independent machine fingerprints | 0 / 21 | **21 / 21** (all 21 distinct) |
| Accounts with `balance > 0` | **0 / 21** | **20 / 21** |
| Typical quota | `balance=0.00000 granted=0.00000` | `balance=200.00000 granted=200.00000` |
| Quota refresh | - | `refresh_at=2026-11-07T04:03:03Z` (next day UTC 04:03) |
| 8 consecutive real inference calls | - | **8/8 HTTP 200**, landing on **8 different accounts** |

The only account with `balance=0` is the desktop-imported account (`source=desktop`, alone in the
`default` group) - its quota for the day under the original machine fingerprint had already been consumed by prior use.

> Note: The `balance` precision of `/theos` is 5 decimal places, and a single request costs about `1.8e-6`,
> so small calls do not show a visible balance decrease; to see consumption, check `/v1/deepai/usage`.

### 6.6 Acceptance Criteria

| Dimension | How to Check |
| --- | --- |
| Quota | Proportion of `balance > 0` after `assign-machine`; whether the three-layer re-probing catches up within 10 minutes |
| Inference | 200 rate for real models and provider branches (criteria in `docs/recon.md` §3.1) |
| Health | `quota_fresh` / `quota_due` in `/health`, and `quota` / `pool` events in logs |
| Scheduling | Whether polling within the same group is fair; whether `blocked` is skipped |

Only rerunning two or more batches under the same configuration provides a meaningful comparison (`docs/lessons.md` §4.4).

### 6.7 Security and Compliance

- The pool is only for managing **your own** DeepAI accounts; please comply with the upstream terms of service;
- State file permissions are `0600`; however, **on Windows, `0600` does not set ACLs**, so other users on the same machine can still read it.
 Therefore, it is strongly recommended to configure `DEEPAI2API_MASTER_KEY` to enable whole-file encryption (see §7);
- Upstream grants daily free quota per machine; this project does not bypass that restriction.

---
## 7. Console

- **Overview**: request volume, failure count, total token in/out, **today's token in/out**, **cache hit rate**,
 upstream cost, ready accounts in the pool, model catalog (free / paid, Theos, context, endpoints).
 Tokens and characters are displayed in compact **K / M / B** form (3 significant digits; hover for exact values); the model catalog keeps exact integers.
 "Today" is counted by **local date** and resets automatically across days; cache hit rate = upstream-reported
 `cached_tokens / prompt_tokens` (all three upstream shapes are normalized, see `gateway.ParseUsage`);
 when there are no samples it shows `—` rather than 0.0%.
- **Accounts**: import desktop credentials, browser login, manually add API keys, refresh quota;
 the list supports **group / status / keyword** filtering (server-side pagination + sorting);
 the row menu, besides enable/disable / delete / clear errors, also covers existing-data governance: **bind proxy / clear proxy,
 assign machine fingerprint (fills empty values only), change group, mark as verified**.
 The "masked display" toggle masks emails (`u***@domain`) and usernames, with the preference stored in localStorage;
 search/filtering still matches on full values, masking only affects display (handy for screenshots, screen recording, remote assistance).
- **Keys**: issue `sk-…` gateway keys; shown only once at creation; can **rebind group**.
 **When no key has been issued, only local loopback is allowed to call without a key; non-loopback sources always get `503 setup_required`.**
 If listening on a non-loopback address with no key yet, a `bootstrap` key is generated automatically at startup and printed once.
- **Logs**: endpoint, model, streaming, status, latency, character count, tokens, cost, account;
 supports filtering by **endpoint / account / minimum status code / keyword / start time** (characters and tokens also in K/M).
- **Settings**: timeout, retries, log retention, CORS, fallback model, strict model mode, model aliases, admin password,
 and per-key rate limiting (RPS / burst / concurrency cap, 0 = unlimited).
- **Top-bar version number**: shows the build version (`v0.1.2`); clicking jumps to the releases page; when the console starts it
 queries `/admin/api/version` once for the latest release (server-side cache 6 hours), and highlights when a new version is available.
- **Login**: the admin passphrase goes through a separate `/login` page (entry validated by `POST /admin/api/auth`),
 and only on success do you enter the console; the passphrase is stored only in **this tab's** `sessionStorage`, and the top bar only has "Log out".
 When any admin API returns 401, the passphrase is cleared automatically and you return to the login page. When no admin passphrase is set, it doesn't block anyone (local direct access).
 Whether the login gate is enabled is determined by `admin_required` from `/health`.

Secure defaults:

- When listening on a **non-loopback** address, an admin password must be set, otherwise startup is refused (use `-allow-insecure` to bypass);
- When no key has been issued, data-plane requests from non-loopback sources return `503 setup_required` rather than being allowed through;
- When no admin password is set, admin APIs **allow only local loopback**;
- Write operations perform same-origin validation to block CSRF;
- CORS by default allows only local origins and does not offer `*`;
- State file permissions are `0600`, and the console only shows masked keys.
- After configuring `DEEPAI2API_MASTER_KEY`, the state file is encrypted entirely with AES-256-GCM,
 and plaintext upstream keys / account passwords no longer appear on disk; an already-encrypted file, when the master key is missing or mismatched,
 **refuses to start** and will not silently reset to empty state.
- `-seal` re-seals **historical plaintext lines** in the append-only log as well (line-by-line validation,
 idempotent); changing only environment variables will not backfill old logs.
- The state file defaults to `os.UserConfigDir()` (Windows `%AppData%`, Unix `~/.config`),
 avoiding Program Files or shared drives; if a file already exists in the old location, it continues to be used, so the account pool is not lost.

---
## 8. Architecture and Directory Layout

```text
main.go                     assembly: flags -> store -> routes -> signals (no business logic)
tasks.go                    background tasks: persistence / catalog refresh / quota guard + import and seal commands
env.go                      startup flag and env parsing (defaults centralized; see §5)
cmd/probe/                  diagnostic tool
internal/deepai/            upstream client (protocol, SSE, machine fingerprint, credential import)
internal/envfile/           .env loading (applied at startup)
internal/registry/          model catalog parsing and aliases
internal/store/             state persistence + account pool scheduling (files <=400 lines after split)
  ├─ store_open.go         Open / Save / persistence snapshot / atomic writes
  ├─ store_accounts.go     account CRUD / pagination / scheduling entry points
  ├─ store_keys.go         gateway keys
  ├─ store_models.go       model catalog / machine fingerprint / settings
  ├─ store_telemetry.go    stats and log queries
  ├─ reseal.go             -seal: seals existing plaintext (including historical logs)
  └─ state_seal.go         AES-256-GCM envelope / key fingerprint
internal/proxy/             proxy pool (rotation / masked labels / file loading)
internal/pool/              account selection policy (group rotation / quota decision / cooldown window); testable without Store
internal/parallel/          bounded concurrent fan-out (concurrency limit / ctx cancellation / index-based backfill)
internal/retry/             backoff retry (thresholds / jitter / non-retryable error classification)
internal/httpx/             pure HTTP helpers and middleware (JSON responses / error envelope / decoding / gzip / ETag)
internal/gateway/           protocol conversion layer: field mapping + usage parsing; independent of HTTP / store / account pool
  ├─ anthropic_types.go    Anthropic request models (Messages / tools)
  ├─ anthropic_translate.go bidirectional Anthropic <-> Chat field mapping
  ├─ anthropic_stream.go   streaming content block manager (thinking / text / tool_use)
  ├─ responses_translate.go Responses <-> Chat field mapping
  └─ usage.go              Usage + upstream usage parsing + lenient JSON extraction
internal/server/           HTTP routing, streaming, console (assembly layer)
  ├─ chat.go               OpenAI Chat Completions (pass-through + normalization)
  ├─ anthropic.go          Anthropic Messages entry point
  ├─ responses.go          Responses API entry point (pass-through / Chat synthesis)
  ├─ responses_stream.go   Responses stream event generator (writes sseWriter, so it stays in this package)
  ├─ upstream.go           openModel: account selection / slot acquisition / failover / error classification
  ├─ accounts.go           account slots, concurrency limits, admission queueing
  ├─ models.go             model catalog parsing and alias fallback
  ├─ sse.go                SSE output and [DONE]
  ├─ reqlog.go             per-request instrumentation and stats persistence
  ├─ probe.go              quota probing (on-demand / daemon / batched checkin)
  ├─ extensions.go         quota, usage, search, speech
  ├─ login.go              browser login (RFC 8252 loopback callback)
  ├─ admin_accounts.go     account admin entry point: GET pagination / POST dispatch by action
  ├─ admin_actions.go      10 actions, each with a minimal request struct
  ├─ gateway_alias.go      short package-local names for gateway (type aliases, migration compatibility)
  ├─ web.go                console asset route allowlist (assets live in the web/ package)
  └─ web/                  console static package (assets.go: go:embed + ETag + CSP)
       ├─ index.html        page markup (301 lines)
       ├─ style.css         styles (design tokens + layout)
       ├─ js/app.js         assembly layer: data fetching / URL state / startup order (231 lines)
       ├─ js/events.js      event wiring: navigation / top bar / filters / sorting / language / masking
       ├─ js/theme.js       theme, sidebar collapse, mobile drawer
       ├─ js/pager.js       account pagination control (page number / page size)
       ├─ js/i18n.js        dictionary + text replacement (zero dependencies)
       ├─ js/format.js      pure formatting (zero dependencies)
       ├─ js/state.js       shared mutable UI state (pagination cursor)
       ├─ js/dom.js         DOM helpers + inline menus
       ├─ js/table.js       table header sort state and pure functions
       ├─ js/api.js         admin API calls (auth / error normalization / deduplication / timeout retries)
       ├─ js/ui.js          toast
       ├─ js/actions.js     account / key changes + quota probing (refresh injected via setReload)
       └─ js/render/        four render modules: accounts / keys / logs / settings
```

Layering principles:

- **`deepai`** only cares about the upstream protocol (paths, request headers, error semantics, SSE) and is unaware of the account pool;
- **`store`** is only responsible for state and scheduling (persistence, grouping, account switching on failure, quota determination) and does not make network requests;
- **`server`** only performs protocol translation and authentication; account selection always goes through `Store`;

This way, upstream protocol changes only affect `deepai`, and scheduling policy changes only affect `store`;
tests can run separately against a fake upstream and a temporary state file.

---
## 9. Deployment (Docker)

```bash
docker build -t deepaicode2api .
docker run -d --name deepaicode2api \
  -p 10087:10087 \
  -e DEEPAI_API_KEY=<api-key> \
  -e DEEPAI_MACHINE_ID=<raw machine UUID> \
  -e DEEPAI2API_ADMIN_PASSWORD=<admin password> \
  -v $PWD/data:/data \
  -e DEEPAI2API_DATA_PATH=/data/deepaicode2api-data.json \
  deepaicode2api
```

> `X-DeepAI-Code-Theos: 6` is the lane for the current client (0.3.37): on
> `/chat/completions`, both 5 and 6 return 200, while older 2-4 and newer 7+ return `426 lane_version_unsupported`
> (`/models` is relaxed to 1-6, see `docs/recon.md` §3).
> `X-DeepAI-Code-Machine` is still a required header for the metering model: when missing, it returns `400 machine_required`.
> Containers usually cannot read the host machine's SMBIOS UUID, so `DEEPAI_MACHINE_ID` is **required** for container deployments.
> Enter the original UUID of the machine where the desktop client runs to reuse the same daily free quota.
>
> **Browser login** inside the container must use the paste-callback flow: the upstream redirects the browser to
> `http://127.0.0.1:<random session port>/auth-callback?code=…&state=…`, but this
> `127.0.0.1` is the **host machine's own loopback**; the gateway listens inside the container (and the port is randomly
> assigned for each login), and `-p` cannot map to the container's loopback either. So the browser will stop on a "cannot connect"
> page - this is expected behavior, not a broken gateway. Paste the **full URL from the address bar** into the console
> under "Browser Login → Paste Callback URL" (`POST /admin/api/login/callback`), and the gateway
> will complete the code → Key exchange and add it to the pool. `code` is one-time use; the same URL can only be redeemed once;
> `state` must match the current session (pasting only `code` will be rejected).
>
> On Linux, you can also use `--network host` to let the container share the host loopback, so the local callback works directly;
> Docker Desktop (Windows/macOS) does not offer the same behavior.

---
## 9.5 Versions and Releases

The version number is injected at build time via `-ldflags -X main.version=...`, defaulting to `dev`:

```bash
make build                       # Makefile automatically uses git describe as the version
go build -ldflags "-X main.version=v0.1.2" -o bin/deepaicode2api .
./bin/deepaicode2api -version    # print the version and exit
```

- The **console** displays this version number at the top; click it to open the releases page.
- **Update check**: when the console starts, it requests `/admin/api/version`; the server queries
 `api.github.com/repos/<owner>/<repo>/releases/latest` on its behalf and caches the result for 6 hours.
 When offline, blocked, or the repository is unreachable, only the current version is returned (with the `error` field explaining why).
 This does not affect console usage and does not pop up an error in the UI.
- **Development builds** (`version=dev`) skip update comparison; otherwise they would always show "a new version is available".
- **Docker builds** default to `VERSION=docker` (no semver can be parsed, so no comparison is done and no upgrade prompt is shown).
 To have the console display the real version and highlight it when a new version is available:

  ```bash
  VERSION=$(git describe --tags) docker compose up -d --build
  # equivalent: make docker (automatically includes the git describe version)
  ```
- Release process: `git tag v0.1.5 && git push origin v0.1.5`, then use
 `sh scripts/release.sh v0.1.5 --publish` to cross-compile binaries for each platform and create a GitHub Release
 (before building, the script clears the previous version's `deepaicode2api_*` artifacts from `dist/`: `gh release upload`
 uploads `dist/*`, so leftover old binaries would be uploaded as well).
## 10. Limitations and Notes

- `X-DeepAI-Code-Theos` must be sent as `6` (since 0.3.37, re-verified 2026-11-15; metadata endpoints 1-6 all accept it, inference endpoints 5 / 6 all accept it - don't use `/models` to judge upstream availability); if the inference endpoint lane doesn't match, it returns 426 `lane_version_unsupported`;
- The `v=` in the heartbeat `X-DeepAI-Code-Beacon` must be the current client version (currently **0.3.37**, see `deepai.AppVersion`); `pi=` has been optional since 0.3.30, but sending it does no harm;
- Lane probe results are trusted unconditionally when verified by the **current build** (`Settings.LaneBuild == deepai.LaneVersion`), even if older than the compiled value; otherwise the newer of `max(stored value, compiled value)` is used. The probe sequence starts from the compile-time lane and **must not try old lanes first** (`/models` is still lenient toward old lanes);
- Body-shape rejection, besides matching both versions of the message text, also falls back to `400 + code=invalid_request_error`; if the shape-patching retry still fails, the second error is returned;
- Anthropic `budget_tokens` is mapped by magnitude to `low/medium/high/xhigh/max` (boundaries at 4k/16k/32k/64k), and `output_config.effort` is passed through as-is; as of 2026-11-15, upstream accepts all six tiers;
- Inference requests send `user-agent: node` + `accept: */*` per desktop-measured values, **without** Chrome UA / `sec-ch-ua` / `Accept-Language` (the inference side only sends desktop request headers);
- When `X-DeepAI-Code-Machine` is missing, the metering model returns `machine_required`, so environments without SMBIOS must set `DEEPAI_MACHINE_ID`;
- 0.3.19 once narrowed inference to accounts signed in via **Google/Apple login** (email/password accounts got `403
 social_login_required`); **re-testing on 2026-11-09 confirmed this is lifted**, and email accounts work again.
 The gateway still treats this error code as a terminal policy state (marks `blocked`, stops scheduling), so
 no code change is needed if the policy tightens again; when you see `blocked`, first check whether `last_err`
 is still this code, then decide whether to re-authorize;
- Upstream free quota is issued daily per "machine fingerprint + account", and returns 403 once exhausted;
 the machine fingerprint is **one per account** (see §6.4), so when shared, only the first account can claim it;
- `openai-responses` models such as `gpt-6-astra` must go through `/v1/responses`; calling them via
 `/v1/chat/completions` is rejected by upstream (the gateway passes the error through as-is);
- Speech transcription upstream only accepts `audio/webm` / `audio/mp4`; transcode other formats first;
- **The managed-host control plane** `/deepai_code/connect/*` (device registration / remote hosts / TLS relay) **is not implemented**:
 it requires Ed25519 + X25519 dual-key device identity and managed-host lifecycle, and serves the desktop
 "turn the machine into a managed host" feature, which is unrelated to inference (reverse-engineering details in `docs/recon.md` §2.1);
- Model catalog and lane probing **must carry an account's `Api-Key`** (since 2026-11-14; the earlier "credential-free"
 conclusion is no longer valid): `GET /deepai_code/models` without a Key always returns 401 `auth_required`.
 Neither catalog nor lane probing consumes Theos, so the gateway can borrow credentials from one account in the pool
 (`Server.probeAccount`: prefers a ready account with a machine fingerprint); when there is no account, refreshing the catalog
 directly reports "please import an account first" instead of throwing the 401 to the console; proxy warm-up counts 401 as success
 (the link is up), see `docs/recon.md` §2;
- `/v1/messages/count_tokens` is a character-count estimate, not the upstream's real tokenizer;
- The state file stores upstream API Keys in plaintext by default (consistent with the desktop client); configuring
 `DEEPAI2API_MASTER_KEY` encrypts the whole file - be sure the master key does not leak with backups;
- Browser login exchanges for an upstream API Key and writes it to the state file; whole-file encryption can be enabled (see above);
- Proxy URLs contain account credentials and must only be placed in `.env` / local ledgers (already gitignored), never committed;
- **Single-instance deployment**: the account pool, rate-limit buckets, and in-flight counters all live in process memory, and the state file is single-writer
 (`tmp + fsync + rename`). Do not mount the same `DEEPAI2API_DATA_PATH` to multiple replicas
 writing simultaneously, and do not expect scheduling state to be shared across replicas - horizontal scaling requires external storage, which is currently out of scope;
- **Data-plane rate limiting is off by default**: the console's "Runtime Parameters" can set `rate_limit_rps` (requests per second per key),
 `rate_limit_burst` (bucket capacity), and `key_concurrency` (per-key concurrency cap); 0 means unlimited.
 Rate limiting is isolated per key; when triggered it returns `429` + `Retry-After` + `rate_limit_error`;
- **Check-in budget** (`checkin_block_ms`, default 10000): how long inference may wait for a synchronous check-in before proceeding.
 Production measurements on 2026-11-13 showed check-in blocking 292 times, averaging 4.8s, with 53 hitting the 10s cap - the main
 source of first-byte latency; lowering it to 2000 significantly improves latency (at the cost of this request possibly going out with an old nonce,
 which is topped up next round and does not fail);
- **Preset desktop shape** (`preset_desktop_shape`, default off): for callers confirmed to need the desktop shape
 (remembered per API Key for 10 minutes), a system message is added before the first send, saving a 400 that is bound to fail.
 When off, the learning path of "pass through → 400 → patch shape and retry" still applies;
- **Check-in cadence** matches the desktop client (based on the completion time of the callback, `seconds/2` defaults to 300s);
 the previous logic of "latest nonce ≠ returned nonce" degraded to once every 30s (see `docs/changes.md` §60);
- **Admin password has brute-force backoff**: after 5 consecutive failures from the same source, exponential backoff kicks in (starting at 1s, capped at 30s);
 during backoff, even the correct password returns `429`; one success resets it. If you lock yourself out, wait for the backoff to expire
 or restart the process (the backoff table is in memory);
- **Logs are JSONL**: startup and request logs each output one JSON object per line (`slog`), with `msg` kept verbatim;
 access logs carry `request_id` / `status` / `ms` / `bytes`; use `jq -r .msg` to restore a human-readable form.

### 10.1 Known Pitfalls (read before changing)

These are all pitfalls we've hit; `docs/recon.md` has more complete reverse-engineering notes:

| Symptom | Cause | What not to "fix" it into |
| --- | --- | --- |
| Invalid Key returns 200 with HTML | Upstream 302 to the login page | Do not follow with `CheckRedirect`; `ErrUseLastResponse` is intentional |
| "Models changed" after switching UA | **As of the 2026-11-13 re-test there is no UA-based routing** (`node` / `curl` / Chrome all return the same response shape); for historical reasons see `docs/recon.md` §3.1 | Do not "fix" the pool's UA based on old conclusions; the gateway continuing to send `user-agent: node` is just for desktop consistency |
| Response lacks `provider` | **No longer usable as a criterion**: since 2026-11-13, real requests also no longer send `provider` / `service_tier` / `cost_details`, `id` is uniformly `gen-<32 hex>`, and `model` returns `zai/<slug>` | Stop judging substitutes by `provider` / `gen_` / cost (see `docs/recon.md` §3.1; use model fingerprints to verify accounts) |
| Pool "fingerprint suddenly degrades" | Check-in not completed: only pulling `/theos` without returning the nonce is equivalent to not checking in, and falls into the substitute branch | Wait one `EnsureCheckIn` renewal cycle; changing the machine fingerprint resets it to cold state |
| `403 check_in_refused` | Today's "restart check-in" count is exhausted (`restarted its check-in too many times today`) | Do not retry, and do not mark the account as error: inference still works and it recovers automatically tomorrow; also stop running check-in health checks |
| `400 This request cannot be accepted.` | New wording for a request body that isn't desktop-shaped (since 2026-11-13; `node` UA + real local machine fingerprint + missing system message. When the gateway and desktop client are co-deployed, this is **normal**) | Don't only match the old wording `not one DeepAI Code sends`: `BodyRejected` must match both, otherwise `ChatBodyFallback` won't retry (see `docs/recon.md` §5.2) |
| Upstream model catalog default model is empty | The field is called `default`, not `default_model` | Do not add `default_model` compatibility; it will silently fail to get the value |
| Refreshing model catalog / lane probe returns 401 `auth_required` | **Since 2026-11-14 `/models` requires `Api-Key`** (previously credential-free); with an empty pool it is inevitably 401 | Do not treat it as "Key invalid" and clear accounts; the gateway has been changed to borrow one account's credentials (see `docs/recon.md` §2.2) |
| Pool account `granted=0` | Shared machine fingerprint; daily quota is issued only once | Do not remove the `Account.MachineID` fallback logic |
| Long responses get cut off | Upstream heartbeat `: keep-alive` is dropped | `streamChat` must pass comment lines through verbatim |
| Tool-call arguments truncated | Multiple `data:` lines in the same event not joined by `\n` | Use `deepai.ReadSSE`, not line-by-line `json.Unmarshal` |
| Passwords disappear from the ledger | `Append` goes through `redactRecord` | This is by design; passwords are in `*-credentials.jsonl` |
| `go test -race` link failure (`cannot find 'ld'`) | The mingw runtime only ships `ld.bfd.exe` | Do not require PATH changes; run `make race-shim` to add a same-named copy |
| Non-streaming request returns 502 "not valid JSON" | Upstream **defaults to SSE when `stream` is absent** | Must explicitly send `stream:false`; don't treat `stream` as an optional field to omit |
| New account in pool has `balance=0` and is skipped long-term | Upstream hasn't issued quota yet at the moment of joining | Do not cache 0 as fact; keep the 45s/2m/6min re-probes and the 15-minute staleness check |
| Quota only half-probed after closing the console | Background task mistakenly uses `r.Context()`, cancelled when the page closes | Background tasks must always detach from the request ctx and use independent timeouts |
| One dead egress causes a whole batch to get 0 quota | `No exit node` is an egress failure, not a bad account | Do not delete and recreate accounts; rebind with `assign-proxy` |

Also: `reasoning_content` in `/v1/chat/completions` is a compatibility field added by the gateway;
upstream natively only has `reasoning`; it will not be overwritten if already present.
## 11. Document Index

**Check the "Status" column before the content**: `Maintained` = consistent with the current code and usable as a basis; `Historical` = retained process records whose conclusions may have been overturned by later reviews (defer to the latest entries in `docs/changes.md` and `docs/recon.md`).

| File | Content | Status |
| --- | --- | --- |
| `README.md` | Overview, interfaces, parameters, acceptance baseline, and known pitfalls (this document) | Maintained |
| `docs/recon.md` | Reverse-engineering quick reference: upstream endpoints, request headers, error semantics, reproduction commands (replaces HAR) | Maintained (last reviewed 2026-11-14) |
| `docs/changes.md` | Change log for 2026-11: root causes, decisions, and acceptance (§61/§62 are the latest round) | Maintained (Chinese only; not translated) |
| `docs/lessons.md` | **Distilled engineering experience**: ten rounds of load testing → reusable rules + pre-change checklist | Maintained |
| `docs/performance.md` | Load-test data and optimization plan: per-order E2E, stage breakdown, P0/P1/P2 benefit estimates | Maintained (data snapshot 2026-11-10) |
| `docs/pi-lens.md` | pi-lens configuration notes for this repo: what was disabled, why, and how to verify and restore | Maintained |
| `docs/model-fingerprint-2026-11-10.md` | Measured fingerprints of the 22 models in the pool (criteria and reproduction commands) | Historical snapshot: the directory now holds 27 models; the criteria still apply |
| `docs/optimization-plan.md` | Complete optimization plan from the first scan | Historical: fully implemented (Chinese only) |
| `docs/optimization-plan-r2.md` | Plan and benefit estimates from the second scan (write amplification / console / re-probing) | Historical: implemented (Chinese only) |
| `docs/assets/` | Project icons: `logo.png` (512, used by README) / `logo.ico` (multi-size 16-256, for Windows); the console's `/favicon.ico` is the same file (see `docs/changes.md` §58) | - |
| `.env.example` | All configurable options and their descriptions | - |
| `Makefile` | `make build / test / test-race / lint / cover` | - |

## 12. License

MIT. This project only replicates the call chain of the client already logged in on the local machine; please comply with DeepAI's Terms of Service.

> **Logo exception**: `docs/assets/logo.png` / `logo.ico` are taken from the DeepAI Code desktop app's own application icon
> (`apps/desktop/prod-resources/icon.png` inside `resources/app.asar`), and the copyright belongs to its owner.
> **They are not covered by the MIT license**; they are used here only to identify "which upstream client this gateway is for."
> If you need to distribute them yourself, please first confirm the icon's usage license, or replace it with your own image.
