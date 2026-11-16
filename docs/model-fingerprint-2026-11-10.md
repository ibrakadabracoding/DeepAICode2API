# Model Pool Model Fingerprint Test (2026-11-10)

Using [LM Fingerpoint Detector](https://lm.ikale.io) (`lmfpd` CLI) to test each of the 22 models in the model pool one by one to see
**which model they are actually routed to**. The detector asks the model to write ~300 integers from 1-355 based on first instinct,
then compares the distribution of these numbers against a reference library.

> **The catalog has changed (2026-11-14)**: The upstream model catalog grew from 22 to 27 (newly added `muse-ember` /
> `kimi-k3.1-pro` / `gpt-6.1-terra` / `gemini-4-hydrogen` / `gemini-4-carbon` /
> `claude-sonnet-5-5`). This document is a **historical snapshot**: those 6 new models have not been tested,
> but the **criteria** for existing conclusions **still apply** (when choosing models, look at the actual routing, not the `provider` field).
>
> **Nature of the conclusions**: What the detector provides is a "closed-set ranking within the reference library,"
> and every result carries `evidence.insufficient=true`. **It is a reference, not proof of identity**.
> To judge channel credibility, fix the parameters, repeat across multiple rounds, and combine with other evidence.

## Method

```bash
# Gateway local loopback without key (when keys=0)
npx lmfpd@latest -b http://127.0.0.1:10087/v1 -k sk-local \
  -m <model> -a chatcompletion --count 3 -p 1 --timeout 180 --json
```

Two lessons that must be remembered:

1. **`-p 1` (sequential sampling) is a hard requirement**. With the default `-p 3` concurrency, 2 of the 3 samples return empty text
 (`used_outputs=1`, `probability_status=unavailable`), and all results are invalidated.
 After switching to `-p 1`, all 3 samples are valid (same cost, only more time).
2. **`gpt-6-astra` requires `-a responses`**; the rest use `-a chatcompletion`.

## Results

| Model | Billing | Identified as | Confidence | Conclusion |
| --- | --- | --- | ---: | --- |
| `glm-5.3-flash` | Free | glm-5.3-flashx | 0.747 | ✅ Same-family variant |
| `deepseek-v4.1-flash` | Free | deepseek-v4.1-flash-1017 | 0.980 | ✅ |
| `qwen-3.8-flash` | Free | qwen3.8-flash | 0.912 | ✅ |
| `minimax-m3.1-flash` | Free | grok-4.5 | 0.305 | ❌ Mismatch |
| `solar-mini-4` | Free | qwen3.8-omni-flash | 0.177 | ❌ Mismatch |
| `gpt-6-luna` | Free | qwen3.8-omni-flash | 0.323 | ❌ Mismatch |
| `glyph-cluster` | Free | kimi-k3.1-pro | - | ⚠️ Only 1/3 samples valid |
| `mimo-2.6-flash` | Free | - | - | 🚫 Timeout (300s all 3 times) |
| `mimo-2.6-pro` | Free | - | - | 🚫 Timeout |
| `crouton-alpha` | Free | - | - | 🚫 3/3 empty responses |
| `deepai-cyber` | Free | - | - | 🚫 3/3 empty responses |
| `glm-5.3` | 3 | glm-5.3 | 0.999 | ✅ |
| `kimi-k3` | 3 | kimi-k3 | 0.550 | ✅ |
| `muse-spark-1.3` | 6 | muse-spark-1.3 | 0.984 | ✅ |
| `claude-opus-5-5` | 15 | claude-opus-5.5 | 0.999 | ✅ |
| `muse-wonder` | 15 | muse-wonder | 1.000 | ✅ |
| `claude-fable-5-2` | 20 | claude-fable-5.2 | - | ⚠️ Only 2/3 samples valid |
| `minimax-m3` | 2 | qwen3.8-max-0902 | 0.651 | ❌ Mismatch |
| `gpt-6.1-sol` | 10 | mimo-v2.5-pro | 0.523 | ❌ Mismatch |
| `gemini-4-argon` | 10 | qwen3.8-omni-flash | 0.562 | ❌ Mismatch |
| `qwen-3.8-max-prime` | 15 | - | - | 🚫 Timeout → **see supplementary test at end for 2026-11-11** |
| `gpt-6-astra` | 50 | - | - | 🚫 3/3 empty responses (responses endpoint) → **see supplementary test at end for 2026-11-11** |

Statistics: **8 match**, **6 fingerprint mismatches**, **2 insufficient samples**, **6 unable to obtain valid output**.

> Supplementary test on 2026-11-11: Both `qwen-3.8-max-prime` and `gpt-6-astra` can produce results normally, but according to the §3.1 criteria they fall into the **substitute branch** (`gen_` prefix + no `provider` + cost=0), see the end of the document.

## Corroborating evidence from the model pool side

- In the gateway logs, **75 times** `[upstream] request body rejected, retrying with desktop-client shape`
 - the upstream is still picky about request body shape, and the gateway goes through the `ChatBodyFallback` retry path;
 empty responses most likely come from this retry path where the upstream still returns 200 with no content.
- Single detection session statistics: `total_requests=122`, `failed_requests=50`,
 `total_cost=0.2764` (upstream billing basis); one account's quota dropped from 200 to 119.35.
- Only **2 accounts are ready**, and concurrency/headroom for paid tiers is tight; this is also one of the reasons `-p 3` is more likely to fail.

## Reproduction

```bash
# 1) Start the gateway (locally)
go build -o bin/deepaicode2api.exe .
./bin/deepaicode2api.exe -no-import

# 2) Get the live catalog
curl -s http://127.0.0.1:10087/v1/models | python -c "import json,sys;print('\n'.join(m['id'] for m in json.load(sys.stdin)['data']))"

# 3) Test one by one (be sure to use -p 1)
for m in $(cat models.txt); do
  npx -y lmfpd@latest -b http://127.0.0.1:10087/v1 -k sk-local \
    -m "$m" -a chatcompletion --count 3 -p 1 --json > "out/$m.json"
done
```

## Notes

- The catalog is based on the **online `/v1/models`** (22 models). If an old
 `deepaicode2api-data.json` remains in the repository, its `models` field may lag behind the upstream (18 have been seen before),
 so do not use it as the basis for the catalog.
- This file is a snapshot from 2026-11-10. Conclusions will change after the upstream switches backends or changes routing; rerun at that time.

---

## Supplementary test: `qwen-3.8-max-prime` and `gpt-6-astra` (2026-11-11)

The two models above that "could not obtain valid output" were retested. This time, instead of using an external detector, we looked directly at the upstream raw
response (criteria in `recon.md` §3.1), and **ran free models on the same account and same proxy as a control** to rule out
account/proxy factors. Each sample used a different model pool account to avoid single-account quota or cache interference.

| Model | Endpoint | Samples | `id` | `model` echo | `provider` | `system_fingerprint` | `cost_details` | Output |
| --- | --- | ---: | --- | --- | --- | --- | --- | --- |
| `qwen-3.8-max-prime` | chat | 4 | `gen_<ULID>` | `alibaba/qwen3.8-max-prime` | **Missing** | `fp_…` (different each time) | All 0 | ✅ Correct |
| `gpt-6-astra` | responses | 4 | `gen_<ULID>` | `openai/gpt-6-astra` | **Missing** | **Missing** | Field absent | ✅ Correct |
| `glm-5.3-flash` (control) | chat | 2 | `gen-<epoch>-<rand>` | `z-ai/glm-5.3-flash` | `InferenceNet` / `Friendli` | Missing | 7e-6 / 3.2e-5 USD | ✅ Correct |

### Conclusions

1. **Both models can produce results normally**: `qwen-3.8-max-prime` 4/4 and `gpt-6-astra` 4/4 returned 200,
 and the content matched the instruction ("fp-sample-N"). The previously recorded "timeout / 3/3 empty responses" was a **detector-side** problem
 (concurrency 3 sampling, responses endpoint unsupported), not upstream unavailability.
2. **According to the §3.1 criteria, both fall into the substitute branch**: `id` is the ULID prefix `gen_`, `provider` is missing,
 and `cost_details.upstream_inference_*` are all 0. On the same account, `glm-5.3-flash` is cleanly
 `gen-<epoch>-<rand>` + `provider=…` + cost>0 - the difference is **model-level routing**, not an account or proxy anomaly.
3. **The substitute now echoes the real vendor slug** (`alibaba/…`, `openai/…`), no longer
 `reflection/beam`. Therefore, the **auxiliary** criterion in §3.1 that "`model` does not echo the requested slug → substitute"
 no longer applies to these two models; the only reliable criteria left are "`provider` missing + `id` uses the `gen_` prefix + cost is 0".
4. **Billing and catalog price are not the same thing**: catalog prices are 15 / 50 Theos, but in actual testing, after consecutive samples on the same account, the Theos balance
 only dropped by **0.03-0.07 per request** (`qwen` and `astra` are on the same order of magnitude); the control group's
 `upstream_inference_cost` was also only on the order of 1e-5 USD. In other words, `theosPerRequest` is more like
 a **price tag/upper limit**, not a hard per-request deduction; using it for model pool cost estimation will overestimate by two orders of magnitude.

### Request body shape for `/responses` (new finding)

When `gpt-6-astra` goes through `/deepai_code/responses`, the upstream likewise validates that "it must be the shape sent by DeepAI Code";
if non-compliant, it directly returns 400 `This request is not one DeepAI Code sends.` The minimal usable shape
(consistent with pi's `openai-responses.js`):

```json
{
  "model": "gpt-6-astra",
  "stream": false,
  "store": false,
  "prompt_cache_key": "<32 hex>",
  "input": [
    {"role": "system", "content": "You are a helpful AI assistant. …"},
    {"role": "user", "content": [{"type": "input_text", "text": "…"}]}
  ]
}
```

Key points: **the system script must be placed in `input[0]`** (not the top-level `instructions`), the user's content must
be a `[{type:"input_text",text}]` array, and it must include `store:false` and `prompt_cache_key`.
Sending only `{"model":…,"input":[{"role":"user",…}]}` will be rejected by the 400 above.
