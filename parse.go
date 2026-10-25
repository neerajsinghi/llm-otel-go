package llmotel

import (
	"bytes"
	"encoding/json"
	"strings"
)

// requestInfo is what the transport learns from a request body.
type requestInfo struct {
	Model          string
	Stream         bool
	MaxTokens      *int64
	Temperature    *float64
	TopP           *float64
	StreamUsageSet bool
	Prompt         string // compact JSON of the prompt-bearing fields
}

type wireRequest struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream"`
	MaxTokens           *int64          `json:"max_tokens"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens"`
	MaxOutputTokens     *int64          `json:"max_output_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Messages            json.RawMessage `json:"messages"`
	System              json.RawMessage `json:"system"`
	Instructions        json.RawMessage `json:"instructions"`
	Input               json.RawMessage `json:"input"`
	StreamOptions       *struct {
		IncludeUsage *bool `json:"include_usage"`
	} `json:"stream_options"`
}

// parseRequest extracts request attributes. The prompt is only assembled when
// capture is true, to avoid needless allocation.
func parseRequest(body []byte, capture bool) requestInfo {
	var w wireRequest
	if len(body) == 0 || json.Unmarshal(body, &w) != nil {
		return requestInfo{}
	}
	info := requestInfo{
		Model:       w.Model,
		Stream:      w.Stream,
		Temperature: w.Temperature,
		TopP:        w.TopP,
		MaxTokens:   firstNonNil(w.MaxTokens, w.MaxCompletionTokens, w.MaxOutputTokens),
	}
	if w.StreamOptions != nil && w.StreamOptions.IncludeUsage != nil {
		info.StreamUsageSet = true
	}
	if capture {
		prompt := map[string]json.RawMessage{}
		for k, v := range map[string]json.RawMessage{
			"system":       w.System,
			"instructions": w.Instructions,
			"messages":     w.Messages,
			"input":        w.Input,
		} {
			if len(v) > 0 && !bytes.Equal(v, []byte("null")) {
				prompt[k] = v
			}
		}
		if len(prompt) > 0 {
			b, _ := json.Marshal(prompt) // map keys are sorted: stable output
			info.Prompt = string(b)
		}
	}
	return info
}

// injectStreamUsage adds stream_options.include_usage=true to a JSON body.
// It returns the body unchanged if it cannot be decoded.
func injectStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok {
		_ = json.Unmarshal(raw, &opts)
	}
	opts["include_usage"] = json.RawMessage("true")
	b, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	m["stream_options"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func firstNonNil[T any](vs ...*T) *T {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// result accumulates everything learned from a response.
type result struct {
	api API

	ID            string
	Model         string
	FinishReasons []string
	ErrorType     string

	hasUsage      bool
	inputTokens   int64
	outputTokens  int64
	cacheRead     int64
	cacheCreation int64

	capture    bool
	maxContent int
	completion strings.Builder
	truncated  bool
}

// InputTokens returns total input tokens. For Anthropic, whose input_tokens
// excludes cached tokens, cache reads and writes are added so that the value
// is comparable across providers, as the GenAI conventions recommend.
func (r *result) InputTokens() int64 {
	if r.api == APIAnthropicMessages {
		return r.inputTokens + r.cacheRead + r.cacheCreation
	}
	return r.inputTokens
}

// OutputTokens returns output tokens.
func (r *result) OutputTokens() int64 { return r.outputTokens }

func (r *result) appendCompletion(s string) {
	if !r.capture || s == "" || r.truncated {
		return
	}
	if room := r.maxContent - r.completion.Len(); len(s) > room {
		s = truncateUTF8(s, room)
		r.truncated = true
	}
	r.completion.WriteString(s)
}

func (r *result) addFinish(reason string) {
	if reason == "" {
		return
	}
	for _, f := range r.FinishReasons {
		if f == reason {
			return
		}
	}
	r.FinishReasons = append(r.FinishReasons, reason)
}

type wireUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	InputTokens         *int64 `json:"input_tokens"`
	OutputTokens        *int64 `json:"output_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// apply merges usage into r. Fields present in u overwrite earlier values,
// which matches how Anthropic streams send cumulative counts.
func (u *wireUsage) apply(r *result) {
	if u == nil {
		return
	}
	set := func(dst *int64, vs ...*int64) {
		if v := firstNonNil(vs...); v != nil {
			*dst = *v
			r.hasUsage = true
		}
	}
	set(&r.inputTokens, u.PromptTokens, u.InputTokens)
	set(&r.outputTokens, u.CompletionTokens, u.OutputTokens)
	set(&r.cacheCreation, u.CacheCreationTokens)
	set(&r.cacheRead, u.CacheReadTokens)
	if u.PromptTokensDetails != nil {
		set(&r.cacheRead, u.PromptTokensDetails.CachedTokens)
	}
	if u.InputTokensDetails != nil {
		set(&r.cacheRead, u.InputTokensDetails.CachedTokens)
	}
}

type wireTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireResponse struct {
	ID      string     `json:"id"`
	Model   string     `json:"model"`
	Usage   *wireUsage `json:"usage"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Message      *struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"message"`
		Delta *struct {
			Content *string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`

	// Anthropic
	StopReason *string        `json:"stop_reason"`
	Content    []wireTextPart `json:"content"`

	// OpenAI Responses
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string         `json:"type"`
		Content []wireTextPart `json:"content"`
	} `json:"output"`

	Error json.RawMessage `json:"error"`
}

// apply merges a decoded response document (or stream chunk) into r.
func (w *wireResponse) apply(r *result) {
	if w == nil {
		return
	}
	if w.ID != "" {
		r.ID = w.ID
	}
	if w.Model != "" {
		r.Model = w.Model
	}
	w.Usage.apply(r)
	for _, c := range w.Choices {
		if c.FinishReason != nil {
			r.addFinish(*c.FinishReason)
		}
		if c.Message != nil {
			r.appendCompletion(textOf(c.Message.Content))
		}
		if c.Delta != nil && c.Delta.Content != nil {
			r.appendCompletion(*c.Delta.Content)
		}
	}
	if w.StopReason != nil {
		r.addFinish(*w.StopReason)
	}
	for _, p := range w.Content {
		if p.Type == "text" {
			r.appendCompletion(p.Text)
		}
	}
	if r.api == APIOpenAIResponses {
		toolCall := false
		for _, o := range w.Output {
			if o.Type == "function_call" {
				toolCall = true
			}
			for _, p := range o.Content {
				if p.Type == "output_text" {
					r.appendCompletion(p.Text)
				}
			}
		}
		switch w.Status {
		case "completed":
			if toolCall {
				r.addFinish("tool_calls")
			} else {
				r.addFinish("stop")
			}
		case "incomplete":
			if w.IncompleteDetails != nil && w.IncompleteDetails.Reason != "" {
				r.addFinish(w.IncompleteDetails.Reason)
			} else {
				r.addFinish("incomplete")
			}
		case "failed":
			r.addFinish("error")
		}
	}
	if t := errorTypeOf(w.Error); t != "" {
		r.ErrorType = t
	}
}

// textOf returns the text of an OpenAI message content field, which is either
// a string or an array of typed parts.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []wireTextPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// errorTypeOf extracts a low-cardinality error type from an OpenAI or
// Anthropic error object: {"type": "...", "code": "...", "message": "..."}.
func errorTypeOf(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var e struct {
		Type string          `json:"type"`
		Code json.RawMessage `json:"code"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	if e.Type != "" {
		return e.Type
	}
	var code string
	if json.Unmarshal(e.Code, &code) == nil && code != "" {
		return code
	}
	return ""
}

// parseResponseJSON parses a complete, non-streaming response document.
func parseResponseJSON(body []byte, r *result) {
	var w wireResponse
	if json.Unmarshal(body, &w) != nil {
		return
	}
	w.apply(r)
}

type wireStreamEvent struct {
	wireResponse
	Type     string          `json:"type"`
	Message  *wireResponse   `json:"message"`  // Anthropic message_start
	Response *wireResponse   `json:"response"` // OpenAI Responses lifecycle events
	Delta    json.RawMessage `json:"delta"`    // Anthropic: object; Responses: string
	Code     string          `json:"code"`     // OpenAI Responses error event
}

// parseStreamEvent folds one SSE data payload into r.
func parseStreamEvent(data []byte, r *result) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var ev wireStreamEvent
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch {
	case r.api == APIAnthropicMessages:
		switch ev.Type {
		case "message_start":
			ev.Message.apply(r)
		case "content_block_delta":
			var d struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Delta, &d) == nil && d.Type == "text_delta" {
				r.appendCompletion(d.Text)
			}
		case "message_delta":
			var d struct {
				StopReason *string `json:"stop_reason"`
			}
			if json.Unmarshal(ev.Delta, &d) == nil && d.StopReason != nil {
				r.addFinish(*d.StopReason)
			}
			ev.Usage.apply(r)
		case "error":
			if t := errorTypeOf(ev.Error); t != "" {
				r.ErrorType = t
			} else {
				r.ErrorType = "stream_error"
			}
		}
	case r.api == APIOpenAIResponses:
		switch ev.Type {
		case "response.created", "response.in_progress":
			if ev.Response != nil {
				if ev.Response.ID != "" {
					r.ID = ev.Response.ID
				}
				if ev.Response.Model != "" {
					r.Model = ev.Response.Model
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
			ev.Response.apply(r)
			if ev.Type == "response.failed" && r.ErrorType == "" {
				r.ErrorType = "response_failed"
			}
		case "error":
			r.ErrorType = ev.Code
			if r.ErrorType == "" {
				r.ErrorType = "stream_error"
			}
		}
	default: // OpenAI chat completion chunks
		ev.wireResponse.apply(r)
	}
}

// truncateUTF8 shortens s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
