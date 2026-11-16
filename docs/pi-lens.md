# pi-lens configuration for this project

The `.pi-lens.json` in the repository root is this project's **pi-lens** (coding-time diagnostic tool) configuration.
It does not participate in compilation and is not included in the binary; it only affects the diagnostic noise that humans and AI see when writing code in this repository.

pi-lens configuration has two layers: **project-level** `.pi-lens.json` (in the repository where this file resides, committable and reviewable)
and **machine-level** `~/.pi-lens/config.json` (each person's own). This repository only provides the project-level one
and does not decide for users what tools to install on their machines.

## Why it is needed

Previously this repository had no pi-lens configuration, so it ran with defaults:

| Symptom | Root cause | Handling |
| --- | --- | --- |
| Dozens of `spawn failure classified: tool-not-found` per session | `typos` / `vale` / `markdownlint` / `yamllint` / `bash-language-server` / `vscode-css\|html-language-server` / `docker-langserver` / `yaml-language-server` are all not installed (this repository deliberately has zero toolchain) | Disable the corresponding runner / server (see below) |
| Long Chinese documents repeatedly scanned by spelling/prose checks | `README.md` (47KB) and `docs/*.md` (300KB+) are long Chinese technical documents | Disable `typos` / `vale` / `markdownlint` |
| Valid code reported | `waitgroup-done-scope` treats `ctx.Done()` as `WaitGroup.Done()`; `nil-map-assignment` does not see that `json.Unmarshal(&m)` allocates; the Semgrep XSS rule targets HTML, while the gateway writes JSON / SSE | Disable the first two rules; change the third to go through the unified `writeRawJSON` (which also eliminates duplicated response header assembly) |
| Complexity warnings persist | `openModel` is a known scheduling state machine (cc 39), and splitting it requires separate design | Threshold 15 → 25, with a separate exemption for that file (other new code is still bound by 25) |

## What the configuration does

### Scan scope (`ignore`)

Runtime state and build artifacts are not indexed: `dist/`, `bin/`, `exports/`, `coverage.out`,
`*.out`, `.env*`, `deepaicode2api-data*.json*`, `*.jsonl`, `*.bak`.
The state files and ledgers **contain upstream keys / passwords** and should never be read into the index by any tool.

### Disabled LSP / runners

- `lsp.disabledServers`: `bash`, `css`, `docker`, `html`, `python`, `typos`, `yaml`
 - the servers for these languages are not installed on this machine/in this repository, and enabling them would only flood each session with `tool-not-found`.
 **Cost**: these files will show as *unconfirmed* (cannot confirm clean) rather than *clean*.
- `dispatch.disabledRunners`: `vale`, `markdownlint`, `yamllint`
 - Chinese documents and YAML are not part of this repository's quality gate (there is only one YAML each for CI and compose).

### Retained checks

Go: `go-vet`, go LSP, tree-sitter, ast-grep, opengrep (Semgrep), `fact-rules`.
Frontend (zero-build ES module): TypeScript/Deno LSP, `oxlint`, `biome-check-json`, ast-grep.

### Rule policy (`rules`)

| Rule | Policy | Reason |
| --- | --- | --- |
| `high-complexity` | Threshold 25; `internal/server/upstream.go` exempted | See the table above; new code is still bound by 25 |
| `high-fan-out` | Threshold 30 | The default 20 is too strict for "routing + assembly" style handlers |
| `nil-map-assignment` | disable | The rule does not track writes through `&m`; `var m map[…]` + `json.Unmarshal` is valid code |
| `waitgroup-done-scope` | disable | The rule treats `ctx.Done()` as `WaitGroup.Done()` as well |
| `no-console-except-error-js` | `ignorePaths: js/api.js` | The `console.warn` there is a deliberately retained English UI troubleshooting output |
| `no-array-reverse-mutation-js` | `ignorePaths: render/logs.js` | The actual code is `list.slice().reverse()`, and the rule cannot see through `.slice()` |

Real code issues are **not handled by exemptions** but by directly changing the code: in this round, `isFinite` → `Number.isFinite`,
`!!m.free` → `Boolean(m.free)`, and upstream error bodies were unified through `writeRawJSON`.

## How to verify the configuration takes effect

```bash
# In pi:
#   effective_config path=internal/server/upstream.go
# Expect: documents includes the project .pi-lens.json, provenanceCounts.project > 0,
#         recordCounts is empty (a value means some entry id is wrong; the log will show PILENS_CFG_0005),
#         disabled servers show disabled-by-config and identify the key and tier.

#   lens_diagnostics path=<directory> scope=paths mode=full source=lsp
# Expect: exempted entries no longer appear; the end of the output will state
#         "suppressed by disposition: N finding(s) …" (not "clean", but "hidden by policy").
```

No restart is needed after changing the configuration: `rules` and the denial list are parsed per file, and take effect on the next dispatch.

### What is `●1E !20W` in the status bar

That badge reads the **session cache** (cumulative findings for each file in this session), not "the current code has 1
error". For edited files, entries at old line numbers remain in the cache until a rescan. A real determination
requires actively probing once:

```text
lens_diagnostics mode=full source=lsp scope=paths paths=[<changed files…>]
```

The criterion is `clean=N` and `findings=N` in `Outcomes`, not the badge number. Measured on 2026-11-12:
the badge showed `●1E !20W`; running `mode=full` on the 14 files of that release → `1 warning` (a `!!on` in `console-smoke.mjs`, changed to `Boolean(on)`), the rest clean; probing again → `0`.
What remains are **quality hints** for test code (`time.Sleep` / `context.Background()` /
`_ = Encode(...)` / `s, _ := newTestServer(...)`); the tool misjudges tests as handlers,
and it is not worth introducing wrappers for them.

## When to turn these back on

- After installing `typos` / `vale` / `markdownlint` / `yamllint` / `shfmt`: remove the corresponding entries from
 `dispatch.disabledRunners`;
- To get LSP diagnostics for the console's CSS / HTML: `npm i -g vscode-langservers-extracted`
 (provides css/html/json/eslint servers), then remove `css` / `html` from `lsp.disabledServers`;
- After installing `golangci-lint`: this repository also needs a `.golangci.yml` for pi-lens to select it
 (it requires "a project configuration" to enable, to avoid scanning with the default rule set and reporting issues that conflict with project conventions).
