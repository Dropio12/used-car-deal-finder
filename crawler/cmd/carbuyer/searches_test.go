package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func writeSearches(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "searches.yml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var fixturePath = filepath.Join("..", "..", "testdata", "rav4-qc.html")

// --searches runs each entry in order, here offline against the saved page,
// with --no-score so no scorer binary is needed, and pushes to a fake Worker.
func TestSearchesRunAllAndPush(t *testing.T) {
	var posts, listings atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Listings []json.RawMessage `json:"listings"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		posts.Add(1)
		listings.Add(int64(len(body.Listings)))
		_ = json.NewEncoder(w).Encode(map[string]int{"seen": len(body.Listings), "added": len(body.Listings), "newAlerts": 1})
	}))
	defer srv.Close()
	t.Setenv(TokenEnv, "tok")

	file := writeSearches(t, "searches:\n  - make: toyota\n    model: rav4\n  - make: toyota\n    model: rav4\n    name: again\n    maxPrice: 40000\n")
	var out, errOut bytes.Buffer
	args := []string{"--searches", file, "--no-score", "--offline", fixturePath,
		"--db", filepath.Join(t.TempDir(), "x.db"), "--push", srv.URL}
	if code := run(context.Background(), args, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "toyota rav4: 20 listings") || !strings.HasPrefix(lines[1], "again: 20 listings") {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.Contains(lines[0], "20 new") || !strings.Contains(lines[1], "0 new") || !strings.Contains(lines[0], "0 priced locally") {
		t.Errorf("second run should find the same 20 cars already stored: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "[2/2] again") || listings.Load() != 40 || posts.Load() < 2 ||
		!strings.Contains(errOut.String(), fmt.Sprintf(", %d new deal alerts", posts.Load())) {
		t.Errorf("posts=%d listings=%d stderr=%s", posts.Load(), listings.Load(), errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), "tok") {
		t.Error("the token must never be printed")
	}
}

// A failing search stops the run: later searches never send a request.
func TestSearchesStopAtFirstError(t *testing.T) {
	// The site answers with a block page instead of search results.
	blocked := filepath.Join(t.TempDir(), "blocked.html")
	if err := os.WriteFile(blocked, []byte("<html><body>Access denied</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := writeSearches(t, "searches:\n  - make: honda\n    model: civic\n  - make: toyota\n    model: rav4\n")
	var out, errOut bytes.Buffer
	args := []string{"--searches", file, "--no-score", "--offline", blocked, "--db", filepath.Join(t.TempDir(), "x.db")}
	code := run(context.Background(), args, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "stopping: 0 of 2 searches done") || strings.Contains(errOut.String(), "[2/2]") {
		t.Errorf("code=%d stderr=%s", code, errOut.String())
	}
}

func TestSearchesJSON(t *testing.T) {
	file := writeSearches(t, "searches:\n  - make: toyota\n    model: rav4\n")
	var out, errOut bytes.Buffer
	args := []string{"--searches", file, "--no-score", "--json", "--offline", fixturePath, "--db", filepath.Join(t.TempDir(), "x.db")}
	if code := run(context.Background(), args, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var res []struct {
		Name   string `json:"name"`
		Report struct {
			Deals []json.RawMessage `json:"deals"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res) != 1 || res[0].Name != "toyota rav4" || len(res[0].Report.Deals) != 20 {
		t.Errorf("err=%v res=%+v", err, res)
	}
}

func TestSearchesBadUsageExits2(t *testing.T) {
	good := writeSearches(t, "searches:\n  - make: toyota\n")
	cases := map[string][]string{
		"with --make":  {"--searches", good, "--make", "toyota"},
		"missing file": {"--searches", filepath.Join(t.TempDir(), "nope.yml")},
		"bad file":     {"--searches", writeSearches(t, "searches:\n  - make: toyota\n    colour: red\n")},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(context.Background(), append(args, "--db", filepath.Join(t.TempDir(), "x.db")), &out, &errOut); code != 2 {
				t.Errorf("code=%d stderr=%s", code, errOut.String())
			}
		})
	}
}
