package llmotel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope name used for the tracer and meter.
const ScopeName = "github.com/neerajsinghi/llm-otel-go"

// Version is the version of this package.
const Version = "0.1.0"

// Transport is an [http.RoundTripper] that instruments LLM API calls. Create
// one with [NewTransport]. It is safe for concurrent use.
type Transport struct {
	base   http.RoundTripper
	cfg    *config
	tracer trace.Tracer

	tokenUsage metric.Int64Histogram
	duration   metric.Float64Histogram
	cost       metric.Float64Counter
}

// NewTransport wraps base (http.DefaultTransport when nil) with LLM
// instrumentation.
func NewTransport(base http.RoundTripper, opts ...Option) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	cfg := newConfig(opts)
	tp := cfg.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	mp := cfg.meterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	t := &Transport{
		base:   base,
		cfg:    cfg,
		tracer: tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version)),
	}
	meter := mp.Meter(ScopeName, metric.WithInstrumentationVersion(Version))
	var err error
	t.tokenUsage, err = meter.Int64Histogram(MetricTokenUsage,
		metric.WithUnit("{token}"),
		metric.WithDescription("Number of input and output tokens used per LLM call."),
		metric.WithExplicitBucketBoundaries(1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864),
	)
	otelHandle(err)
	t.duration, err = meter.Float64Histogram(MetricOperationDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of LLM calls, including the full stream for streaming responses."),
		metric.WithExplicitBucketBoundaries(0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92),
	)
	otelHandle(err)
	t.cost, err = meter.Float64Counter(MetricCost,
		metric.WithUnit("USD"),
		metric.WithDescription("Estimated cost of LLM calls in US dollars, from a user-maintained price table."),
	)
	otelHandle(err)
	return t
}

func otelHandle(err error) {
	if err != nil {
		otel.Handle(err)
	}
}

// NewClient returns an *http.Client whose transport is
// NewTransport(http.DefaultTransport, opts...).
func NewClient(opts ...Option) *http.Client {
	return &http.Client{Transport: NewTransport(nil, opts...)}
}

// WrapClient returns a shallow copy of c (http.DefaultClient when nil) whose
// transport is wrapped with LLM instrumentation. c itself is not modified.
func WrapClient(c *http.Client, opts ...Option) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	cp := *c
	cp.Transport = NewTransport(c.Transport, opts...)
	return &cp
}

// CloseIdleConnections forwards to the wrapped transport when it supports it.
func (t *Transport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// RoundTrip implements [http.RoundTripper].
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	api := DetectAPI(req)
	if api == APIUnknown {
		return t.base.RoundTrip(req)
	}
	start := time.Now()

	body, err := readRequestBody(req)
	if err != nil {
		return nil, err
	}
	info := parseRequest(body, t.cfg.captureContent)
	if t.cfg.injectUsage && api == APIOpenAIChat && info.Stream && !info.StreamUsageSet {
		body = injectStreamUsage(body)
	}

	host := req.URL.Hostname()
	provider := providerName(host, api)
	op := api.Operation()
	port := serverPort(req.URL.Port(), req.URL.Scheme)

	// Attributes shared by the span and every metric data point.
	common := []attribute.KeyValue{
		AttrOperationName.String(op),
		AttrProviderName.String(provider),
		AttrServerAddress.String(host),
		AttrServerPort.Int(port),
	}
	if info.Model != "" {
		common = append(common, AttrRequestModel.String(info.Model))
	}

	spanAttrs := append([]attribute.KeyValue{AttrAPI.String(api.String())}, common...)
	if t.cfg.legacySystemAttr {
		spanAttrs = append(spanAttrs, AttrSystem.String(provider))
	}
	if info.MaxTokens != nil {
		spanAttrs = append(spanAttrs, AttrRequestMaxTokens.Int64(*info.MaxTokens))
	}
	if info.Temperature != nil {
		spanAttrs = append(spanAttrs, AttrRequestTemperature.Float64(*info.Temperature))
	}
	if info.TopP != nil {
		spanAttrs = append(spanAttrs, AttrRequestTopP.Float64(*info.TopP))
	}
	if info.Stream {
		spanAttrs = append(spanAttrs, AttrRequestStream.Bool(true))
	}

	name := op
	if info.Model != "" {
		name += " " + info.Model
	}
	ctx, span := t.tracer.Start(req.Context(), name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(start),
		trace.WithAttributes(spanAttrs...),
	)
	if t.cfg.captureContent && info.Prompt != "" {
		t.addContentEvent(span, ContentPrompt, info.Prompt)
	}

	call := &call{t: t, span: span, start: start, info: info, common: common}

	resp, resends, err := t.send(ctx, req, body, span)
	if resends == 0 {
		resends = sdkRetryCount(req.Header)
	}
	if resends > 0 {
		span.SetAttributes(AttrResendCount.Int(resends))
	}
	if err != nil {
		call.fail(transportErrorType(err), err)
		return nil, err
	}

	span.SetAttributes(AttrHTTPStatusCode.Int(resp.StatusCode))
	res := &result{api: api, capture: t.cfg.captureContent, maxContent: t.cfg.maxContentLength}
	call.status = resp.StatusCode

	var s sink = &jsonSink{}
	if resp.StatusCode < 400 && (isEventStream(resp.Header) || info.Stream) {
		s = &sseSink{r: res}
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		s.finish(res)
		call.done(res, nil)
		return resp, nil
	}
	resp.Body = &instrumentedBody{rc: resp.Body, sink: s, res: res, done: call.done}
	return resp, nil
}

// send performs the request, retrying according to the retry policy. It
// returns the final response or error and the number of resends.
func (t *Transport) send(ctx context.Context, orig *http.Request, body []byte, span trace.Span) (*http.Response, int, error) {
	for attempt := 0; ; attempt++ {
		req := orig.Clone(ctx)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.ContentLength = int64(len(body))

		resp, err := t.base.RoundTrip(req)
		reason, retry := retryReason(ctx, resp, err)
		if !retry || attempt >= t.cfg.retry.MaxRetries {
			return resp, attempt, err
		}
		delay := t.backoff(attempt, resp)
		span.AddEvent(EventRetry, trace.WithAttributes(
			AttrRetryAttempt.Int(attempt+1),
			AttrRetryReason.String(reason),
			AttrRetryDelay.Int64(delay.Milliseconds()),
		))
		if resp != nil {
			_, _ = io.CopyN(io.Discard, resp.Body, 4096)
			_ = resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, attempt, ctx.Err()
		case <-t.cfg.sleep(delay):
		}
	}
}

func retryReason(ctx context.Context, resp *http.Response, err error) (string, bool) {
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", false
		}
		return "transport_error", true
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, 529: // 529: Anthropic "overloaded"
		return strconv.Itoa(resp.StatusCode), true
	}
	return "", false
}

func (t *Transport) backoff(attempt int, resp *http.Response) time.Duration {
	p := t.cfg.retry
	if resp != nil {
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, p.MaxDelay)
		}
	}
	d := p.BaseDelay << attempt
	if d <= 0 || d > p.MaxDelay {
		d = p.MaxDelay
	}
	return d
}

func (t *Transport) addContentEvent(span trace.Span, kind ContentKind, content string) {
	if t.cfg.redactor != nil {
		content = t.cfg.redactor(kind, content)
	}
	if content == "" {
		return
	}
	content = truncateUTF8(content, t.cfg.maxContentLength)
	name, key := EventPrompt, AttrPrompt
	if kind == ContentCompletion {
		name, key = EventCompletion, AttrCompletion
	}
	span.AddEvent(name, trace.WithAttributes(key.String(content)))
}

// call carries per-request state from RoundTrip to the moment the response
// body is finished.
type call struct {
	t      *Transport
	span   trace.Span
	start  time.Time
	info   requestInfo
	common []attribute.KeyValue
	status int
}

// fail ends the span for a request that produced no response.
func (c *call) fail(errType string, err error) {
	c.span.RecordError(err)
	c.span.SetStatus(codes.Error, err.Error())
	c.span.SetAttributes(AttrErrorType.String(errType))
	attrs := append(c.common, AttrErrorType.String(errType))
	c.t.duration.Record(context.Background(), time.Since(c.start).Seconds(), metric.WithAttributes(attrs...))
	c.span.End()
}

// done records response attributes and metrics and ends the span.
func (c *call) done(res *result, readErr error) {
	ctx := trace.ContextWithSpan(context.Background(), c.span)
	span := c.span
	attrs := c.common
	if res.Model != "" {
		attrs = append(attrs, AttrResponseModel.String(res.Model))
		span.SetAttributes(AttrResponseModel.String(res.Model))
	}
	if res.ID != "" {
		span.SetAttributes(AttrResponseID.String(res.ID))
	}
	if len(res.FinishReasons) > 0 {
		span.SetAttributes(AttrFinishReasons.StringSlice(res.FinishReasons))
	}

	errType := ""
	switch {
	case c.status >= 400:
		errType = res.ErrorType
		if errType == "" {
			errType = strconv.Itoa(c.status)
		}
		span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(c.status))
	case readErr != nil:
		errType = "stream_interrupted"
		span.RecordError(readErr)
		span.SetStatus(codes.Error, readErr.Error())
	case res.ErrorType != "":
		errType = res.ErrorType
		span.SetStatus(codes.Error, "error event in response stream")
	}
	if errType != "" {
		span.SetAttributes(AttrErrorType.String(errType))
	}

	if res.hasUsage {
		in, out := res.InputTokens(), res.OutputTokens()
		span.SetAttributes(AttrInputTokens.Int64(in))
		if res.api != APIOpenAIEmbeddings {
			span.SetAttributes(AttrOutputTokens.Int64(out))
		}
		if res.cacheRead > 0 {
			span.SetAttributes(AttrCacheReadTokens.Int64(res.cacheRead))
		}
		if res.cacheCreation > 0 {
			span.SetAttributes(AttrCacheCreationTokens.Int64(res.cacheCreation))
		}
		c.t.tokenUsage.Record(ctx, in, metric.WithAttributes(append(attrs, AttrTokenType.String("input"))...))
		if res.api != APIOpenAIEmbeddings {
			c.t.tokenUsage.Record(ctx, out, metric.WithAttributes(append(attrs, AttrTokenType.String("output"))...))
		}
		if c.t.cfg.prices != nil {
			model := res.Model
			if model == "" {
				model = c.info.Model
			}
			usd, ok := c.t.cfg.prices.Cost(model, in, out)
			if !ok && model != c.info.Model {
				usd, ok = c.t.cfg.prices.Cost(c.info.Model, in, out)
			}
			if ok {
				span.SetAttributes(AttrCostUSD.Float64(usd))
				c.t.cost.Add(ctx, usd, metric.WithAttributes(attrs...))
			}
		}
	}

	if c.t.cfg.captureContent && res.completion.Len() > 0 {
		c.t.addContentEvent(span, ContentCompletion, res.completion.String())
	}

	durAttrs := attrs
	if errType != "" {
		durAttrs = append(durAttrs, AttrErrorType.String(errType))
	}
	c.t.duration.Record(ctx, time.Since(c.start).Seconds(), metric.WithAttributes(durAttrs...))
	span.End()
}

// readRequestBody reads and closes the request body.
func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}

// sdkRetryCount reads the retry counter sent by the official OpenAI and
// Anthropic SDKs, which retry by calling RoundTrip again.
func sdkRetryCount(h http.Header) int {
	n, err := strconv.Atoi(h.Get("X-Stainless-Retry-Count"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func transportErrorType(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	}
	return "_OTHER"
}

func isEventStream(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "text/event-stream"
}

func serverPort(port, scheme string) int {
	if p, err := strconv.Atoi(port); err == nil {
		return p
	}
	if scheme == "http" {
		return 80
	}
	return 443
}
