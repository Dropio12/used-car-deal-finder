// Package geo says how far a listing is from home. Port of src/geo.js.
//
// AutoHebdo publishes a postal code and no coordinates, so distance falls back
// to the centre of the listing's forward sortation area (first 3 characters),
// learned from listings that carry both. FSA-level accuracy is the right
// resolution for "is this worth driving to", and no finer.
package geo

import (
	"math"
	"strings"
)

// Point is a latitude/longitude pair.
type Point struct{ Latitude, Longitude float64 }

// Home is H2Y 1C6.
var Home = Point{Latitude: 45.5155, Longitude: -73.5619}

const earthRadiusKm = 6371

func jsRound(x float64) float64 {
	f := math.Floor(x)
	if x-f >= 0.5 {
		return f + 1
	}
	return f
}

// HaversineKm is the straight-line distance, rounded to whole km. A real drive is longer.
func HaversineKm(a, b Point) float64 {
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := rad(b.Latitude - a.Latitude)
	dLon := rad(b.Longitude - a.Longitude)
	h := math.Pow(math.Sin(dLat/2), 2) + math.Cos(rad(a.Latitude))*math.Cos(rad(b.Latitude))*math.Pow(math.Sin(dLon/2), 2)
	return jsRound(earthRadiusKm * 2 * math.Atan2(math.Sqrt(h), math.Sqrt(1-h)))
}

// FSAOf returns the forward sortation area ("H2X"), or "" when there is none.
func FSAOf(postalCode *string) string {
	if postalCode == nil || len([]rune(*postalCode)) < 3 {
		return ""
	}
	return strings.ToUpper(string([]rune(*postalCode)[:3]))
}

// Located is anything with an optional postal code and optional coordinates.
type Located struct {
	PostalCode *string
	Latitude   *float64
	Longitude  *float64
}

// Centre is the averaged position of one FSA and how many points made it.
type Centre struct {
	Point
	N int
}

// BuildFSAIndex averages the coordinates of every listing sharing an FSA.
// minPoints guards against one mislocated listing defining a whole region.
func BuildFSAIndex(listings []Located, minPoints int) map[string]Centre {
	type acc struct {
		lat, lon float64
		n        int
	}
	sums := map[string]*acc{}
	for _, l := range listings {
		fsa := FSAOf(l.PostalCode)
		if fsa == "" || l.Latitude == nil || l.Longitude == nil {
			continue
		}
		a, ok := sums[fsa]
		if !ok {
			a = &acc{}
			sums[fsa] = a
		}
		a.lat += *l.Latitude
		a.lon += *l.Longitude
		a.n++
	}
	index := map[string]Centre{}
	for fsa, a := range sums {
		if a.n < minPoints {
			continue
		}
		index[fsa] = Centre{Point: Point{a.lat / float64(a.n), a.lon / float64(a.n)}, N: a.n}
	}
	return index
}

// Distance is a distance from home and what it rests on.
type Distance struct {
	Km    *float64
	Basis string // "coordinates" | "fsa" | "unknown"
	FSA   string
	From  int
}

// FromHome prefers real coordinates, falls back to the FSA centre, and says
// "unknown" rather than "far" when it has neither.
func FromHome(l Located, index map[string]Centre, home Point) Distance {
	if l.Latitude != nil && l.Longitude != nil {
		km := HaversineKm(home, Point{*l.Latitude, *l.Longitude})
		return Distance{Km: &km, Basis: "coordinates"}
	}
	fsa := FSAOf(l.PostalCode)
	if c, ok := index[fsa]; ok && fsa != "" {
		km := HaversineKm(home, c.Point)
		return Distance{Km: &km, Basis: "fsa", FSA: fsa, From: c.N}
	}
	return Distance{Basis: "unknown"}
}
