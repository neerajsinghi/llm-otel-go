package llmotel_test

import (
	"fmt"
	"net/http"
	"strings"

	llmotel "github.com/neerajsinghi/llm-otel-go"
)

// Wrap any *http.Client; pass it to your OpenAI or Anthropic SDK via its
// option for a custom HTTP client.
func ExampleNewTransport() {
	client := &http.Client{
		Transport: llmotel.NewTransport(http.DefaultTransport,
			llmotel.WithPrices(llmotel.ExamplePrices()),
		),
	}
	_ = client
}

// Capture prompts and completions, scrubbing e-mail addresses first.
func ExampleWithRedactor() {
	redact := func(kind llmotel.ContentKind, s string) string {
		var out []string
		for _, w := range strings.Fields(s) {
			if strings.Contains(w, "@") {
				w = "[email]"
			}
			out = append(out, w)
		}
		return strings.Join(out, " ")
	}
	client := llmotel.NewClient(
		llmotel.WithContentCapture(true),
		llmotel.WithRedactor(redact),
	)
	_ = client
	fmt.Println(redact(llmotel.ContentPrompt, "mail me at jane@example.com"))
	// Output: mail me at [email]
}

func ExamplePriceTable_Cost() {
	prices := llmotel.PriceTable{
		"gpt-4o-mini": {InputPerMillion: 0.15, OutputPerMillion: 0.60},
	}
	usd, ok := prices.Cost("gpt-4o-mini-2024-07-18", 12_000, 3_000)
	fmt.Printf("%.4f %v\n", usd, ok)
	// Output: 0.0036 true
}
