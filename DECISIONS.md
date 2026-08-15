# DECISIONS

What was chosen, what was rejected, why. One section per phase.

---

## Phase 1 — Provider Abstraction & Core Proxy

### Public wire format: mirror OpenAI

Chosen over a bespoke SwitchYard schema. Clients adopt the gateway by changing one base URL and every existing SDK works unmodified — a materially stronger demo than a format nobody speaks. Cost accepted: we inherit OpenAI's warts.

Guardrail: the public wire DTO lives in `proxy/` as a *separate struct* from `provider.Request`. If they ever merge, OpenAI has silently become the internal format and every other adapter gets uglier.

### Error taxonomy: typed `Kind`, stored `Retryable`

Chosen over passing raw HTTP status codes up the stack. Retry, fallback, and the breaker must branch on *meaning*, not on a number whose interpretation differs per provider — status codes would force provider knowledge into `proxy/`.

- **`Retryable` is stored, not derived from `Kind` at read time.** The adapter has read the response body; the retry layer has not. A 429 from a soft rate limit is retryable, a 429 from an exhausted billing quota is not, and both arrive as the same status. `Kind` supplies the default via `DefaultRetryable()`; the adapter overrides when the body says more.
- **Added `KindInvalidRequest`, which the plan omits.** Without it a provider 400 (malformed body, unknown model, context overflow) lands in `KindServerError`, which defaults to retryable — so Phase 6 would retry a request that can never succeed, against every provider in the chain.
- **`Kind` is a named string, not `iota`.** It becomes a Prometheus label in Phase 9 with no conversion, and its zero value is `""` rather than a valid classification, so an unset `Kind` reads as a bug instead of silently meaning "rate limited".

### System prompt stays a `Message`, not a `Request` field

Rejected a top-level `Request.System string`. Anthropic and Gemini want it top-level; OpenAI and Ollama want it in the array. Putting it on the canonical type makes the majority special-case it, so the adapters that need it hoist it themselves. Edge case: multiple system messages, or one out of first position, are concatenated in order.

### `Provider` interface lives in `provider`, not the consumer

Departs from this repo's consumer-defines-the-interface rule. The registry returns a `Provider` and lives in `provider`, so a `proxy.Provider` would create a `provider` ↔ `proxy` import cycle that Go rejects at compile time. Go's implicit satisfaction means `proxy` can still declare a narrower interface later with no adapter changes.

### Streaming types declared in Phase 1, implemented in Phase 2

`StreamReader` and `Chunk` declared now; `Stream` returns `ErrStreamingNotImplemented`. Same reasoning the plan gives for `Ping` on day one — the interface is final, so Phase 2 never reopens an adapter file to change a signature.

### Dev stand-ins: Groq for the OpenAI slot, Gemini for the Anthropic slot

OpenAI/Anthropic slots backed by Groq/Gemini during dev due to cost; interface never assumed a specific vendor, so swapping in real keys before the demo is a config change, not a code change.

Rejected using an OpenAI-compatible free service for *both* slots. Gemini differs from OpenAI in the same places Anthropic does — top-level system instruction, different usage and finish-reason vocabulary — so building it proves the adapters are genuinely decoupled rather than all secretly OpenAI-shaped.

Consequence: `providers.yaml` separates instance `name` (`groq`, `openai`) from adapter `type` (`openai-compatible`). No provider name is hardcoded in Go.

### YAML parser: `goccy/go-yaml`

Chosen over `yaml.v3` (archived upstream, no further fixes) and `sigs.k8s.io/yaml` (pulls in `yaml.v2` transitively anyway). `goccy` reports errors with line, column, and a source excerpt, which is what makes "malformed config prevents startup with a clear message" actually true rather than a bare `yaml: line 6: ...`.

### `enabled` flag on each provider entry

Added to `providers.yaml`, not in the plan. Without it, a provider entry with no key set (OpenAI, Anthropic — no paid key yet) either hard-fails startup or has to be deleted, losing the documentation of intent. A disabled entry is still validated and its model names still reserved, so enabling it later can't introduce a collision that was invisible while it was off.

### Cost stored as integer micro-dollars, converted once at load

YAML holds readable decimals (`0.59`); `internal/config` converts to `int64` micro-dollars exactly once, at load. Phase 4 accumulates over thousands of requests, and repeated float addition drifts — the float exists for one multiplication and never again.

### Upstream auth failure maps to 502, not 401

A rejected upstream credential is reported to the caller as 502, not 401. A 401 says *their* key was rejected, which they can't act on and would waste time rotating a key that was never the problem. From the client's seat, a bad upstream credential is a broken gateway.

### Per-provider timeout applied via context, not `http.Client.Timeout`

Each adapter wraps the caller's `ctx` with `context.WithTimeout(ctx, cfg.Timeout)`, so whichever deadline is sooner wins automatically. `Client.Timeout` would silently override a shorter caller deadline, which Phase 6's "never retry past the caller's deadline" rule depends on.

### Gateway overhead measurement: known limitation on Windows dev machine

`X-Switchyard-Overhead-Ms` = handler time minus `provider.Response.Latency` (the adapter's own measured round trip), so adapter translation work correctly counts as overhead. Verified live against real Groq and Gemini calls. However, Go's monotonic clock on this Windows dev machine has ~529µs granularity — sub-millisecond overhead often reads as `0.000`. Not a code bug, but it means the sub-10ms number can't be credibly measured on Windows; Phase 11's load test must run inside Docker (Linux), where clock resolution is nanosecond-grade.
