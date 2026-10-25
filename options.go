package llmotel

import (
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ContentKind identifies which side of a conversation a piece of captured
// content belongs to. It is passed to a [Redactor].
type ContentKind string

const (
	// ContentPrompt is request content: messages, system prompt, input.
	ContentPrompt ContentKind = "prompt"
	// ContentCompletion is response content: generated text.
	ContentCompletion ContentKind = "completion"
)

// Redactor rewrites captured content before it is attached to a span. It is
// only called when content capture is enabled. Returning an empty string drops
// the event entirely.
type Redactor func(kind ContentKind, content string) string

// RetryPolicy configures the transport's optional built-in retries. The zero
// value disables retries.
//
// Retries happen on network errors (other than context cancellation) and on
// HTTP 408, 409, 429, 500, 502, 503, 504 and 529. A Retry-After header with a
// delay in seconds is honoured, capped at MaxDelay.
type RetryPolicy struct {
	// MaxRetries is the number of additional attempts after the first one.
	MaxRetries int
	// BaseDelay is the initial backoff. It doubles on each retry.
	// Defaults to 500ms when MaxRetries > 0.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff. Defaults to 8s when MaxRetries > 0.
	MaxDelay time.Duration
}

// DefaultMaxContentLength is the default cap, in bytes, on captured prompt or
// completion content per span event.
const DefaultMaxContentLength = 16 * 1024

type config struct {
	tracerProvider   trace.TracerProvider
	meterProvider    metric.MeterProvider
	prices           PriceTable
	captureContent   bool
	redactor         Redactor
	maxContentLength int
	retry            RetryPolicy
	injectUsage      bool
	legacySystemAttr bool
	sleep            func(time.Duration) <-chan time.Time
}

// Option configures a [Transport].
type Option func(*config)

// WithTracerProvider sets the TracerProvider used to create spans. Defaults
// to the global provider from otel.GetTracerProvider.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracerProvider = tp }
}

// WithMeterProvider sets the MeterProvider used to create instruments.
// Defaults to the global provider from otel.GetMeterProvider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// WithPrices enables the llm.client.cost counter using the given price table.
// Without a price table no cost is recorded. See [ExamplePrices] for a
// starting point; the table is yours to maintain.
func WithPrices(p PriceTable) Option {
	return func(c *config) { c.prices = p }
}

// WithContentCapture turns recording of prompts and completions as span
// events on or off. It is off by default because prompts routinely contain
// personal or confidential data.
func WithContentCapture(enabled bool) Option {
	return func(c *config) { c.captureContent = enabled }
}

// WithRedactor sets a hook that rewrites captured content before it is
// recorded. It has no effect unless content capture is enabled.
func WithRedactor(r Redactor) Option {
	return func(c *config) { c.redactor = r }
}

// WithMaxContentLength caps the size in bytes of each captured content event.
// Longer content is truncated. Values <= 0 restore the default.
func WithMaxContentLength(n int) Option {
	return func(c *config) { c.maxContentLength = n }
}

// WithRetry enables built-in retries for transient failures. All attempts are
// covered by a single span; each retry is recorded as a span event and the
// total is reported as http.request.resend_count.
func WithRetry(p RetryPolicy) Option {
	return func(c *config) { c.retry = p }
}

// WithStreamUsageInjection makes the transport add
// "stream_options": {"include_usage": true} to streaming OpenAI chat
// completion requests that do not already set it, so token usage can be
// recorded. The provider then sends one extra final chunk with an empty
// choices array; most SDKs handle this, but check yours. Off by default.
func WithStreamUsageInjection(enabled bool) Option {
	return func(c *config) { c.injectUsage = enabled }
}

// WithLegacySystemAttribute controls whether the deprecated gen_ai.system
// attribute is emitted alongside gen_ai.provider.name. It is on by default for
// compatibility with backends that still key on it.
func WithLegacySystemAttribute(enabled bool) Option {
	return func(c *config) { c.legacySystemAttr = enabled }
}

func newConfig(opts []Option) *config {
	c := &config{
		maxContentLength: DefaultMaxContentLength,
		legacySystemAttr: true,
		sleep:            time.After,
	}
	for _, o := range opts {
		o(c)
	}
	if c.maxContentLength <= 0 {
		c.maxContentLength = DefaultMaxContentLength
	}
	if c.retry.MaxRetries > 0 {
		if c.retry.BaseDelay <= 0 {
			c.retry.BaseDelay = 500 * time.Millisecond
		}
		if c.retry.MaxDelay <= 0 {
			c.retry.MaxDelay = 8 * time.Second
		}
	}
	return c
}
