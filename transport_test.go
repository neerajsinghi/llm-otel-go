package llmotel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
)

func TestNonStreamingSpanAttributes(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		req      string
		resp     string
		spanName string
		want     map[attribute.Key]any
		absent   []attribute.Key
	}{
		{
			name:     "openai chat completions",
			path:     "/v1/chat/completions",
			req:      `{"model":"gpt-4o-mini","max_tokens":256,"temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`,
			resp:     `{"id":"chatcmpl-1","model":"gpt-4o-mini-2024-07-18","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`,
			spanName: "chat gpt-4o-mini",
			want: map[attribute.Key]any{
				AttrOperationName:      "chat",
				AttrProviderName:       "openai",
				AttrSystem:             "openai",
				AttrRequestModel:       "gpt-4o-mini",
				AttrRequestMaxTokens:   256,
				AttrRequestTemperature: 0.2,
				AttrResponseModel:      "gpt-4o-mini-2024-07-18",
				AttrResponseID:         "chatcmpl-1",
				AttrFinishReasons:      []string{"stop"},
				AttrInputTokens:        11,
				AttrOutputTokens:       7,
				AttrCacheReadTokens:    3,
				AttrHTTPStatusCode:     200,
				AttrServerAddress:      "127.0.0.1",
				AttrAPI:                "openai.chat_completions",
			},
			absent: []attribute.Key{AttrErrorType, AttrCostUSD, AttrResendCount},
		},
		{
			name:     "openai embeddings",
			path:     "/v1/embeddings",
			req:      `{"model":"text-embedding-3-small","input":["a","b"]}`,
			resp:     `{"object":"list","model":"text-embedding-3-small","data":[],"usage":{"prompt_tokens":5,"total_tokens":5}}`,
			spanName: "embeddings text-embedding-3-small",
			want: map[attribute.Key]any{
				AttrOperationName: "embeddings",
				AttrInputTokens:   5,
				AttrResponseModel: "text-embedding-3-small",
			},
			absent: []attribute.Key{AttrOutputTokens, AttrFinishReasons},
		},
		{
			name:     "openai responses",
			path:     "/v1/responses",
			req:      `{"model":"gpt-4.1","input":"hi","max_output_tokens":100}`,
			resp:     `{"id":"resp_1","model":"gpt-4.1-2025-04-14","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hey"}]}],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}`,
			spanName: "chat gpt-4.1",
			want: map[attribute.Key]any{
				AttrOperationName:    "chat",
				AttrRequestMaxTokens: 100,
				AttrResponseID:       "resp_1",
				AttrFinishReasons:    []string{"stop"},
				AttrInputTokens:      9,
				AttrOutputTokens:     4,
				AttrAPI:              "openai.responses",
			},
		},
		{
			name:     "anthropic messages",
			path:     "/v1/messages",
			req:      `{"model":"claude-sonnet-4-20250514","max_tokens":1024,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`,
			resp:     `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":5,"output_tokens":3}}`,
			spanName: "chat claude-sonnet-4-20250514",
			want: map[attribute.Key]any{
				AttrOperationName:       "chat",
				AttrProviderName:        "anthropic",
				AttrRequestModel:        "claude-sonnet-4-20250514",
				AttrRequestMaxTokens:    1024,
				AttrResponseModel:       "claude-sonnet-4-20250514",
				AttrResponseID:          "msg_1",
				AttrFinishReasons:       []string{"end_turn"},
				AttrInputTokens:         35, // 10 uncached + 20 cache read + 5 cache write
				AttrOutputTokens:        3,
				AttrCacheReadTokens:     20,
				AttrCacheCreationTokens: 5,
				AttrAPI:                 "anthropic.messages",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, jsonHandler(200, tt.resp))
			_, got := h.post(tt.path, tt.req)
			if got != tt.resp {
				t.Fatalf("caller saw modified body:\n got %s\nwant %s", got, tt.resp)
			}
			s := h.onlySpan()
			if s.Name != tt.spanName {
				t.Errorf("span name = %q, want %q", s.Name, tt.spanName)
			}
			if s.SpanKind != trace.SpanKindClient {
				t.Errorf("span kind = %v, want client", s.SpanKind)
			}
			if s.Status.Code == codes.Error {
				t.Errorf("unexpected error status %v", s.Status)
			}
			checkAttrs(t, s.Attributes, tt.want)
			m := attrMap(s.Attributes)
			for _, k := range tt.absent {
				if _, ok := m[k]; ok {
					t.Errorf("unexpected attribute %s", k)
				}
			}
		})
	}
}

func TestStreamingUsageExtraction(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		req    string
		events []string
		want   map[attribute.Key]any
	}{
		{
			name: "openai chat with include_usage",
			path: "/v1/chat/completions",
			req:  `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[]}`,
			events: []string{
				"data: {\"id\":\"c1\",\"model\":\"gpt-4o-2024-08-06\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n",
				"data: {\"id\":\"c1\",\"model\":\"gpt-4o-2024-08-06\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n",
				"data: {\"id\":\"c1\",\"model\":\"gpt-4o-2024-08-06\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n",
				"data: {\"id\":\"c1\",\"model\":\"gpt-4o-2024-08-06\",\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":2}}\n\n",
				"data: [DONE]\n\n",
			},
			want: map[attribute.Key]any{
				AttrRequestStream: true,
				AttrResponseModel: "gpt-4o-2024-08-06",
				AttrResponseID:    "c1",
				AttrFinishReasons: []string{"length"},
				AttrInputTokens:   21,
				AttrOutputTokens:  2,
			},
		},
		{
			name: "anthropic messages",
			path: "/v1/messages",
			req:  `{"model":"claude-haiku-4-5","stream":true,"max_tokens":50,"messages":[]}`,
			events: []string{
				"event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_9\",\"model\":\"claude-haiku-4-5-20251001\",\"usage\":{\"input_tokens\":40,\"output_tokens\":1}}}\r\n\r\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
				"event: ping\ndata: {\"type\":\"ping\"}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":50}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
			want: map[attribute.Key]any{
				AttrResponseModel: "claude-haiku-4-5-20251001",
				AttrResponseID:    "msg_9",
				AttrFinishReasons: []string{"max_tokens"},
				AttrInputTokens:   40,
				AttrOutputTokens:  50,
			},
		},
		{
			name: "openai responses",
			path: "/v1/responses",
			req:  `{"model":"gpt-4.1-mini","stream":true,"input":"hi"}`,
			events: []string{
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_7\",\"model\":\"gpt-4.1-mini\",\"status\":\"in_progress\"}}\n\n",
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"yo\"}\n\n",
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_7\",\"model\":\"gpt-4.1-mini\",\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n",
			},
			want: map[attribute.Key]any{
				AttrResponseID:    "resp_7",
				AttrFinishReasons: []string{"stop"},
				AttrInputTokens:   8,
				AttrOutputTokens:  1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, sseHandler(tt.events...))
			req, _ := http.NewRequest(http.MethodPost, h.baseURL+tt.path, strings.NewReader(tt.req))
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(h.spans.GetSpans()); n != 0 {
				t.Fatalf("span ended before stream was consumed (%d spans)", n)
			}
			// One byte at a time exercises SSE lines split across reads.
			b, err := io.ReadAll(iotest.OneByteReader(resp.Body))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if want := strings.Join(tt.events, ""); string(b) != want {
				t.Fatalf("stream modified for caller:\n got %q\nwant %q", b, want)
			}
			s := h.onlySpan()
			checkAttrs(t, s.Attributes, tt.want)
			if s.Status.Code == codes.Error {
				t.Errorf("unexpected error status %v", s.Status)
			}
		})
	}
}

func TestErrorStatus(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		status   int
		body     string
		wantType string
	}{
		{"anthropic rate limit", "/v1/messages", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "rate_limit_error"},
		{"openai invalid request", "/v1/chat/completions", 400, `{"error":{"message":"bad","type":"invalid_request_error","param":null,"code":null}}`, "invalid_request_error"},
		{"openai code only", "/v1/chat/completions", 404, `{"error":{"message":"nope","type":"","code":"model_not_found"}}`, "model_not_found"},
		{"no body", "/v1/chat/completions", 502, ``, "502"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, jsonHandler(tt.status, tt.body))
			resp, got := h.post(tt.path, `{"model":"m"}`)
			if resp.StatusCode != tt.status || got != tt.body {
				t.Fatalf("caller got %d %q", resp.StatusCode, got)
			}
			s := h.onlySpan()
			if s.Status.Code != codes.Error {
				t.Errorf("status = %v, want Error", s.Status.Code)
			}
			checkAttrs(t, s.Attributes, map[attribute.Key]any{
				AttrHTTPStatusCode: tt.status,
				AttrErrorType:      tt.wantType,
			})

			m, ok := findMetric(h.metrics(), MetricOperationDuration)
			if !ok {
				t.Fatal("no duration metric")
			}
			dp := m.Data.(metricdata.Histogram[float64]).DataPoints[0]
			if v, _ := dp.Attributes.Value(AttrErrorType); v.AsString() != tt.wantType {
				t.Errorf("duration error.type = %q, want %q", v.AsString(), tt.wantType)
			}
		})
	}
}

func TestStreamErrorEvent(t *testing.T) {
	h := newHarness(t, sseHandler(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
	))
	h.post("/v1/messages", `{"model":"claude-x","stream":true}`)
	s := h.onlySpan()
	if s.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", s.Status.Code)
	}
	checkAttrs(t, s.Attributes, map[attribute.Key]any{AttrErrorType: "overloaded_error", AttrHTTPStatusCode: 200})
}

func TestTransportError(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{}`))
	h.server.Close()
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if _, err := h.client.Do(req); err == nil {
		t.Fatal("expected error")
	}
	s := h.onlySpan()
	if s.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", s.Status.Code)
	}
	if _, ok := attrMap(s.Attributes)[AttrErrorType]; !ok {
		t.Error("missing error.type")
	}
	if len(s.Events) == 0 || s.Events[0].Name != "exception" {
		t.Errorf("error not recorded as exception event: %+v", s.Events)
	}
}

func TestCloseBeforeEOFEndsSpan(t *testing.T) {
	h := newHarness(t, sseHandler("data: {\"id\":\"x\",\"model\":\"gpt-4o\",\"choices\":[]}\n\n", "data: [DONE]\n\n"))
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true}`))
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close() // double close must not double-end
	if n := len(h.spans.GetSpans()); n != 1 {
		t.Fatalf("got %d spans, want 1", n)
	}
}

func TestCaptureOffByDefault(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{"model":"gpt-4o","choices":[{"finish_reason":"stop","message":{"content":"secret answer"}}]}`))
	h.post("/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"secret question"}]}`)
	s := h.onlySpan()
	if len(s.Events) != 0 {
		t.Errorf("expected no span events by default, got %+v", s.Events)
	}
	for _, kv := range s.Attributes {
		if strings.Contains(kv.Value.Emit(), "secret") {
			t.Errorf("content leaked into attribute %s", kv.Key)
		}
	}
}

func TestCaptureWithRedactor(t *testing.T) {
	var calls atomic.Int32
	redact := func(kind ContentKind, s string) string {
		calls.Add(1)
		return strings.ReplaceAll(s, "4111-1111", "[REDACTED]")
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		req     string
	}{
		{
			name:    "non-streaming",
			handler: jsonHandler(200, `{"model":"gpt-4o","choices":[{"finish_reason":"stop","message":{"content":"card 4111-1111 ok"}}]}`),
			req:     `{"model":"gpt-4o","messages":[{"role":"user","content":"my card is 4111-1111"}]}`,
		},
		{
			name: "streaming",
			handler: sseHandler(
				"data: {\"choices\":[{\"delta\":{\"content\":\"card 4111\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"-1111 ok\"}}]}\n\n",
				"data: [DONE]\n\n"),
			req: `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"my card is 4111-1111"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls.Store(0)
			h := newHarness(t, tt.handler, WithContentCapture(true), WithRedactor(redact))
			h.post("/v1/chat/completions", tt.req)
			s := h.onlySpan()
			if len(s.Events) != 2 {
				t.Fatalf("got %d events, want 2: %+v", len(s.Events), s.Events)
			}
			prompt := attrMap(s.Events[0].Attributes)[AttrPrompt].AsString()
			completion := attrMap(s.Events[1].Attributes)[AttrCompletion].AsString()
			if s.Events[0].Name != EventPrompt || s.Events[1].Name != EventCompletion {
				t.Errorf("event names = %q, %q", s.Events[0].Name, s.Events[1].Name)
			}
			if strings.Contains(prompt+completion, "4111-1111") {
				t.Errorf("redactor not applied: prompt=%q completion=%q", prompt, completion)
			}
			if !strings.Contains(prompt, "my card is [REDACTED]") || completion != "card [REDACTED] ok" {
				t.Errorf("unexpected content: prompt=%q completion=%q", prompt, completion)
			}
			if calls.Load() != 2 {
				t.Errorf("redactor called %d times, want 2", calls.Load())
			}
		})
	}
}

func TestCaptureTruncatesAndRedactorCanDrop(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{"choices":[{"message":{"content":"éééééééééé"}}]}`),
		WithContentCapture(true),
		WithMaxContentLength(5),
		WithRedactor(func(k ContentKind, s string) string {
			if k == ContentPrompt {
				return ""
			}
			return s
		}))
	h.post("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	s := h.onlySpan()
	if len(s.Events) != 1 || s.Events[0].Name != EventCompletion {
		t.Fatalf("want only completion event, got %+v", s.Events)
	}
	// 5 bytes, rounded down to a rune boundary: two 2-byte runes.
	if got := attrMap(s.Events[0].Attributes)[AttrCompletion].AsString(); got != "éé" {
		t.Errorf("completion = %q, want %q", got, "éé")
	}
}

func TestMetrics(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{"model":"gpt-4o-mini-2024-07-18","choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":500000}}`),
		WithPrices(ExamplePrices()))
	h.post("/v1/chat/completions", `{"model":"gpt-4o-mini"}`)
	h.post("/v1/chat/completions", `{"model":"gpt-4o-mini"}`)
	rm := h.metrics()

	m, ok := findMetric(rm, MetricTokenUsage)
	if !ok {
		t.Fatal("token usage metric missing")
	}
	if m.Unit != "{token}" {
		t.Errorf("unit = %q", m.Unit)
	}
	sums := map[string]int64{}
	for _, dp := range m.Data.(metricdata.Histogram[int64]).DataPoints {
		v, _ := dp.Attributes.Value(AttrTokenType)
		sums[v.AsString()] += dp.Sum
		if model, _ := dp.Attributes.Value(AttrResponseModel); model.AsString() != "gpt-4o-mini-2024-07-18" {
			t.Errorf("response model attr = %q", model.AsString())
		}
	}
	if sums["input"] != 2_000_000 || sums["output"] != 1_000_000 {
		t.Errorf("token sums = %v", sums)
	}

	m, ok = findMetric(rm, MetricCost)
	if !ok {
		t.Fatal("cost metric missing")
	}
	// Per call: 1M input * $0.15 + 0.5M output * $0.60 = $0.45.
	got := m.Data.(metricdata.Sum[float64]).DataPoints[0].Value
	if diff := got - 0.90; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %v, want 0.90", got)
	}
	s := h.spans.GetSpans()[0]
	checkAttrs(t, s.Attributes, map[attribute.Key]any{AttrCostUSD: 0.45})

	m, ok = findMetric(rm, MetricOperationDuration)
	if !ok {
		t.Fatal("duration metric missing")
	}
	if c := m.Data.(metricdata.Histogram[float64]).DataPoints[0].Count; c != 2 {
		t.Errorf("duration count = %d, want 2", c)
	}
}

func TestNoCostWithoutPriceTable(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{"model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":10}}`))
	h.post("/v1/chat/completions", `{"model":"gpt-4o"}`)
	if m, ok := findMetric(h.metrics(), MetricCost); ok {
		if dps := m.Data.(metricdata.Sum[float64]).DataPoints; len(dps) > 0 {
			t.Errorf("unexpected cost data points: %+v", dps)
		}
	}
	if _, ok := attrMap(h.onlySpan().Attributes)[AttrCostUSD]; ok {
		t.Error("unexpected cost attribute")
	}
}

func TestRetry(t *testing.T) {
	var n atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"model":"m"}` {
			t.Errorf("attempt %d got body %q", n.Load()+1, b)
		}
		if n.Add(1) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}
	h := newHarness(t, handler, WithRetry(RetryPolicy{MaxRetries: 3}), instantSleep())
	resp, _ := h.post("/v1/chat/completions", `{"model":"m"}`)
	if resp.StatusCode != 200 || n.Load() != 3 {
		t.Fatalf("status %d after %d attempts", resp.StatusCode, n.Load())
	}
	s := h.onlySpan()
	checkAttrs(t, s.Attributes, map[attribute.Key]any{AttrResendCount: 2, AttrHTTPStatusCode: 200})
	var retries int
	for _, e := range s.Events {
		if e.Name == EventRetry {
			retries++
			checkAttrs(t, e.Attributes, map[attribute.Key]any{AttrRetryReason: "503", AttrRetryDelay: 1000})
		}
	}
	if retries != 2 {
		t.Errorf("retry events = %d, want 2", retries)
	}
}

func TestRetryGivesUp(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(529)
	}, WithRetry(RetryPolicy{MaxRetries: 1}), instantSleep())
	resp, _ := h.post("/v1/messages", `{"model":"claude"}`)
	if resp.StatusCode != 529 || n.Load() != 2 {
		t.Fatalf("status %d after %d attempts", resp.StatusCode, n.Load())
	}
	checkAttrs(t, h.onlySpan().Attributes, map[attribute.Key]any{AttrResendCount: 1, AttrErrorType: "529"})
}

func TestSDKRetryHeader(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{}`))
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/v1/messages", strings.NewReader(`{"model":"c"}`))
	req.Header.Set("X-Stainless-Retry-Count", "2")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	checkAttrs(t, h.onlySpan().Attributes, map[attribute.Key]any{AttrResendCount: 2})
}

func TestStreamUsageInjection(t *testing.T) {
	tests := []struct {
		name   string
		inject bool
		req    string
		want   string
	}{
		{"off by default", false, `{"model":"m","stream":true}`, `{"model":"m","stream":true}`},
		{"adds stream_options", true, `{"model":"m","stream":true}`, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`},
		{"respects explicit false", true, `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`},
		{"ignores non-streaming", true, `{"model":"m"}`, `{"model":"m"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got = string(b)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}, WithStreamUsageInjection(tt.inject))
			h.post("/v1/chat/completions", tt.req)
			if got != tt.want {
				t.Errorf("server got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPassThrough(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{"data":[]}`))
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodPost, "/v1/files"},
		{http.MethodPost, "/v1/threads/t_1/messages"},
	} {
		req, _ := http.NewRequest(c.method, h.baseURL+c.path, nil)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if n := len(h.spans.GetSpans()); n != 0 {
		t.Errorf("got %d spans for non-LLM requests, want 0", n)
	}
}

func TestParentSpanPropagation(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{}`))
	tp := h.transprt.tracer
	ctx, parent := tp.Start(context.Background(), "parent")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	parent.End()
	for _, s := range h.spans.GetSpans() {
		if s.Name == "chat m" && s.Parent.SpanID() != parent.SpanContext().SpanID() {
			t.Error("LLM span is not a child of the caller's span")
		}
	}
}

func TestDetectAPIAndProvider(t *testing.T) {
	tests := []struct {
		method, url  string
		wantAPI      API
		wantProvider string
	}{
		{"POST", "https://api.openai.com/v1/chat/completions", APIOpenAIChat, "openai"},
		{"POST", "https://api.openai.com/v1/embeddings", APIOpenAIEmbeddings, "openai"},
		{"POST", "https://api.openai.com/v1/responses", APIOpenAIResponses, "openai"},
		{"POST", "https://api.anthropic.com/v1/messages", APIAnthropicMessages, "anthropic"},
		{"POST", "https://x.openai.azure.com/openai/deployments/d/chat/completions?api-version=1", APIOpenAIChat, "azure.ai.openai"},
		{"POST", "https://api.groq.com/openai/v1/chat/completions", APIOpenAIChat, "groq"},
		{"POST", "http://localhost:11434/v1/chat/completions/", APIOpenAIChat, "openai"},
		{"POST", "http://gateway.internal/anthropic/v1/messages", APIAnthropicMessages, "anthropic"},
		{"GET", "https://api.openai.com/v1/chat/completions", APIUnknown, ""},
		{"POST", "https://api.openai.com/v1/threads/abc/messages", APIUnknown, ""},
		{"POST", "https://api.openai.com/v1/images/generations", APIUnknown, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.url, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, tt.url, nil)
			api := DetectAPI(req)
			if api != tt.wantAPI {
				t.Fatalf("DetectAPI = %v, want %v", api, tt.wantAPI)
			}
			if api == APIUnknown {
				return
			}
			if p := providerName(req.URL.Hostname(), api); p != tt.wantProvider {
				t.Errorf("provider = %q, want %q", p, tt.wantProvider)
			}
		})
	}
}

func TestTransportErrorType(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{errors.New("boom"), "_OTHER"},
	}
	for _, tt := range tests {
		if got := transportErrorType(tt.err); got != tt.want {
			t.Errorf("transportErrorType(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
