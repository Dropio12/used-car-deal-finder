package describe

import (
	"slices"
	"testing"
)

func labels(text string) []string {
	out := []string{}
	for _, d := range Read(&text).Defects {
		out = append(out, d.Label)
	}
	return out
}

// Every string is real text from a live Québec listing (from test/describe.test.js).
func TestReadLabels(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		has     string // label that must be present ("" = none)
		hasNot  string // label that must be absent
		noneAll bool   // no defects at all
	}{
		{name: "no rust boast", text: "Aucune rouille, toujours remisée l'hiver", noneAll: true},
		{name: "no rust english", text: "No rust at all, southern car", noneAll: true},
		{name: "jamais accidenté", text: "Jamais accidenté, toujours entretenue", noneAll: true},
		{name: "jamais eu d'accident", text: "n'a jamais eu d'accident", noneAll: true},
		{name: "aucun accident", text: "aucun accident, carfax propre", noneAll: true},
		{name: "aucun problème mécanique", text: "Aucun problème mécanique, prête à rouler", noneAll: true},
		{name: "real accident", text: "Voiture accidentée, vendue pour pièces", has: "accident or rebuilt"},
		{name: "real rust", text: "beaucoup de rouille sur les bas de caisse", has: "rust"},
		{name: "real mechanical", text: "problème mécanique important", has: "mechanical problem"},
		{name: "far negation", text: "Pas cher pour le modèle. Très bonne voiture. Il y a de la rouille sur les ailes.", has: "rust"},
		{name: "sans garantie opts out of negation", text: "Vendue sans garantie légale", has: "sold as is, no warranty"},
		{name: "pas d'accident is not a/c", text: "Pas d'accident, propriétaire unique", hasNot: "no air conditioning"},
		{name: "real a/c fault", text: "l'air climatisé ne fonctionne pas", has: "no air conditioning"},
		{name: "pas d'a/c", text: "pas d'a/c", has: "no air conditioning"},
		{name: "besoin d'un VUS", text: "Je vends car besoin d'un VUS, la famille s'agrandit", noneAll: true},
		{name: "besoin d'une transmission", text: "besoin d'une transmission", has: "repairs needed"},
		{name: "needs a new battery", text: "needs a new battery", has: "repairs needed"},
		{name: "accidenté", text: "véhicule accidenté", has: "accident or rebuilt"},
		{name: "accidentée", text: "véhicule accidentée", has: "accident or rebuilt"},
		{name: "accidentellement is not an accident", text: "rayé accidentellement", hasNot: "accident or rebuilt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := labels(c.text)
			if c.noneAll && len(got) != 0 {
				t.Errorf("want none, got %v", got)
			}
			if c.has != "" && !slices.Contains(got, c.has) {
				t.Errorf("want %q in %v", c.has, got)
			}
			if c.hasNot != "" && slices.Contains(got, c.hasNot) {
				t.Errorf("did not want %q in %v", c.hasNot, got)
			}
		})
	}
}

func TestNegatedMentionsCounted(t *testing.T) {
	a, b := "Aucune rouille", "beaucoup de rouille"
	if Read(&a).NegatedMentions != 1 || Read(&b).NegatedMentions != 0 {
		t.Error("negated mentions miscounted")
	}
}

func TestSeverityAndDamage(t *testing.T) {
	cases := []struct {
		text              string
		damaged, parts    bool
		worst, firstLabel string
	}{
		{"moteur brisé, à réparer", true, false, "major", ""},
		{"Très propre, bien entretenue", false, false, "", ""},
		{"Vendu pour les pièces, ne démarre pas", true, true, "terminal", ""},
		{"Rouille sur les ailes et le moteur cogne, pas d'a/c", true, false, "major", "engine trouble"},
		{"Vendu tel quel", false, false, "terms", ""},
	}
	for _, c := range cases {
		r := Read(&c.text)
		worst := ""
		if r.Worst != nil {
			worst = *r.Worst
		}
		if r.IsDamaged != c.damaged || r.IsParts != c.parts || worst != c.worst {
			t.Errorf("%q: damaged=%v parts=%v worst=%q", c.text, r.IsDamaged, r.IsParts, worst)
		}
		if c.firstLabel != "" && r.Defects[0].Label != c.firstLabel {
			t.Errorf("%q: first = %q", c.text, r.Defects[0].Label)
		}
	}
}

func TestRepairCosts(t *testing.T) {
	cases := []struct {
		text      string
		low, high int
	}{
		{"transmission qui saute et de la rouille partout", 2500 + 600, 5500 + 4000},
		{"véhicule reconstruit", 0, 0},
		{"Très propre, bien entretenue", 0, 0},
	}
	for _, c := range cases {
		r := Read(&c.text)
		if r.RepairLow != c.low || r.RepairHigh != c.high {
			t.Errorf("%q: %d-%d", c.text, r.RepairLow, r.RepairHigh)
		}
	}
}

func TestEmptyText(t *testing.T) {
	blank := "   "
	for _, d := range []*string{nil, &blank} {
		if r := Read(d); r.HasText || len(r.Defects) != 0 {
			t.Errorf("%v: %+v", d, r)
		}
	}
}

func TestEstimateRepair(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	read := func(s string) Reading { return Read(&s) }

	e := EstimateRepair(f(10_000), f(7000), read("Très propre"), 0.2)
	if *e.AfterRepair != 10_000 || *e.Margin != 3000 {
		t.Errorf("clean: %+v", e)
	}
	e = EstimateRepair(f(10_000), f(4000), read("transmission qui saute"), 0.2)
	if e.RepairHigh != 5500 || *e.Margin != 10_000-4000-5500 {
		t.Errorf("transmission: %+v", e)
	}
	e = EstimateRepair(f(10_000), f(5000), read("véhicule accidenté réparé"), 0.2)
	if *e.AfterRepair != 8000 || e.Note == nil || *e.Note != "accident history: 20% off resale" {
		t.Errorf("accident: %+v", e)
	}
	e = EstimateRepair(f(5000), f(800), read("pour les pièces"), 0.2)
	if e.AfterRepair != nil || e.Margin != nil {
		t.Errorf("parts: %+v", e)
	}
	if EstimateRepair(nil, f(1), read("x"), 0.2) != nil {
		t.Error("missing baseline must give nil")
	}
}
