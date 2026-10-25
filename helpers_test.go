package llmotel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// harness wires a Transport to in-memory trace and metric exporters.
type harness struct {
	t        *testing.T
	spans    *tracetest.InMemoryExporter
	reader   *sdkmetric.ManualReader
	client   *http.Client
	server   *httptest.Server
	baseURL  string
	transprt *Transport
}

func newHarness(t *testing.T, handler http.HandlerFunc, opts ...Option) *harness {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	opts = append([]Option{WithTracerProvider(tp), WithMeterProvider(mp)}, opts...)
	tr := NewTransport(srv.Client().Transport, opts...)
	return &harness{
		t:        t,
		spans:    exp,
		reader:   reader,
		client:   &http.Client{Transport: tr},
		server:   srv,
		baseURL:  srv.URL,
		transprt: tr,
	}
}

// post sends body to path and returns the full response body the caller sees.
func (h *harness) post(path, body string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.baseURL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("request failed: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, string(b)
}

func (h *harness) onlySpan() tracetest.SpanStub {
	h.t.Helper()
	ss := h.spans.GetSpans()
	if len(ss) != 1 {
		h.t.Fatalf("got %d spans, want 1", len(ss))
	}
	return ss[0]
}

func (h *harness) metrics() metricdata.ResourceMetrics {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatal(err)
	}
	return rm
}

func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func attrMap(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

// checkAttrs asserts that span carries every wanted attribute. Values are
// compared via their Emit() string form.
func checkAttrs(t *testing.T, got []attribute.KeyValue, want map[attribute.Key]any) {
	t.Helper()
	m := attrMap(got)
	for k, w := range want {
		v, ok := m[k]
		if !ok {
			t.Errorf("missing attribute %s", k)
			continue
		}
		if g := v.AsInterface(); !equalAttr(g, w) {
			t.Errorf("attribute %s = %#v, want %#v", k, g, w)
		}
	}
}

func equalAttr(got, want any) bool {
	switch w := want.(type) {
	case int:
		g, ok := got.(int64)
		return ok && g == int64(w)
	case []string:
		g, ok := got.([]string)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	default:
		return got == want
	}
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func sseHandler(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			f.Flush()
		}
	}
}

// instantSleep makes retry backoff immediate in tests.
func instantSleep() Option {
	return func(c *config) {
		c.sleep = func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		}
	}
}
