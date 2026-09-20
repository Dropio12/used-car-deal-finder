package geo

import "testing"

func s(v string) *string   { return &v }
func f(v float64) *float64 { return &v }

func TestHaversineKm(t *testing.T) {
	cases := []struct {
		name string
		to   Point
		min  float64
		max  float64
	}{
		{"same point", Home, 0, 0},
		{"Québec City", Point{46.8139, -71.2080}, 225, 240},
		{"Sherbrooke", Point{45.4042, -71.8929}, 125, 135},
	}
	for _, c := range cases {
		got := HaversineKm(Home, c.to)
		if got < c.min || got > c.max {
			t.Errorf("%s: %v km", c.name, got)
		}
	}
}

func TestFSAOf(t *testing.T) {
	cases := map[*string]string{s("h2y 1c6"): "H2Y", s("J4K0A0"): "J4K", s("H2"): "", nil: ""}
	for in, want := range cases {
		if got := FSAOf(in); got != want {
			t.Errorf("FSAOf(%v) = %q", in, got)
		}
	}
}

func TestFromHome(t *testing.T) {
	index := BuildFSAIndex([]Located{
		{PostalCode: s("G1R 1A1"), Latitude: f(46.80), Longitude: f(-71.20)},
		{PostalCode: s("G1R 2B2"), Latitude: f(46.82), Longitude: f(-71.22)},
		{PostalCode: s("J1H 1A1"), Latitude: f(45.40)}, // no longitude: ignored
	}, 1)
	if c := index["G1R"]; c.N != 2 {
		t.Fatalf("G1R centre = %+v", c)
	}
	cases := []struct {
		name  string
		l     Located
		basis string
	}{
		{"coordinates win", Located{PostalCode: s("G1R 9Z9"), Latitude: f(45.5), Longitude: f(-73.5)}, "coordinates"},
		{"FSA fallback", Located{PostalCode: s("G1R 9Z9")}, "fsa"},
		{"unknown, not far", Located{PostalCode: s("X0A 0A0")}, "unknown"},
		{"no postal code", Located{}, "unknown"},
	}
	for _, c := range cases {
		d := FromHome(c.l, index, Home)
		if d.Basis != c.basis || (c.basis == "unknown") != (d.Km == nil) {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
	if d := FromHome(Located{PostalCode: s("G1R 9Z9")}, index, Home); *d.Km < 220 || d.From != 2 {
		t.Errorf("Québec City FSA distance: %+v", d)
	}
	if len(BuildFSAIndex([]Located{{PostalCode: s("G1R 1A1"), Latitude: f(1), Longitude: f(1)}}, 2)) != 0 {
		t.Error("minPoints not enforced")
	}
}
