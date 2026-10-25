# llm-otel-go

[![CI](https://github.com/neerajsinghi/llm-otel-go/actions/workflows/ci.yml/badge.svg)](https://github.com/neerajsinghi/llm-otel-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/neerajsinghi/llm-otel-go.svg)](https://pkg.go.dev/github.com/neerajsinghi/llm-otel-go)

**OpenTelemetry tracing and metrics for LLM API calls in Go, as a single `http.RoundTripper`.**

Wrap the `*http.Client` you already give the OpenAI or Anthropic SDK (or your own
client) and every call gets a span that follows the
[OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/),
plus token, latency and estimated-cost metrics. Streaming responses keep streaming.

```go
client := &http.Client{Transport: llmotel.NewTransport(http.DefaultTransport)}
```

## Why

LLM calls are slow, metered and fail in new ways: rate limits, overloads,
truncated outputs, runaway token counts. You want to see them in the same
traces and dashboards as the rest of your service. Most LLM observability tools
are Python-first or need a proxy or vendor SDK. In Go, the HTTP client is the
natural seam:

- **SDK-agnostic.** Works with `openai-go`, `anthropic-sdk-go`, `go-openai`,
  LangChainGo or hand-written `net/http` code, because they all accept an
  `*http.Client`.
- **No proxy, no vendor.** Plain OpenTelemetry. Send it to Jaeger, Tempo,
  Honeycomb, Datadog, Grafana Cloud or anything else that speaks OTLP.
- **Streaming-safe.** The response body is teed as *you* read it, never
  buffered ahead of you. Usage is pulled from the final SSE events.
- **Private by default.** Prompts and completions are never recorded unless you
  opt in, and then only through your redaction hook.

## Features

- Spans for OpenAI-compatible `/v1/chat/completions`, `/v1/embeddings`,
  `/v1/responses` and Anthropic `/v1/messages` (path-suffix matching, so
  gateways, Azure OpenAI, Groq, Ollama, vLLM and other compatible servers work).
- GenAI semconv attributes: `gen_ai.provider.name` (and legacy `gen_ai.system`),
  `gen_ai.operation.name`, request/response model, response id, finish reasons,
  input/output tokens, cache read/write tokens, request parameters.
- Usage from JSON responses and from SSE streams (OpenAI chat chunks, OpenAI
  Responses events, Anthropic message events).
- Error handling: HTTP status, provider error type (`rate_limit_error`,
  `invalid_request_error`, ...), in-stream error events, interrupted streams,
  transport errors, all as `error.type` with span status `Error`.
- Retries: optional built-in retry with exponential backoff and `Retry-After`,
  recorded as span events and `http.request.resend_count`. SDK retries are
  picked up from the `X-Stainless-Retry-Count` header.
- Metrics: `gen_ai.client.token.usage` and `gen_ai.client.operation.duration`
  histograms (semconv bucket boundaries), and an `llm.client.cost` counter from
  your price table.
- Optional prompt/completion capture as span events, with a redaction hook and
  a size cap.
- Optional injection of `stream_options.include_usage` so OpenAI streams report
  usage.
- A docker-compose stack (Collector, Jaeger, Prometheus, Grafana) with a
  provisioned dashboard and a demo that needs no API key.

## Install

```sh
go get github.com/neerajsinghi/llm-otel-go
```

Requires Go 1.26+ (the current OpenTelemetry Go SDK's minimum).

## Quickstart

Set up OpenTelemetry as usual (exporters, `TracerProvider`, `MeterProvider`),
then wrap your HTTP client:

```go
import (
	"net/http"

	llmotel "github.com/neerajsinghi/llm-otel-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

httpClient := &http.Client{
	Transport: llmotel.NewTransport(http.DefaultTransport,
		llmotel.WithPrices(llmotel.ExamplePrices()), // optional: cost metric
	),
}
client := openai.NewClient(option.WithHTTPClient(httpClient))
```

Anthropic is the same:

```go
import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

client := anthropic.NewClient(option.WithHTTPClient(httpClient))
```

Pass a request context that carries your current span and the LLM span becomes
its child.

**Read or close every response body.** The span ends when the body reaches EOF
or is closed, so that streaming calls are timed end to end. SDKs do this for
you; hand-written code should `defer resp.Body.Close()`.

## Usage

### Options

| Option | Default | Effect |
|---|---|---|
| `WithTracerProvider(tp)` | global | TracerProvider for spans |
| `WithMeterProvider(mp)` | global | MeterProvider for metrics |
| `WithPrices(table)` | none | Enables `llm.client.cost` and `llm.usage.cost_usd` |
| `WithContentCapture(bool)` | `false` | Records prompts/completions as span events |
| `WithRedactor(fn)` | none | Rewrites captured content; return `""` to drop it |
| `WithMaxContentLength(n)` | 16 KiB | Truncates each captured event (UTF-8 safe) |
| `WithRetry(RetryPolicy{...})` | off | Built-in retries on 408/409/429/5xx/529 and network errors |
| `WithStreamUsageInjection(bool)` | `false` | Adds `stream_options.include_usage` to OpenAI streaming chat requests |
| `WithLegacySystemAttribute(bool)` | `true` | Also emits deprecated `gen_ai.system` |

Helpers: `NewClient(opts...)` returns a ready `*http.Client`;
`WrapClient(c, opts...)` returns a copy of `c` with its transport wrapped.

### Cost estimation

```go
prices := llmotel.PriceTable{
	"gpt-4o-mini":     {InputPerMillion: 0.15, OutputPerMillion: 0.60},
	"claude-sonnet-4": {InputPerMillion: 3.00, OutputPerMillion: 15.00},
}
t := llmotel.NewTransport(nil, llmotel.WithPrices(prices))
```

Keys match exactly or as the longest prefix, so `claude-sonnet-4` also prices
`claude-sonnet-4-20250514`. The response model is tried first, then the request
model. Tables can be loaded from JSON with `llmotel.LoadPrices`.

> **The price table is yours to maintain.** `ExamplePrices()` is a small,
> illustrative snapshot of list prices that this library does not keep
> current. It ignores batch and caching discounts, long-context pricing and
> negotiated rates. Treat the cost metric as an estimate for trends and
> alerting, not as billing data.

### Capturing prompts and completions

Off by default. When on, the transport adds a `gen_ai.content.prompt` event
(the request's `system`/`instructions`/`messages`/`input` as compact JSON) and
a `gen_ai.content.completion` event (the generated text, streamed or not).

```go
llmotel.NewTransport(nil,
	llmotel.WithContentCapture(true),
	llmotel.WithRedactor(func(kind llmotel.ContentKind, s string) string {
		return emailRE.ReplaceAllString(s, "[email]")
	}),
)
```

Only turn this on where your data handling policy allows prompts to sit in
your tracing backend.

### Streaming usage for OpenAI

OpenAI chat completion streams only include token usage when the request sets
`stream_options.include_usage`. Set it yourself, or enable
`WithStreamUsageInjection(true)`. Anthropic and Responses API streams always
report usage.

## Attribute reference

Span name: `{gen_ai.operation.name} {gen_ai.request.model}`, e.g. `chat gpt-4o-mini`. Span kind: `CLIENT`.

| Attribute | Type | Span | Metrics | Notes |
|---|---|:-:|:-:|---|
| `gen_ai.operation.name` | string | ✓ | ✓ | `chat` or `embeddings` |
| `gen_ai.provider.name` | string | ✓ | ✓ | `openai`, `anthropic`, `azure.ai.openai`, `groq`, `deepseek`, `mistral_ai`, ... from host, else from wire format |
| `gen_ai.system` | string | ✓ | | Deprecated alias, on by default |
| `gen_ai.request.model` | string | ✓ | ✓ | |
| `gen_ai.request.max_tokens` | int | ✓ | | `max_tokens`, `max_completion_tokens` or `max_output_tokens` |
| `gen_ai.request.temperature` | double | ✓ | | |
| `gen_ai.request.top_p` | double | ✓ | | |
| `gen_ai.request.stream` | bool | ✓ | | Only set when `true` |
| `gen_ai.response.model` | string | ✓ | ✓ | |
| `gen_ai.response.id` | string | ✓ | | |
| `gen_ai.response.finish_reasons` | string[] | ✓ | | e.g. `["stop"]`, `["end_turn"]`, `["max_tokens"]` |
| `gen_ai.usage.input_tokens` | int | ✓ | | Anthropic: includes cache read + write tokens |
| `gen_ai.usage.output_tokens` | int | ✓ | | Not set for embeddings |
| `gen_ai.usage.cache_read.input_tokens` | int | ✓ | | When reported |
| `gen_ai.usage.cache_creation.input_tokens` | int | ✓ | | Anthropic, when reported |
| `gen_ai.token.type` | string | | ✓ | `input` / `output` (token usage histogram only) |
| `server.address`, `server.port` | string, int | ✓ | ✓ | |
| `http.response.status_code` | int | ✓ | | |
| `http.request.resend_count` | int | ✓ | | Built-in retries, or SDK retry header |
| `error.type` | string | ✓ | ✓ | Provider error type, HTTP status, `timeout`, `canceled`, `stream_interrupted`, `_OTHER` |
| `llm.api` | string | ✓ | | `openai.chat_completions`, `openai.embeddings`, `openai.responses`, `anthropic.messages` |
| `llm.usage.cost_usd` | double | ✓ | | Only with `WithPrices` |

Span events: `llm.retry` (`llm.retry.attempt`, `llm.retry.reason`,
`llm.retry.delay_ms`), `gen_ai.content.prompt` (`gen_ai.prompt`),
`gen_ai.content.completion` (`gen_ai.completion`), and `exception` for
transport and stream read errors.

The full URL is deliberately not recorded: some providers accept API keys as
query parameters. Request and response headers are not recorded either.

| Metric | Instrument | Unit | Prometheus name |
|---|---|---|---|
| `gen_ai.client.token.usage` | Int64 histogram | `{token}` | `gen_ai_client_token_usage_*` |
| `gen_ai.client.operation.duration` | Float64 histogram | `s` | `gen_ai_client_operation_duration_seconds_*` |
| `llm.client.cost` | Float64 counter | `USD` | `llm_client_cost_USD_total` |

## Architecture

```
 your code / SDK
       │  *http.Client.Do(req)
       ▼
┌──────────────────────── llmotel.Transport ────────────────────────┐
│ 1. DetectAPI(method, path) ── unknown ──────────────► pass through │
│ 2. read request body → model, stream, params, (prompt)            │
│ 3. start CLIENT span "chat gpt-4o"                                │
│ 4. send via base RoundTripper ◄─┐ optional retry + backoff        │
│                                 └─ 429 / 5xx / net error          │
│ 5. wrap resp.Body:                                                │
│      JSON   ──► jsonSink (buffer ≤ 8 MiB, parse at EOF)           │
│      SSE    ──► sseSink  (line parser, keeps only current line)   │
└───────────────────────────────┬───────────────────────────────────┘
                                │ caller reads body (bytes unchanged)
                                ▼
                 EOF / Close / read error (exactly once)
                                │
       usage, model, finish reasons, error type, (completion)
                                │
       ┌────────────────────────┼─────────────────────────┐
       ▼                        ▼                         ▼
  span attributes         token + duration          cost counter
  + span.End()            histograms                (PriceTable)
```

## Example stack

`examples/` contains a demo program and a full local stack:

```sh
cd examples
docker compose up --build
```

- Grafana at <http://localhost:3000> opens the provisioned **LLM calls**
  dashboard: calls, error rate, p50/p95/p99 latency, tokens per second,
  tokens per call, estimated cost.
- Jaeger at <http://localhost:16686>, service `llm-otel-demo`.
- Prometheus at <http://localhost:9090>.

The demo talks to an in-process fake LLM server (OpenAI and Anthropic formats,
streaming and not, occasional 429s), so it needs no keys. To run it on the host
against the stack, or against the real APIs:

```sh
cd examples
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run ./demo
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 OPENAI_API_KEY=... go run ./demo -real -n 5
```

## Roadmap

- Tool call attributes and `gen_ai.tool.*` spans.
- Time-to-first-token metric for streaming calls.
- More wire formats: Gemini `generateContent`, AWS Bedrock Converse, legacy
  OpenAI `/completions`.
- The newer GenAI content convention (`gen_ai.input.messages` /
  `gen_ai.output.messages`) as an alternative to the event-based capture.
- Cache-aware pricing (separate prices for cached input tokens).
- Optional W3C trace context propagation to self-hosted gateways.

## License

[MIT](LICENSE) © 2026 Neeraj Singhi
