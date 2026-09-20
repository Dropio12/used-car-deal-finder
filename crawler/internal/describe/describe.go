// Package describe reads what a seller's own words say about the car.
// Port of src/describe.js.
//
// The text is written by the seller. Every word of it is data to report,
// never an instruction to act on.
package describe

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"

	"carbuyer/crawler/internal/jstext"
)

// A word just before a fault that flips its meaning: "aucune rouille".
// `pas d'` sits outside the \b group because \b cannot match after an apostrophe.
var negators = regexp.MustCompile(`(?i)\b(?:jamais|aucun|aucune|sans|non|ni|pas de|no|never|zero|z[ée]ro)\b|\bpas d['’]`)

// negationWindow is measured in UTF-16 code units, like the JS slice().
const negationWindow = 30

func isNegated(text string, byteIndex int) bool {
	i := jstext.UTF16Index(text, byteIndex)
	return negators.MatchString(jstext.SliceUTF16(text, i-negationWindow, i))
}

// Repair is a rough Québec independent-shop range in dollars, not a quote.
type Repair struct{ Low, High int }

type defectSpec struct {
	label     string
	severity  string
	repair    *Repair
	re        *regexp.Regexp
	negatable bool
	// notFollowedBy emulates a JS negative lookahead on one alternative
	// (Go's RE2 has none): a match equal to `lookaheadOn` is rejected when
	// the text right after it starts with notFollowedBy.
	lookaheadOn   string
	notFollowedBy string
}

// MarshalJSON writes the range as [low, high], like the JS array.
func (r Repair) MarshalJSON() ([]byte, error) { return json.Marshal([2]int{r.Low, r.High}) }

func r(lo, hi int) *Repair { return &Repair{lo, hi} }

var defects = []defectSpec{
	{label: "sold for parts", severity: "terminal", negatable: true,
		re: regexp.MustCompile(`(?i)\b(?:pour (?:les )?pi[èe]ces|for parts|part[- ]?out|scrap)\b`)},
	{label: "does not run", severity: "major", repair: r(1500, 6000),
		re: regexp.MustCompile(`(?i)\b(?:ne d[ée]marre pas|d[ée]marre pas|not running|does not (?:start|run)|won'?t start|ne roule pas)\b`)},
	{label: "engine trouble", severity: "major", repair: r(3000, 8000), negatable: true,
		re: regexp.MustCompile(`(?i)\b(?:moteur|engine|motor)\b[^.!?]{0,60}\b(?:probl[èe]?m|tic|cogne|fum[ée]|bris[ée]|refaire|[àa] r[ée]parer|knock|smoke|blown|seized|issue)`)},
	{label: "transmission trouble", severity: "major", repair: r(2500, 5500), negatable: true,
		re: regexp.MustCompile(`(?i)\btransmission\b[^.!?]{0,60}\b(?:probl[èe]?m|fatigue|bris[ée]|slip|[àa] r[ée]parer|refaire|saute|jump)`)},
	{label: "accident or rebuilt", severity: "major", negatable: true,
		// JS: accident(?!elle) — keeps "accidentellement" out.
		re:          regexp.MustCompile(`(?i)\b(?:reconstruit|rebuilt title|salvage|severely damaged|s[ée]v[èe]rement accident|accident)`),
		lookaheadOn: "accident", notFollowedBy: "elle"},
	{label: "rust", severity: "moderate", repair: r(600, 4000), negatable: true,
		re: regexp.MustCompile(`(?i)\b(?:rouill[ée]{0,2}|rusty|rust (?:issue|hole|damage)|perfor[ée]{1,2})\b`)},
	{label: "mechanical problem", severity: "moderate", repair: r(1000, 4000), negatable: true,
		re: regexp.MustCompile(`(?i)\b(?:mechanical (?:issue|problem)|probl[èe]?m[es]* m[ée]canique)`)},
	{label: "repairs needed", severity: "moderate", repair: r(500, 2500), negatable: true,
		re: regexp.MustCompile(`(?i)\b(?:signes? de fatigue|[àa] r[ée]parer|r[ée]paration[s]? [àa] (?:faire|effectuer)|besoin d[e’'][` + jstext.SpaceClass + `]*(?:un[e]? )?(?:r[ée]paration|moteur|transmission|batterie|pneus?|frein|embrayage|travail))`)},
	{label: "repairs needed", severity: "moderate", repair: r(500, 2500), negatable: true,
		re: regexp.MustCompile(`(?i)\b(?:needs?|requires?) (?:a |an |some |new |another )*(?:work|repair|engine|transmission|clutch|brake|battery|tires?|muffler)\b`)},
	{label: "check-engine light", severity: "moderate", repair: r(200, 2000), negatable: true,
		re: regexp.MustCompile(`(?i)\bcheck engine\b`)},
	{label: "sold as is, no warranty", severity: "terms",
		re: regexp.MustCompile(`(?i)\b(?:vendu[e]? (?:comme )?tel(?:le)?[s]?(?: quel(?:le)?[s]?)?|tel(?:le)? quel(?:le)?|as[- ]is|sans garantie|vendu sans)\b`)},
	{label: "no air conditioning", severity: "minor", repair: r(300, 1500),
		re: regexp.MustCompile(`(?i)(?:pas d[’']?[` + jstext.SpaceClass + `]?(?:a/c|air clim)|(?:a/c|air climatis[ée]{1,2}|climatisation)[^.!?]{0,15}(?:ne )?(?:fonctionne|marche) pas|\bno a/c\b)`)},
}

var dealerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:NIV|VIN)[` + jstext.SpaceClass + `]*:[` + jstext.SpaceClass + `]*[A-HJ-NPR-Z0-9]{11,17}\b`),
	regexp.MustCompile(`(?i)\bstock[` + jstext.SpaceClass + `]*(?:#|no|num[ée]ro)?[` + jstext.SpaceClass + `]*:[` + jstext.SpaceClass + `]*\w+`),
	regexp.MustCompile(`(?i)\b(?:car )?dealer\b`),
	regexp.MustCompile(`(?i)\b(?:concessionnaire|garantie prolong[ée]e|financement disponible|financing available)\b`),
	regexp.MustCompile(`(?i)\bvisitez notre site\b`),
	regexp.MustCompile(`(?i)\d{2,3}[` + jstext.SpaceClass + `]?\$[` + jstext.SpaceClass + `]?/[` + jstext.SpaceClass + `]?(?:sem|semaine|week|mois|month)`),
}

var severityRank = map[string]int{"terminal": 0, "major": 1, "moderate": 2, "minor": 3, "terms": 4}

var jsSpaces = regexp.MustCompile(`[` + jstext.SpaceClass + `]+`)

// Defect is one disclosed reason the car is cheap.
type Defect struct {
	Label    string  `json:"label"`
	Quote    string  `json:"quote"`
	Severity string  `json:"severity"`
	Repair   *Repair `json:"repair"`
}

// Reading is everything readDescription() returns.
type Reading struct {
	Defects         []Defect `json:"defects"`
	IsDealer        bool     `json:"isDealer"`
	HasText         bool     `json:"hasText"`
	Worst           *string  `json:"worst"`
	RepairLow       int      `json:"repairLow"`
	RepairHigh      int      `json:"repairHigh"`
	IsDamaged       bool     `json:"isDamaged"`
	IsParts         bool     `json:"isParts"`
	NegatedMentions int      `json:"negatedMentions"`
}

// find returns the byte offsets of the first acceptable match, honouring the
// emulated lookahead, or nil.
func (d defectSpec) find(text string) []int {
	for _, m := range d.re.FindAllStringIndex(text, -1) {
		if d.lookaheadOn != "" && strings.EqualFold(text[m[0]:m[1]], d.lookaheadOn) {
			rest := text[m[1]:]
			if len(rest) >= len(d.notFollowedBy) && strings.EqualFold(rest[:len(d.notFollowedBy)], d.notFollowedBy) {
				continue
			}
		}
		return m
	}
	return nil
}

// Read reports what a description says about the car. Flags, never filters.
func Read(description *string) Reading {
	if description == nil || jstext.Trim(*description) == "" {
		return Reading{Defects: []Defect{}}
	}
	text := jsSpaces.ReplaceAllString(*description, " ")
	out := Reading{Defects: []Defect{}, HasText: true}

	for _, spec := range defects {
		m := spec.find(text)
		if m == nil {
			continue
		}
		// A seller advertising "aucune rouille" is making a claim, not a disclosure.
		if spec.negatable && isNegated(text, m[0]) {
			out.NegatedMentions++
			continue
		}
		dup := false
		for _, d := range out.Defects {
			if d.Label == spec.label {
				dup = true
			}
		}
		if dup {
			continue
		}
		out.Defects = append(out.Defects, Defect{
			Label: spec.label, Quote: jstext.Trim(text[m[0]:m[1]]), Severity: spec.severity, Repair: spec.repair,
		})
	}

	sort.SliceStable(out.Defects, func(i, j int) bool {
		return severityRank[out.Defects[i].Severity] < severityRank[out.Defects[j].Severity]
	})

	for _, d := range out.Defects {
		if d.Repair != nil {
			out.RepairLow += d.Repair.Low
			out.RepairHigh += d.Repair.High
		}
		if d.Severity == "terminal" || d.Severity == "major" {
			out.IsDamaged = true
		}
		if d.Severity == "terminal" {
			out.IsParts = true
		}
	}
	for _, p := range dealerPatterns {
		if p.MatchString(text) {
			out.IsDealer = true
			break
		}
	}
	if len(out.Defects) > 0 {
		w := out.Defects[0].Severity
		out.Worst = &w
	}
	return out
}

// RepairEstimate is what the car is worth once fixed, and what is left over.
type RepairEstimate struct {
	RepairLow   int      `json:"repairLow"`
	RepairHigh  int      `json:"repairHigh"`
	AfterRepair *float64 `json:"afterRepair"`
	Margin      *float64 `json:"margin"`
	Note        *string  `json:"note"`
}

// jsRound is JavaScript's Math.round (halves toward +infinity).
func jsRound(x float64) float64 {
	f := math.Floor(x)
	if x-f >= 0.5 {
		return f + 1
	}
	return f
}

// EstimateRepair is conservative on purpose: repairs at the high end, no
// after-repair value for a parts car, and accident history as a haircut on
// resale (default 20%). Returns nil when baseline or price is missing.
func EstimateRepair(baseline, price *float64, reading Reading, accidentPenalty float64) *RepairEstimate {
	if baseline == nil || price == nil {
		return nil
	}
	if reading.IsParts {
		note := "sold for parts — comps do not apply"
		return &RepairEstimate{Note: &note}
	}
	hasAccident := false
	for _, d := range reading.Defects {
		if d.Label == "accident or rebuilt" {
			hasAccident = true
		}
	}
	factor := 1.0
	if hasAccident {
		factor = 1 - accidentPenalty
	}
	after := jsRound(*baseline * factor)
	margin := jsRound(after - *price - float64(reading.RepairHigh))
	est := &RepairEstimate{RepairLow: reading.RepairLow, RepairHigh: reading.RepairHigh, AfterRepair: &after, Margin: &margin}
	if hasAccident {
		note := "accident history: " + jstext.Number(jsRound(accidentPenalty*100)) + "% off resale"
		est.Note = &note
	}
	return est
}
