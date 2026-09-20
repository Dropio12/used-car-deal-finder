package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "--make") {
		t.Errorf("code=%d out=%q", code, out.String())
	}
}

func TestQueryFlags(t *testing.T) {
	cases := []struct {
		args    []string
		check   func(o options) bool
		wantErr bool
	}{
		{[]string{"--make", "toyota", "--model", "rav4", "--private", "--max-price", "15000", "--min-year", "2018"}, func(o options) bool {
			q, err := o.query()
			return err == nil && q.SellerType == "P" && *q.PriceTo == 15000 && *q.YearFrom == 2018 && q.Geo == "reg_qc" && q.MaxPages == 1
		}, false},
		{[]string{"--dealer", "--pages", "3"}, func(o options) bool {
			q, _ := o.query()
			return q.SellerType == "D" && q.MaxPages == 3
		}, false},
		{[]string{"--max-price", "cheap"}, func(o options) bool {
			_, err := o.query()
			return err != nil
		}, false},
		{[]string{"--nope"}, nil, true},
	}
	for _, c := range cases {
		o, err := parseFlags(c.args, &bytes.Buffer{})
		if (err != nil) != c.wantErr {
			t.Errorf("%v: err = %v", c.args, err)
			continue
		}
		if c.check != nil && !c.check(o) {
			t.Errorf("%v: %+v", c.args, o)
		}
	}
}

func TestBadNumberExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	args := []string{"--max-price", "cheap", "--db", filepath.Join(t.TempDir(), "x.db")}
	if code := run(context.Background(), args, &out, &errOut); code != 2 {
		t.Errorf("code = %d", code)
	}
}

func TestMoney(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		in   *float64
		want string
	}{{f(33490), "33 490"}, {f(999), "999"}, {f(1234567), "1 234 567"}, {f(-1500), "-1 500"}, {nil, "—"}}
	for _, c := range cases {
		if got := money(c.in); got != c.want {
			t.Errorf("money(%v) = %q", c.in, got)
		}
	}
}
