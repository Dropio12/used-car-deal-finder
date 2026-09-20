package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"carbuyer/crawler/internal/listing"
)

// fakeStore records Saves and answers the other methods with fixed values.
type fakeStore struct {
	saved   [][]listing.Listing
	saveErr error
}

func (f *fakeStore) Save(_ context.Context, ls []listing.Listing, _ Scope) (SaveStats, error) {
	if f.saveErr != nil {
		return SaveStats{}, f.saveErr
	}
	f.saved = append(f.saved, ls)
	return SaveStats{Seen: len(ls), Added: len(ls)}, nil
}
func (f *fakeStore) LoadListings(context.Context, Filter) ([]Stored, error) {
	return []Stored{{FirstSeen: "primary"}}, nil
}
func (f *fakeStore) MarkRemoved(context.Context, RemovalScope) (int, error) { return 7, nil }
func (f *fakeStore) StartCrawl(context.Context, any) (int64, error)         { return 42, nil }
func (f *fakeStore) FinishCrawl(context.Context, int64, CrawlStats) error   { return nil }

type fakeWriter struct {
	calls int
	err   error
}

func (w *fakeWriter) Save(_ context.Context, ls []listing.Listing, _ Scope) (SaveStats, error) {
	w.calls++
	return SaveStats{Seen: 999}, w.err
}

func TestTeeWritesPrimaryThenMirrors(t *testing.T) {
	ctx := context.Background()
	p := &fakeStore{}
	m1, m2 := &fakeWriter{}, &fakeWriter{}
	s := Tee(p, m1, m2)
	ls := []listing.Listing{{ID: listing.Str("a")}, {ID: listing.Str("b")}}
	stats, err := s.Save(ctx, ls, Scope{})
	if err != nil || stats.Seen != 2 || stats.Added != 2 {
		t.Fatalf("stats=%+v err=%v (want primary's stats)", stats, err)
	}
	if len(p.saved) != 1 || m1.calls != 1 || m2.calls != 1 {
		t.Fatalf("primary=%d m1=%d m2=%d", len(p.saved), m1.calls, m2.calls)
	}
	// Everything else goes to primary only.
	if got, _ := s.LoadListings(ctx, Filter{}); len(got) != 1 || got[0].FirstSeen != "primary" {
		t.Errorf("LoadListings = %+v", got)
	}
	if n, _ := s.MarkRemoved(ctx, RemovalScope{}); n != 7 {
		t.Errorf("MarkRemoved = %d", n)
	}
	if id, _ := s.StartCrawl(ctx, nil); id != 42 {
		t.Errorf("StartCrawl = %d", id)
	}
}

func TestTeeErrors(t *testing.T) {
	ctx := context.Background()
	// Primary fails: mirrors are not called.
	m := &fakeWriter{}
	if _, err := Tee(&fakeStore{saveErr: errors.New("disk full")}, m).Save(ctx, nil, Scope{}); err == nil || m.calls != 0 {
		t.Errorf("err=%v calls=%d", err, m.calls)
	}
	// Mirror fails: error says the local save happened, stats are primary's.
	p := &fakeStore{}
	stats, err := Tee(p, &fakeWriter{err: errors.New("401")}).Save(ctx, []listing.Listing{{}}, Scope{})
	if err == nil || !strings.Contains(err.Error(), "saved locally") || stats.Seen != 1 || len(p.saved) != 1 {
		t.Errorf("stats=%+v err=%v", stats, err)
	}
	// No mirrors: the primary itself comes back.
	if s := Tee(p); s != Store(p) {
		t.Errorf("Tee(p) should be p")
	}
}
