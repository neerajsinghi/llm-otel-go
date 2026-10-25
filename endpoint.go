package llmotel

import (
	"net/http"
	"strings"
)

// API identifies the wire format of an LLM endpoint.
type API int

const (
	// APIUnknown means the request is not an LLM call this package
	// understands; it is passed through uninstrumented.
	APIUnknown API = iota
	// APIOpenAIChat is POST /v1/chat/completions.
	APIOpenAIChat
	// APIOpenAIEmbeddings is POST /v1/embeddings.
	APIOpenAIEmbeddings
	// APIOpenAIResponses is POST /v1/responses.
	APIOpenAIResponses
	// APIAnthropicMessages is POST /v1/messages.
	APIAnthropicMessages
)

// String returns a short name for the API.
func (a API) String() string {
	switch a {
	case APIOpenAIChat:
		return "openai.chat_completions"
	case APIOpenAIEmbeddings:
		return "openai.embeddings"
	case APIOpenAIResponses:
		return "openai.responses"
	case APIAnthropicMessages:
		return "anthropic.messages"
	default:
		return "unknown"
	}
}

// Operation returns the gen_ai.operation.name for the API.
func (a API) Operation() string {
	switch a {
	case APIOpenAIEmbeddings:
		return "embeddings"
	case APIOpenAIChat, APIOpenAIResponses, APIAnthropicMessages:
		return "chat"
	default:
		return ""
	}
}

// DetectAPI classifies a request by method and URL path. Only POST requests
// are instrumented. Matching is on the path suffix, so gateways and proxies
// that mount the API under a prefix (for example /openai/v1/chat/completions)
// are recognised too.
func DetectAPI(r *http.Request) API {
	if r == nil || r.URL == nil || r.Method != http.MethodPost {
		return APIUnknown
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case strings.HasSuffix(p, "/chat/completions"):
		return APIOpenAIChat
	case strings.HasSuffix(p, "/embeddings"):
		return APIOpenAIEmbeddings
	case strings.HasSuffix(p, "/responses"):
		return APIOpenAIResponses
	case strings.HasSuffix(p, "/messages"):
		// /v1/messages is Anthropic's API. OpenAI's Assistants API also has
		// .../threads/{id}/messages, which is not an inference call.
		if strings.Contains(p, "/threads/") {
			return APIUnknown
		}
		return APIAnthropicMessages
	}
	return APIUnknown
}

// hostProviders maps well-known API hosts to gen_ai.provider.name values.
var hostProviders = []struct{ suffix, name string }{
	{"api.openai.com", "openai"},
	{".openai.azure.com", "azure.ai.openai"},
	{"api.anthropic.com", "anthropic"},
	{"api.groq.com", "groq"},
	{"api.deepseek.com", "deepseek"},
	{"api.mistral.ai", "mistral_ai"},
	{"api.x.ai", "x_ai"},
	{"api.perplexity.ai", "perplexity"},
	{"generativelanguage.googleapis.com", "gcp.gemini"},
}

// providerName returns the gen_ai.provider.name for a request. Known hosts
// win; otherwise the wire format decides, so a self-hosted OpenAI-compatible
// server reports "openai".
func providerName(host string, api API) string {
	host = strings.ToLower(host)
	for _, hp := range hostProviders {
		if host == strings.TrimPrefix(hp.suffix, ".") || strings.HasSuffix(host, hp.suffix) {
			return hp.name
		}
	}
	if api == APIAnthropicMessages {
		return "anthropic"
	}
	return "openai"
}
