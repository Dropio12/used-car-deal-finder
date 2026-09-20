package scoring

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"carbuyer/crawler/internal/listing"
)

// TestHelperProcess is not a real test: ExecScorer runs this test binary as a
// stand-in scorer, so the exec + JSON plumbing is tested without cargo.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("SCORER_HELPER")
	if mode == "" {
		return
	}
	defer os.Exit(0)
	in, _ := io.ReadAll(os.Stdin)
	switch mode {
	case "echo":
		var req struct {
			Listings []map[string]any `json:"listings"`
			Options  map[string]any   `json:"options"`
		}
		if err := json.Unmarshal(in, &req); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(2)
		}
		var scores []string
		for i, l := range req.Listings {
			score := "null"
			if i == 0 {
				score = fmt.Sprintf(`{"baseline":30000,"price":%v,"discountPct":10.5,"basis":"trim","market":"dealer","n":%v}`,
					l["price"], req.Options["model"].(map[string]any)["minComps"])
			}
			scores = append(scores, fmt.Sprintf(`{"id":%q,"score":%s}`, l["id"], score))
		}
		fmt.Printf(`{"appraiser":{"sources":[]},"scores":[%s]}`, strings.Join(scores, ","))
	case "fail":
		fmt.Fprint(os.Stderr, "input must be a JSON array")
		os.Exit(2)
	case "garbage":
		fmt.Print("not json")
	}
}

func helper(mode string) *ExecScorer {
	return &ExecScorer{
		Binary:  os.Args[0],
		Args:    []string{"-test.run=^TestHelperProcess$"},
		Env:     []string{"SCORER_HELPER=" + mode},
		Options: map[string]any{"model": map[string]any{"minComps": 3}},
	}
}

func TestExecScorer(t *testing.T) {
	cars := []listing.Listing{
		{ID: listing.Str("a"), Price: listing.Num(27000)},
		{ID: listing.Str("b"), Price: listing.Num(5)},
	}
	cases := []struct {
		mode    string
		wantErr string
	}{
		{"echo", ""},
		{"fail", "input must be a JSON array"},
		{"garbage", "not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			res, err := helper(c.mode).Score(context.Background(), cars)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			a := res.Scores["a"]
			if a == nil || a.Price != 27000 || a.DiscountPct != 10.5 || a.N != 3 || a.Market != "dealer" {
				t.Errorf("a = %+v", a)
			}
			if s, ok := res.Scores["b"]; !ok || s != nil {
				t.Errorf("b should be present and unscored, got %+v", s)
			}
			if !strings.Contains(string(res.Appraiser), "sources") {
				t.Errorf("appraiser = %s", res.Appraiser)
			}
		})
	}
}

func TestMissingBinary(t *testing.T) {
	_, err := (&ExecScorer{Binary: "definitely-not-a-real-scorer-binary"}).Score(context.Background(), nil)
	if err == nil {
		t.Fatal("want an error")
	}
}

func TestNonePricesNothing(t *testing.T) {
	res, err := None{}.Score(context.Background(), []listing.Listing{{ID: listing.Str("a")}})
	if err != nil || len(res.Scores) != 0 || string(res.Appraiser) != "null" {
		t.Errorf("res=%+v err=%v", res, err)
	}
}
