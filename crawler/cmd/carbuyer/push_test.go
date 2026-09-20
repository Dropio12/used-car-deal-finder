package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestPushWithoutTokenExits2(t *testing.T) {
	t.Setenv(TokenEnv, "")
	dbPath := filepath.Join(t.TempDir(), "x.db")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--push", "https://api.example.dev", "--db", dbPath}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), TokenEnv) {
		t.Errorf("code=%d stderr=%q", code, errOut.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("a bad --push must fail before the database is created")
	}
}

func TestPushRefusesPlainHTTP(t *testing.T) {
	t.Setenv(TokenEnv, "tok")
	var out, errOut bytes.Buffer
	args := []string{"--push", "http://api.example.dev", "--db", filepath.Join(t.TempDir(), "x.db")}
	if code := run(context.Background(), args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "https") {
		t.Errorf("code=%d stderr=%q", code, errOut.String())
	}
}

// TestPushOfflineFixture runs the whole CLI on the saved page with --push
// pointed at a fake Worker, and checks every stored listing was pushed with
// the token. It needs the real scorer binary (cargo build --release).
func TestPushOfflineFixture(t *testing.T) {
	name := "carbuyer-scorer"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	scorer, _ := filepath.Abs(filepath.Join("..", "..", "..", "scorer", "target", "release", name))
	if _, err := os.Stat(scorer); err != nil {
		t.Skip("scorer binary not built: cd scorer && cargo build --release")
	}

	var mu sync.Mutex
	var ids []string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Listings []struct {
				ID string `json:"id"`
			} `json:"listings"`
			Scope map[string]any `json:"scope"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		auth = r.Header.Get("Authorization")
		for _, l := range body.Listings {
			ids = append(ids, l.ID)
		}
		mu.Unlock()
		if body.Scope["makeSlug"] != "toyota" || body.Scope["modelSlug"] != "rav4" {
			http.Error(w, "bad scope", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]int{"seen": len(body.Listings), "added": len(body.Listings)})
	}))
	defer srv.Close()

	t.Setenv(TokenEnv, "secret-tok")
	var out, errOut bytes.Buffer
	args := []string{"--make", "toyota", "--model", "rav4", "--offline", filepath.Join("..", "..", "testdata", "rav4-qc.html"),
		"--min-comps", "3", "--db", filepath.Join(t.TempDir(), "x.db"), "--scorer", scorer, "--push", srv.URL}
	if code := run(context.Background(), args, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if len(ids) != 20 || auth != "Bearer secret-tok" {
		t.Errorf("pushed %d listings, auth %q", len(ids), auth)
	}
	if !strings.Contains(errOut.String(), "20 seen, 20 added") || !strings.Contains(out.String(), "Best deals") {
		t.Errorf("stderr=%q", errOut.String())
	}
}
