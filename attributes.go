package llmotel

import "go.opentelemetry.io/otel/attribute"

// Attribute keys emitted by this package. Keys in the gen_ai.*, server.*,
// http.* and error.* namespaces follow the OpenTelemetry semantic conventions;
// keys in the llm.* namespace are specific to this package.
const (
	AttrOperationName       = attribute.Key("gen_ai.operation.name")
	AttrProviderName        = attribute.Key("gen_ai.provider.name")
	AttrSystem              = attribute.Key("gen_ai.system") // deprecated alias of gen_ai.provider.name
	AttrRequestModel        = attribute.Key("gen_ai.request.model")
	AttrRequestMaxTokens    = attribute.Key("gen_ai.request.max_tokens")
	AttrRequestTemperature  = attribute.Key("gen_ai.request.temperature")
	AttrRequestTopP         = attribute.Key("gen_ai.request.top_p")
	AttrRequestStream       = attribute.Key("gen_ai.request.stream")
	AttrResponseModel       = attribute.Key("gen_ai.response.model")
	AttrResponseID          = attribute.Key("gen_ai.response.id")
	AttrFinishReasons       = attribute.Key("gen_ai.response.finish_reasons")
	AttrInputTokens         = attribute.Key("gen_ai.usage.input_tokens")
	AttrOutputTokens        = attribute.Key("gen_ai.usage.output_tokens")
	AttrCacheReadTokens     = attribute.Key("gen_ai.usage.cache_read.input_tokens")
	AttrCacheCreationTokens = attribute.Key("gen_ai.usage.cache_creation.input_tokens")
	AttrTokenType           = attribute.Key("gen_ai.token.type")
	AttrPrompt              = attribute.Key("gen_ai.prompt")
	AttrCompletion          = attribute.Key("gen_ai.completion")

	AttrServerAddress  = attribute.Key("server.address")
	AttrServerPort     = attribute.Key("server.port")
	AttrHTTPStatusCode = attribute.Key("http.response.status_code")
	AttrResendCount    = attribute.Key("http.request.resend_count")
	AttrErrorType      = attribute.Key("error.type")

	AttrAPI          = attribute.Key("llm.api")
	AttrCostUSD      = attribute.Key("llm.usage.cost_usd")
	AttrRetryAttempt = attribute.Key("llm.retry.attempt")
	AttrRetryReason  = attribute.Key("llm.retry.reason")
	AttrRetryDelay   = attribute.Key("llm.retry.delay_ms")
)

// Span event names.
const (
	EventPrompt     = "gen_ai.content.prompt"
	EventCompletion = "gen_ai.content.completion"
	EventRetry      = "llm.retry"
)

// Metric instrument names.
const (
	MetricTokenUsage        = "gen_ai.client.token.usage"
	MetricOperationDuration = "gen_ai.client.operation.duration"
	MetricCost              = "llm.client.cost"
)
