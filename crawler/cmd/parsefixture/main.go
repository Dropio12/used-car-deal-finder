// Command parsefixture parses a saved AutoHebdo search page and prints the
// result as JSON. Used by the parity check against the original JS parser.
//
//	go run ./cmd/parsefixture testdata/rav4-qc.html
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"carbuyer/crawler/internal/parse"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: parsefixture <page.html>")
		os.Exit(2)
	}
	html, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	page, err := parse.SearchPage(string(html))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(page); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
