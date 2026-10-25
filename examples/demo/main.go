// Command demo generates LLM telemetry with llmotel and exports it over OTLP.
//
// By default it starts an in-process fake LLM server that speaks the OpenAI
// chat completions and Anthropic messages wire formats (streaming and not,
// with occasional rate-limit errors), so the whole observability stack can be
// tried without API keys or spend.
//
// With -real it calls api.openai.com and/or api.anthropic.com using
// OPENAI_API_KEY / ANTHROPIC_API_KEY instead. That costs real money.
//
// The OTLP endpoint is taken from the standard OTEL_EXPORTER_OTLP_ENDPOINT
// environment variable, e.g. http://localhost:4318 (an http:// URL disables TLS).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"time"

	llmotel "github.com/neerajsinghi/llm-otel-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func main() {
	var (
		useReal  = flag.Bool("real", false, "call the real OpenAI/Anthropic APIs (needs API keys; costs money)")
		interval = flag.Duration("interval", time.Second, "delay between calls")
		count    = flag.Int("n", 0, "number of calls to make (0 = until interrupted)")
		capture  = flag.Bool("capture", false, "capture prompts and completions as span events")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shutdown, err := setupOTel(ctx)
	if err != nil {
		log.Fatalf("otel setup: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(c); err != nil {
			log.Printf("otel shutdown: %v", err)
		}
	}()

	client := &http.Client{
		Timeout: 2 * time.Minute,
		Transport: llmotel.NewTransport(http.DefaultTransport,
			llmotel.WithPrices(llmotel.ExamplePrices()),
			llmotel.WithRetry(llmotel.RetryPolicy{MaxRetries: 2, BaseDelay: 200 * time.Millisecond}),
			llmotel.WithStreamUsageInjection(true),
			llmotel.WithContentCapture(*capture),
		),
	}

	targets := mockTargets()
	if *useReal {
		targets = realTargets()
		if len(targets) == 0 {
			log.Fatal("-real needs OPENAI_API_KEY and/or ANTHROPIC_API_KEY")
		}
	} else {
		srv := httptest.NewServer(fakeLLM())
		defer srv.Close()
		for i := range targets {
			targets[i].baseURL = srv.URL
		}
	}

	tracer := otel.Tracer("llm-otel-demo")
	for i := 0; *count == 0 || i < *count; i++ {
		t := targets[rand.IntN(len(targets))]
		stream := rand.IntN(2) == 0
		cctx, span := tracer.Start(ctx, "answer-question")
		err := t.call(cctx, client, stream)
		span.End()
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("%s (%s, stream=%v): %v", t.name, t.model, stream, err)
		} else if err == nil {
			log.Printf("%s %s stream=%v ok", t.name, t.model, stream)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

func setupOTel(ctx context.Context) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("llm-otel-demo"),
	))
	if err != nil {
		return nil, err
	}
	te, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	me, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(5*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

type target struct {
	name    string
	baseURL string
	model   string
	headers map[string]string
}

func mockTargets() []target {
	return []target{
		{name: "openai", model: "gpt-4o-mini"},
		{name: "openai", model: "gpt-4.1"},
		{name: "anthropic", model: "claude-sonnet-4-20250514"},
		{name: "anthropic", model: "claude-haiku-4-5"},
	}
}

func realTargets() []target {
	var ts []target
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		ts = append(ts, target{name: "openai", baseURL: "https://api.openai.com", model: "gpt-4o-mini",
			headers: map[string]string{"Authorization": "Bearer " + k}})
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		ts = append(ts, target{name: "anthropic", baseURL: "https://api.anthropic.com", model: "claude-haiku-4-5",
			headers: map[string]string{"x-api-key": k, "anthropic-version": "2023-06-01"}})
	}
	return ts
}

var questions = []string{
	"Explain goroutines in one sentence.",
	"What is a span in OpenTelemetry?",
	"Write a haiku about latency.",
	"Why are histograms better than averages?",
}

func (t target) call(ctx context.Context, c *http.Client, stream bool) error {
	q := questions[rand.IntN(len(questions))]
	var path, body string
	switch t.name {
	case "anthropic":
		path = "/v1/messages"
		body = fmt.Sprintf(`{"model":%q,"max_tokens":200,"stream":%v,"messages":[{"role":"user","content":%q}]}`, t.model, stream, q)
	default:
		path = "/v1/chat/completions"
		body = fmt.Sprintf(`{"model":%q,"max_tokens":200,"stream":%v,"messages":[{"role":"user","content":%q}]}`, t.model, stream, q)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Read the body to completion: the span ends when the body is drained.
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// fakeLLM serves plausible OpenAI and Anthropic responses with random
// latency, token counts and occasional 429s.
func fakeLLM() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if !decode(w, r, &req) || maybeRateLimit(w, `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`) {
			return
		}
		in, out := 10+rand.IntN(300), 20+rand.IntN(400)
		words := strings.Fields("Goroutines are lightweight threads managed by the Go runtime.")
		if !req.Stream {
			sleep(out)
			writeJSON(w, fmt.Sprintf(`{"id":"chatcmpl-%d","object":"chat.completion","model":"%s-2024-07-18","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
				rand.Int(), req.Model, strings.Join(words, " "), in, out, in+out))
			return
		}
		sse(w, func(emit func(string)) {
			id := rand.Int()
			for _, wd := range words {
				sleep(out / len(words))
				emit(fmt.Sprintf(`data: {"id":"chatcmpl-%d","model":"%s-2024-07-18","choices":[{"index":0,"delta":{"content":%q}}]}`, id, req.Model, wd+" "))
			}
			emit(fmt.Sprintf(`data: {"id":"chatcmpl-%d","model":"%s-2024-07-18","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, id, req.Model))
			emit(fmt.Sprintf(`data: {"id":"chatcmpl-%d","model":"%s-2024-07-18","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, id, req.Model, in, out))
			emit("data: [DONE]")
		})
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if !decode(w, r, &req) || maybeRateLimit(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`) {
			return
		}
		in, out := 10+rand.IntN(300), 20+rand.IntN(400)
		stop := "end_turn"
		if rand.IntN(10) == 0 {
			stop = "max_tokens"
		}
		text := "A span is one timed operation in a trace."
		if !req.Stream {
			sleep(out)
			writeJSON(w, fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":%q,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
				rand.Int(), req.Model, text, stop, in, out))
			return
		}
		sse(w, func(emit func(string)) {
			emit(fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_%d\",\"model\":%q,\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}}", rand.Int(), req.Model, in))
			for _, wd := range strings.Fields(text) {
				sleep(out / 10)
				emit(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}", wd+" "))
			}
			emit(fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":%d}}", stop, out))
			emit("event: message_stop\ndata: {\"type\":\"message_stop\"}")
		})
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, v); err != nil {
		http.Error(w, `{"error":{"type":"invalid_request_error"}}`, http.StatusBadRequest)
		return false
	}
	return true
}

func maybeRateLimit(w http.ResponseWriter, body string) bool {
	if rand.IntN(20) != 0 {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, body)
	return true
}

func writeJSON(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, s)
}

func sse(w http.ResponseWriter, fn func(emit func(string))) {
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	fn(func(ev string) {
		_, _ = io.WriteString(w, ev+"\n\n")
		if f != nil {
			f.Flush()
		}
	})
}

// sleep simulates generation time of roughly 2ms per output token.
func sleep(tokens int) { time.Sleep(time.Duration(tokens) * 2 * time.Millisecond) }
