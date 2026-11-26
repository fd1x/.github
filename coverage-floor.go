// Command coverage-floor fails when a Cobertura report's line coverage under the given source
// prefixes falls below a floor.
//
// Usage: COVERAGE_FLOOR=80 COVERAGE_PREFIXES=$'src/core/\nsrc/world/' coverage-floor <report.xml>
//
// COVERAGE_FLOOR is a percentage; COVERAGE_PREFIXES is newline-separated and matched
// case-insensitively against each class's filename, backslashes read as slashes. The gate is on
// the overall rate across every prefix; each prefix's own rate is printed for information.
//
// This file is standard library only and self-contained: the godot-test reusable workflow runs it
// with `go run` from the public `fd1x/.github` copy, outside any module, so it must not import
// anything else or gain a sibling.
package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// element is the part of an XML element the gate reads: unqualified names only, as
// ElementTree's `findall("class")` in the Python this replaced did not match a namespaced tag.
type element struct {
	name     string
	attrs    map[string]string
	children []*element
}

// bucket counts one prefix's lines: covered (hits > 0) and valid (every line).
type bucket struct {
	covered, valid int
}

// run is main without the process: it returns the exit code (0 at or above the floor, 1 below it
// or on any error, 2 a usage error).
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: coverage-floor <coverage.cobertura.xml>")
		return 2
	}

	root, err := parse(args[0])
	if err != nil {
		fmt.Fprintf(stdout, "::error::cannot read the coverage report: %v\n", err)
		return 1
	}

	floorPercent, err := strconv.ParseFloat(strings.TrimFunc(getenv("COVERAGE_FLOOR"), isPythonSpace), 64)
	if err != nil {
		fmt.Fprintf(stdout, "::error::COVERAGE_FLOOR is not a number: %q\n", getenv("COVERAGE_FLOOR"))
		return 1
	}
	floor := floorPercent / 100

	var prefixes []string
	for _, line := range strings.FieldsFunc(getenv("COVERAGE_PREFIXES"), isLineBreak) {
		if p := strings.ToLower(strings.TrimFunc(line, isPythonSpace)); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	if len(prefixes) == 0 {
		fmt.Fprintln(stdout, "::error::coverage-floor is set but coverage-prefixes is empty")
		return 1
	}

	// Per-prefix buckets, so a failure names the module that slipped.
	buckets := map[string]*bucket{}
	for _, p := range prefixes {
		buckets[p] = &bucket{}
	}

	for _, class := range descendants(root, "class") {
		filename := strings.ToLower(strings.ReplaceAll(class.attrs["filename"], `\`, "/"))
		// The first prefix listed wins when two overlap.
		i := slices.IndexFunc(prefixes, func(p string) bool { return strings.HasPrefix(filename, p) })
		if i < 0 {
			continue
		}
		b := buckets[prefixes[i]]
		for _, lines := range children(class, "lines") {
			for _, line := range children(lines, "line") {
				hits, ok := line.attrs["hits"]
				if !ok {
					hits = "0"
				}
				n, ok := new(big.Int).SetString(strings.TrimFunc(hits, isPythonSpace), 10)
				if !ok {
					fmt.Fprintf(stdout, "::error::%s has a line whose hits is not an integer: %q\n", class.attrs["filename"], hits)
					return 1
				}
				b.valid++
				if n.Sign() > 0 {
					b.covered++
				}
			}
		}
	}

	covered, valid := 0, 0
	for _, b := range buckets {
		covered += b.covered
		valid += b.valid
	}
	if valid == 0 {
		fmt.Fprintln(stdout, "::error::no coverage data matched any of: "+strings.Join(prefixes, ", "))
		return 1
	}

	overall := float64(covered) / float64(valid)
	fmt.Fprintf(stdout, "overall: %.2f%% (%d/%d)\n", overall*100, covered, valid)
	names := make([]string, 0, len(buckets))
	for p := range buckets {
		names = append(names, p)
	}
	slices.Sort(names)
	for _, p := range names {
		b := buckets[p]
		if b.valid == 0 {
			fmt.Fprintf(stdout, "  %s: no data\n", p)
			continue
		}
		rate := float64(b.covered) / float64(b.valid)
		marker := ""
		if rate < floor {
			marker = fmt.Sprintf("  [below %.0f%%]", floor*100)
		}
		fmt.Fprintf(stdout, "  %s: %.2f%% (%d/%d)%s\n", p, rate*100, b.covered, b.valid, marker)
	}

	// The gate is on the overall rate; the per-prefix marker above is informational until every
	// module clears the floor on its own.
	if overall >= floor {
		return 0
	}
	return 1
}

// parse reads the report into a tree. encoding/xml resolves no external entities, and this report
// comes from the run.
func parse(path string) (*element, error) {
	f, err := os.Open(path) //nolint:gosec // G703: the workflow names the report to read
	if err != nil {
		return nil, err
	}
	defer f.Close()

	decoder := xml.NewDecoder(f) //nolint:gosec // G709: see above; encoding/xml expands no external entities
	var root *element
	var stack []*element
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			e := &element{attrs: map[string]string{}}
			if t.Name.Space == "" {
				e.name = t.Name.Local
			}
			for _, a := range t.Attr {
				if a.Name.Space == "" {
					e.attrs[a.Name.Local] = a.Value
				}
			}
			if len(stack) == 0 {
				// encoding/xml accepts a second top-level element; ElementTree rejected it.
				if root != nil {
					return nil, fmt.Errorf("%s: content after the document element", path)
				}
				root = e
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, e)
			}
			stack = append(stack, e)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 && len(strings.TrimSpace(string(t))) > 0 {
				return nil, fmt.Errorf("%s: text outside the document element", path)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("%s: no root element", path)
	}
	return root, nil
}

// descendants returns every element below e (e itself excluded) named name, as `.//name`.
func descendants(e *element, name string) []*element {
	var found []*element
	for _, c := range e.children {
		if c.name == name {
			found = append(found, c)
		}
		found = append(found, descendants(c, name)...)
	}
	return found
}

// children returns e's direct children named name, as `./name`.
func children(e *element, name string) []*element {
	var found []*element
	for _, c := range e.children {
		if c.name == name {
			found = append(found, c)
		}
	}
	return found
}

// isLineBreak matches the separators Python's str.splitlines splits on.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
		return true
	}
	return false
}

// isPythonSpace matches what Python's str.strip removes, which adds \x1c-\x1f to unicode.IsSpace.
func isPythonSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= '\x1c' && r <= '\x1f')
}
