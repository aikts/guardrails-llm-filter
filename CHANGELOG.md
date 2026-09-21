# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Standalone HTTP data plane**: guardrails-llm-filter now runs as a standalone service
  that clients call directly (`GUARDRAILS_LISTEN_ADDR`, default `:8080`). It
  masks the request, forwards it to the configured upstream LLM provider itself,
  then demasks the response — no Envoy in the path. Upstream configuration:
  `GUARDRAILS_UPSTREAM_BASE_URL` (required), per-path overrides via
  `GUARDRAILS_UPSTREAM_PATH_BASE_URLS`, and connection-pool / timeout / TLS
  tuning via the other `GUARDRAILS_UPSTREAM_*` variables.
- HTTP health endpoints `/healthz` (liveness) and `/readyz` (readiness) on the
  data-plane port, replacing the gRPC health service.
- Masking-outcome response headers on the standalone data plane, in the
  ext_proc variant's format and under its conditions: a response to a masked
  request carries `x-guardrails-data-types-triggered`, and with
  `GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES=true` also
  `x-guardrails-triggered-rules` plus the new `x-guardrails-replacement-counts`
  (`rule_id=count` for the same rules: distinct values each rule replaced; name
  set by `GUARDRAILS_HEADERS_REPLACEMENT_COUNTS_HEADER`). They are set before
  the body, so SSE responses carry them too. The standalone data plane
  previously emitted none of these headers, although the configuration and docs
  described them. Same-named headers from the upstream are not relayed: the
  data-types one never, the rule ones while exposure is on.
- Configurable graceful shutdown. `GUARDRAILS_SHUTDOWN_DRAIN_PERIOD` (default
  `0s`) adds a drain phase on SIGTERM/SIGINT: `/readyz` turns 503 and the data
  plane stops keeping connections alive but keeps serving, so the instance
  leaves load balancing before its listener closes (clients are no longer
  refused mid-rollout). `GUARDRAILS_SHUTDOWN_TIMEOUT` (default `10s`, the
  previously hard-coded budget) bounds how long in-flight requests, long SSE
  streams included, may take to finish afterwards.
- Optional keyword pre-filter for the sensitive scanner
  (`GUARDRAILS_KEYWORD_PREFILTER_ENABLED`, off by default): skips a rule's regex
  when none of its `keywords` is present in the text. It is recall-preserving —
  applied only to rules whose regex provably requires a keyword in every match
  (verified at compile time via a `regexp/syntax` analysis), so detections never
  change; rules that are not eligible are always scanned and listed in a startup
  log. Benchmarks (`tests/benchmarks/keyword_prefilter`) show a ~2–3× scan
  speedup on representative request bodies.
- Anthropic `/v1/messages`: the top-level `system` prompt is now masked (it was
  previously left untouched), closing a path where PII/secrets in `system`
  reached the model unmasked.
- Custom-rule limits: `GUARDRAILS_RULES_MAX_CUSTOM` (default 500) and
  `GUARDRAILS_RULES_MAX_PATTERN_LEN` (default 4096) bound how many custom rules
  and how large a regex the configuration API accepts, protecting the request
  hot path. Exceeding them returns 409 / 400 respectively; `0` disables a limit.
- Metric `unknown_format_passthrough_total`: counts response bodies passed
  through unchanged because their API format was unknown (fail-open).
- Metric `unguarded_path_passthrough_total`: counts requests passed through
  unmasked because their path matched no guarded LLM path. The closed-source
  origin rejected such requests with HTTP 400; the OSS verdict model
  (mask/pass — never block) forwards them instead, and this counter is the
  operator's signal that the gateway receives traffic on paths beyond
  `GUARDRAILS_PATHS`.

### Changed

- The data path is now standalone HTTP instead of an Envoy `ext_proc` gRPC
  sidecar: a single handler masks the request, forwards it to the upstream, and
  demasks the response on one replica, so masking state is held in-process and
  no store round-trip is needed on the hot path. The `x-guardrails-data-types`
  override header is consumed by the service and not forwarded upstream.
- SSE demask hot path: the placeholder scanner now resolves compiled rules once
  per stream and scans small fragments sequentially instead of spawning a
  goroutine fan-out per token-sized chunk.
- An unknown/empty API format on a streaming response now passes the stream
  through unchanged (with `unknown_format_passthrough_total`), matching the
  full-body policy, instead of routing it into the chat-completions processor
  where named-event streams would leak placeholders undemasked.

### Fixed

- `/v1/responses` and `/v1/messages` streams: a frame the processor rewrites
  no longer goes out with an empty `event: ` line. When the upstream sends
  unnamed events (LiteLLM on `/v1/responses` sends data lines only), the
  rebuilt `output_text.done`, `content_part.done`, `output_item.done` and
  `response.completed` frames got `event: ` with no name, which a client
  dispatching on the event name does not take for a missing line. The event
  line is now left out when the name is empty, and a frame made up on flush
  follows the stream: unnamed in an unnamed stream, so a client listening for
  the default `message` event still gets it.
- `/v1/responses` streams: reasoning summaries are demasked.
  `response.reasoning_summary_text.delta`, `.done` and
  `response.reasoning_summary_part.done` were relayed verbatim on the
  assumption that a summary cannot hold placeholders — but a summary
  paraphrases the prompt, and LiteLLM streams a chat model's
  `reasoning_content` on `/v1/responses` as exactly these events. They are
  now handled like `reasoning_text`: a streaming demasker per
  (`output_index`, `summary_index`), flushed on the done event, and a fresh
  demask of the full text the done events repeat. The snapshots LiteLLM
  sends for such a model are demasked too: a `content_part.done` part that
  carries the reasoning as `part.reasoning`, and a reasoning output item
  whose content parts are typed `output_text` (in `response.completed` and in
  full, non-streamed responses) — every part of a reasoning item is now
  demasked whatever its type.
- `/v1/chat/completions` streams: a reasoning model's chain-of-thought no
  longer reaches the client with placeholders in it. The SSE processor knew
  only `delta.reasoning`, so a delta carrying `delta.reasoning_content`
  (DeepSeek, LiteLLM) or OpenRouter's `delta.reasoning_details` was relayed
  verbatim, placeholders included — and when the same delta also had
  `reasoning` or `content`, those fields were dropped from the rebuilt frame.
  Both are now demasked with their own streaming demaskers (`text`/`summary`
  of a `reasoning_details` entry; its signature and encrypted data pass
  through), and the reasoning fields of one delta go out in one frame, so a
  client reading `reasoning_content or reasoning` does not show the text
  twice. A signed `reasoning_details` entry releases the text held for it
  first. Non-streamed responses now demask `reasoning_details` too, and the
  copies of the reasoning LiteLLM keeps under
  `message.provider_specific_fields` (OpenRouter's `reasoning` and
  `reasoning_details`), which reached the client with placeholders.
- A request body that repeats a JSON object key no longer slips past masking.
  The extractors read the first occurrence of a key (gjson), while typical
  upstreams keep the last one, so
  `{"role":"user","content":"clean","content":"<PII>"}` was scanned as `clean`
  and the model got the PII — likewise for a repeated `messages`, `input` or
  part `type`. On a guarded path such a body is now collapsed to one value per
  key (the last, in the key's first position) before it is scanned, and in
  enforce mode the collapsed body is what gets forwarded; detect mode still
  forwards the client's bytes. Only the objects involved are re-serialized.
  New metric `duplicate_keys_collapsed_total` counts these requests.
- The data plane no longer sends a request to the upstream twice. With a
  client `Idempotency-Key` / `X-Idempotency-Key` header, net/http replayed a
  request whose reused keep-alive connection broke after the request was
  written — for an LLM call, a second model invocation (and charge) while the
  first could still be running. The upstream request is now built without
  `GetBody`, as `httputil.ReverseProxy` does: the transport retries nothing,
  so such failures reach the client as 502, and a 307/308 from the upstream is
  relayed instead of followed.
- Anthropic `/v1/messages` requests: `tool_use.input` is now masked per decoded
  string leaf. Previously the raw JSON object text was regex-scanned: PII
  containing quotes/backslashes/escapes could be missed entirely, and when a
  match broke the object's JSON validity the original was sent to the model
  unmasked while metrics, masking state and the audit record claimed it was
  masked. Metrics/state/audit are now recorded only after the body is patched.
- Non-streamed `/v1/messages` responses are no longer round-tripped through the
  Anthropic SDK's typed `Message` (which fabricated `stop_details`/`container`
  objects and union zero values, coerced `stop_sequence: null` to `""`, and
  dropped unmodeled fields on every masked response); demasking now extracts
  and patches fields in place like the other two formats. The
  `anthropic-sdk-go` dependency is gone.
- Anthropic SSE: restored originals inserted into `input_json_delta` fragments
  are now JSON-escaped, so an original containing a quote or backslash no
  longer corrupts the tool input the client accumulates (stream and non-stream
  tool-input demasking now agree).
- chat/completions SSE: non-demaskable frames (refusal/audio/annotations/
  role-only/empty keepalive deltas) no longer force-flush the choice demaskers —
  a placeholder split across such a frame was emitted as raw fragments and
  never restored.
- chat/completions SSE: a tool call whose arguments never arrive (parameterless
  calls as streamed by some OpenAI-compatible backends) is no longer dropped;
  its id/name announcement frame is forwarded.
- chat/completions SSE: providers attaching `usage` to every content chunk
  (vLLM continuous usage stats) no longer grow the metadata buffer for the
  whole stream and dump stale usage snapshots at stream end; usage frames are
  now emitted in their stream position, preserving the per-chunk cadence the
  client requested.
- Responses SSE: `sequence_number` is now strictly monotonic across synthetic
  flush frames — the synthetic claims the next number and subsequent real
  frames are renumbered past it (previously the client saw duplicates and
  out-of-order numbers right at the flush point).
- Non-streamed chat/completions responses no longer lose unmodeled fields
  (`system_fingerprint`, `service_tier`, `logprobs`, `refusal`, annotations) or
  gain a spurious empty `usage` object when a response is demasked; demasking
  now patches fields in place instead of round-tripping a partial struct.
- Anthropic SSE: a held text tail is no longer dropped when a `text_delta`
  arrives without a preceding `content_block_start` (or on a delta/block type
  desync) — flushing now keys off the live demaskers, not the recorded block
  type.
- chat/completions SSE: frames carrying only `refusal`, `audio` or a role-only
  opening delta are forwarded instead of being silently dropped.
- Responses SSE tool-call `arguments` now use the same structural demask as the
  full-body path, so a restored secret containing a quote/backslash stays valid
  JSON instead of leaking a placeholder.
- Responses SSE synthetic delta frames now include `item_id` and
  `sequence_number`, which strict SDKs require.
- Unknown/empty response format now passes the body through unchanged instead of
  rewriting it into an empty chat-completions skeleton.
- chat/completions SSE frames are serialized without HTML-escaping `<`, `>`,
  `&`, matching the other dialects and preserving placeholder markers.

### Removed

- The Envoy `ext_proc` gRPC controller, the gRPC and gRPC-health servers, the
  `GUARDRAILS_GRPC_ADDR` / `GUARDRAILS_GRPC_SECURE` / `GUARDRAILS_HEALTH_PORT`
  and masking-state-key (`GUARDRAILS_STATE_KEY_SALT` /
  `GUARDRAILS_STATE_DELETE_ON_CLOSE`) configuration, and the `go-control-plane`
  and gRPC-middleware dependencies. The Envoy sidecar packaging now lives in the
  sibling project `guardrails-llm-filter-extproc`.
- Legacy `/v1/completions` (OpenAI text completions) is no longer supported;
  use `/v1/chat/completions`. Prompt masking and `choices[].text` demasking for
  that endpoint were dropped in the OSS refactor and are not being restored.

## [0.1.0] - 2026-07-06

Initial public release. An Envoy ext_proc v3 gRPC service that masks
PII/secrets in LLM request bodies and demasks them in responses (including SSE
streams). Verdict model is mask/pass only; the data path is fail-open.

### Added

- ext_proc data path: request-body masking and response demasking for OpenAI
  (`/v1/chat/completions`, `/v1/responses`) and Anthropic
  (`/v1/messages`) formats, including token-by-token SSE demasking.
- Configurable request paths via `GUARDRAILS_PATHS`.
- Regex + validator rule engine (~260 built-in rules) with an immutable,
  atomically reloadable registry.
- Detect/shadow mode (`GUARDRAILS_MODE=detect`): scan and record metrics/audit
  without mutating traffic.
- Pluggable persistence (`in_memory` / `redis` / `postgres`) for masking state,
  custom rules, settings, and the audit trail, with a shared conformance suite.
- Optional at-rest encryption of masking state (AES-256-GCM,
  `GUARDRAILS_STORE_ENCRYPTION_*`).
- HTTP configuration API (rules CRUD, per-rule enable/disable via
  `PATCH /v1/rules/{id}`, global settings, audit query) with optional bearer
  auth and an OpenAPI spec.
- Global settings with a narrow-only per-request override header.
- Optional audit trail with a non-blocking, fail-open recorder.
- Prometheus metrics + alert rules and a Grafana dashboard.
- Packaging: distroless Docker image, Kubernetes manifests, and an
  `examples/quickstart` end-to-end demo (Envoy + mock LLM).

[Unreleased]: https://github.com/cloud-ru-tech/guardrails-llm-filter/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/cloud-ru-tech/guardrails-llm-filter/releases/tag/v0.1.0
