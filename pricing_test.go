package llmotel

import (
	"math"
	"strings"
	"testing"
)

func TestPriceTableCost(t *testing.T) {
	table := PriceTable{
		"gpt-4o":          {InputPerMillion: 2.5, OutputPerMillion: 10},
		"gpt-4o-mini":     {InputPerMillion: 0.15, OutputPerMillion: 0.6},
		"claude-sonnet-4": {InputPerMillion: 3, OutputPerMillion: 15},
		"embed":           {InputPerMillion: 0.02},
	}
	tests := []struct {
		name    string
		model   string
		in, out int64
		want    float64
		wantOK  bool
	}{
		{"exact match", "gpt-4o", 1_000_000, 1_000_000, 12.5, true},
		{"dated snapshot uses prefix", "gpt-4o-2024-08-06", 2_000, 1_000, 0.015, true},
		{"longest prefix wins", "gpt-4o-mini-2024-07-18", 1_000_000, 0, 0.15, true},
		{"anthropic dated", "claude-sonnet-4-20250514", 1_000, 2_000, 0.033, true},
		{"input only", "embed", 500_000, 0, 0.01, true},
		{"zero tokens", "gpt-4o", 0, 0, 0, true},
		{"unknown model", "llama-3", 100, 100, 0, false},
		{"empty model", "", 100, 100, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := table.Cost(tt.model, tt.in, tt.out)
			if ok != tt.wantOK || math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("Cost(%q, %d, %d) = %v, %v; want %v, %v", tt.model, tt.in, tt.out, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestNilPriceTable(t *testing.T) {
	var table PriceTable
	if _, ok := table.Cost("gpt-4o", 1, 1); ok {
		t.Error("nil table should not price anything")
	}
}

func TestExamplePricesIsACopy(t *testing.T) {
	a := ExamplePrices()
	a["gpt-4o"] = Price{}
	if ExamplePrices()["gpt-4o"].InputPerMillion == 0 {
		t.Error("ExamplePrices returned shared state")
	}
}

func TestLoadPrices(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		check   func(PriceTable) bool
	}{
		{
			name:  "valid",
			in:    `{"my-model": {"input_per_million": 1.5, "output_per_million": 3}}`,
			check: func(p PriceTable) bool { return p["my-model"] == Price{1.5, 3} },
		},
		{name: "malformed", in: `{`, wantErr: true},
		{name: "negative", in: `{"m": {"input_per_million": -1}}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := LoadPrices(strings.NewReader(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.check != nil && !tt.check(p) {
				t.Errorf("unexpected table %+v", p)
			}
		})
	}
}
