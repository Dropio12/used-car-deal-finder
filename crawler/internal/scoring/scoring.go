// Package scoring talks to the price scorer. The pipeline depends on the
// Scorer interface; ExecScorer is the implementation that runs the Rust
// `carbuyer-scorer` binary and exchanges JSON over stdin/stdout.
package scoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"carbuyer/crawler/internal/listing"
)

// SellerAdjustment is the private-party discount applied to a dealer baseline.
type SellerAdjustment struct {
	Ratio       float64  `json:"ratio"`
	DiscountPct *float64 `json:"discountPct"`
	N           int      `json:"n"`
}

// Score is one priced listing, as the scorer reports it.
type Score struct {
	Baseline     float64  `json:"baseline"`
	Low          *float64 `json:"low"`
	High         *float64 `json:"high"`
	N            int      `json:"n"`
	Basis        string   `json:"basis"`
	Trim         *string  `json:"trim"`
	KmAdjustment float64  `json:"kmAdjustment"`
	Spread       float64  `json:"spread"`
	Warnings     []string `json:"warnings"`
	Reliable     bool     `json:"reliable"`
	Price        float64  `json:"price"`
	Delta        float64  `json:"delta"`
	// DiscountPct is how far under the baseline the price sits. Positive = cheaper.
	DiscountPct      float64           `json:"discountPct"`
	BelowP25         bool              `json:"belowP25"`
	Damaged          bool              `json:"damaged"`
	Thin             bool              `json:"thin"`
	Market           string            `json:"market"`
	DealerBaseline   *float64          `json:"dealerBaseline,omitempty"`
	SellerAdjustment *SellerAdjustment `json:"sellerAdjustment,omitempty"`
}

// Result maps listing id to its score (nil when it could not be priced).
type Result struct {
	Scores    map[string]*Score
	Appraiser json.RawMessage
}

// Scorer prices a batch of listings against a baseline built from that batch.
type Scorer interface {
	Score(ctx context.Context, listings []listing.Listing) (Result, error)
}

// ExecScorer runs an external scorer program.
type ExecScorer struct {
	Binary string
	Args   []string
	Env    []string // extra environment, KEY=VALUE
	// Options is passed through as the scorer's "options" object, e.g.
	// {"model": {"minComps": 3}}.
	Options map[string]any
}

var _ Scorer = (*ExecScorer)(nil)

type wireScore struct {
	ID    *string `json:"id"`
	Score *Score  `json:"score"`
}

type wireOutput struct {
	Appraiser json.RawMessage `json:"appraiser"`
	Scores    []wireScore     `json:"scores"`
}

// Score implements Scorer.
func (e *ExecScorer) Score(ctx context.Context, listings []listing.Listing) (Result, error) {
	input := map[string]any{"listings": listings}
	if e.Options != nil {
		input["options"] = e.Options
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return Result{}, err
	}

	cmd := exec.CommandContext(ctx, e.Binary, e.Args...)
	cmd.Stdin = bytes.NewReader(payload)
	if len(e.Env) > 0 {
		cmd.Env = append(os.Environ(), e.Env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("scorer %s failed: %w: %s", e.Binary, err, strings.TrimSpace(stderr.String()))
	}

	var out wireOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Result{}, fmt.Errorf("scorer output is not valid JSON: %w", err)
	}
	res := Result{Scores: make(map[string]*Score, len(out.Scores)), Appraiser: out.Appraiser}
	for _, s := range out.Scores {
		res.Scores[listing.Deref(s.ID)] = s.Score
	}
	return res, nil
}

// None prices nothing. For crawls whose only job is to store and push
// listings (the Worker scores them), so the Rust binary is not needed.
type None struct{}

var _ Scorer = None{}

// Score implements Scorer: every listing is left unpriced.
func (None) Score(context.Context, []listing.Listing) (Result, error) {
	return Result{Scores: map[string]*Score{}, Appraiser: json.RawMessage("null")}, nil
}
