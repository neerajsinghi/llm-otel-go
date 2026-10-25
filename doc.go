// Package llmotel instruments LLM API calls made over net/http with
// OpenTelemetry traces and metrics.
//
// The entry point is [NewTransport], an [http.RoundTripper] wrapper. Plug it
// into the *http.Client you hand to any OpenAI-compatible or Anthropic SDK (or
// your own hand-rolled client) and every call to a recognised endpoint gets:
//
//   - a CLIENT span named "{gen_ai.operation.name} {gen_ai.request.model}",
//     carrying the OpenTelemetry GenAI semantic convention attributes
//     (provider, request/response model, token usage, finish reasons, ...);
//   - gen_ai.client.token.usage and gen_ai.client.operation.duration
//     histograms;
//   - an optional llm.client.cost counter computed from a user-maintained
//     [PriceTable].
//
// Recognised endpoints are OpenAI-style /chat/completions, /embeddings and
// /responses, and Anthropic-style /messages. Other requests pass through
// untouched.
//
// Response bodies are never buffered ahead of the caller: the transport tees
// the body as the caller reads it, so streaming (Server-Sent Events) responses
// keep streaming. Usage is extracted from the JSON document or from the final
// stream events once the body reaches EOF or is closed, and the span ends at
// that point, so its duration covers the whole stream.
//
// Capturing prompt and completion text is OFF by default. Enable it with
// [WithContentCapture] and scrub it with [WithRedactor].
package llmotel
