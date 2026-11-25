// Command terraform-see-links fails when a `# see` link names a version the module does not allow.
//
// The fleet requires every `resource` and `data` block to carry a `# see` comment linking to its
// Terraform Registry documentation, pinned to the provider's constraint floor. Nothing else checks
// that: a provider bump moves `required_providers` and leaves every link behind, still pointing at
// docs for a version the workspace no longer allows. `latest` is the same drift with no visible
// symptom.
//
// Usage: terraform-see-links [--fix] [path ...]
//
// This file is standard library only and self-contained: CI runs it with `go run` from the public
// `fd1x/.github` copy, outside any module, so it must not import anything else or gain a sibling.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

var (
	// `# see https://registry.terraform.io/providers/<namespace>/<name>/<version>/docs/...`
	// The trailing path is not captured: this checks the version segment, not whether the page exists.
	linkPattern = regexp.MustCompile(`registry\.terraform\.io/providers/([^/\s]+)/([^/\s]+)/([^/\s#]+)`)

	// A `required_providers` entry: `tfe = { source = "..." version = "..." }`. Provider entries
	// hold no nested braces, so `[^{}]*` is enough and avoids depending on an HCL parser.
	entryPattern   = regexp.MustCompile(`[A-Za-z0-9_-]+\s*=\s*\{([^{}]*)\}`)
	sourcePattern  = regexp.MustCompile(`source\s*=\s*"([^"]+)"`)
	versionPattern = regexp.MustCompile(`version\s*=\s*"([^"]+)"`)

	// The floor of `>= 0.80.0, < 1.0.0`. Only the `>=` bound is the documented version.
	floorPattern = regexp.MustCompile(`>=\s*(\d+\.\d+\.\d+)`)

	blockPattern = regexp.MustCompile(`\brequired_providers\s*\{`)
)

const usageLine = "usage: terraform-see-links [-h] [--fix] [paths ...]"

const usage = usageLine + `

Fail when a ` + "`# see`" + ` link names a version the module does not allow.

positional arguments:
  paths       directories to scan (default: .)

options:
  -h, --help  show this help message and exit
  --fix       rewrite stale links instead of reporting them
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process: it returns the exit code (0 clean or fixed, 1 drift or a read
// error, 2 a usage error).
func run(args []string, stdout, stderr io.Writer) int {
	// Parsed as argparse parses the original: the paths are one run of positionals, so a path after
	// an option that follows them (`. --fix agents`) is rejected, not appended.
	fix := false
	var roots, unrecognized []string
	onlyPaths, pathsDone := false, false
	for _, arg := range args {
		isOption := !onlyPaths && strings.HasPrefix(arg, "-") && arg != "-" && arg != "--"
		switch {
		case !onlyPaths && arg == "--":
			onlyPaths = true
		case !isOption && pathsDone:
			unrecognized = append(unrecognized, arg)
		case !isOption:
			roots = append(roots, arg)
		case arg == "-h" || isPrefixOf(arg, "--help"):
			fmt.Fprint(stdout, usage)
			return 0
		case isPrefixOf(arg, "--fix"):
			fix = true
			pathsDone = pathsDone || len(roots) > 0
		default:
			unrecognized = append(unrecognized, arg)
			pathsDone = pathsDone || len(roots) > 0
		}
	}
	if len(unrecognized) > 0 {
		fmt.Fprintf(stderr, "%s\nterraform-see-links: error: unrecognized arguments: %s\n", usageLine, strings.Join(unrecognized, " "))
		return 2
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}

	var files []string
	for _, root := range roots {
		files = append(files, terraformFiles(root)...)
	}

	byDir := map[string]map[string]string{}
	contents := map[string]string{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		contents[path] = normalizeNewlines(string(raw))
		declared := requiredProviders(contents[path])
		if len(declared) == 0 {
			continue
		}
		dir := filepath.Dir(path)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		for source, floor := range declared {
			byDir[dir][source] = floor
		}
	}

	var problems []string
	fixed := 0

	for _, path := range files {
		text := contents[path]
		expected := constraintsFor(path, byDir)
		if len(expected) == 0 {
			continue
		}

		var replacements [][2]string
		for index, line := range splitLines(text) {
			for _, link := range linkPattern.FindAllStringSubmatch(line, -1) {
				namespace, name, actual := link[1], link[2], link[3]
				source := strings.ToLower(namespace + "/" + name)
				floor, ok := expected[source]
				// A link to a provider this root doesn't declare is a cross-reference, not drift.
				if !ok || actual == floor {
					continue
				}

				if fix {
					replacements = append(replacements, [2]string{
						"providers/" + namespace + "/" + name + "/" + actual,
						"providers/" + namespace + "/" + name + "/" + floor,
					})
					continue
				}
				reason := "names " + actual
				if actual == "latest" {
					reason = "floats"
				}
				problems = append(problems, fmt.Sprintf(
					"%s:%d: %s link %s, but required_providers pins the floor at %s",
					path, index+1, source, reason, floor,
				))
			}
		}

		if len(replacements) > 0 {
			for _, replacement := range replacements {
				text = strings.ReplaceAll(text, replacement[0], replacement[1])
			}
			if runtime.GOOS == "windows" {
				text = strings.ReplaceAll(text, "\n", "\r\n")
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil { //nolint:gosec // rewrites an existing source file
				fmt.Fprintln(stderr, err)
				return 1
			}
			fixed += len(replacements)
		}
	}

	if fix {
		fmt.Fprintf(stdout, "rewrote %d link(s)\n", fixed)
		return 0
	}

	if len(problems) > 0 {
		fmt.Fprintln(stderr, strings.Join(problems, "\n"))
		fmt.Fprintf(stderr,
			"\n%d `# see` link(s) do not match their provider's constraint floor.\n"+
				"Pin each link to the version in `required_providers`, or run with --fix.\n",
			len(problems),
		)
		return 1
	}

	fmt.Fprintf(stdout, "checked %d file(s): every `# see` link matches its provider's constraint floor\n", len(files))
	return 0
}

// isPrefixOf accepts an unambiguous abbreviation of a long option (`--fi`), as argparse does.
func isPrefixOf(arg, option string) bool {
	return len(arg) > 2 && strings.HasPrefix(option, arg)
}

// terraformFiles lists every `*.tf` file under root, outside `.terraform`, in path-component order.
// A missing or unreadable root yields nothing rather than an error.
func terraformFiles(root string) []string {
	var files []string
	if info, err := os.Stat(root); err != nil || !info.IsDir() { //nolint:gosec // the operator names the directories to scan
		return nil
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error { //nolint:gosec // as above
		if err != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".tf") && !slices.Contains(splitPath(path), ".terraform") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func splitPath(path string) []string {
	return strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' })
}

// requiredProviders maps provider source to constraint floor for one file's declarations.
func requiredProviders(text string) map[string]string {
	found := map[string]string{}
	for _, block := range braceBlocks(text) {
		for _, entry := range entryPattern.FindAllStringSubmatch(block, -1) {
			source := sourcePattern.FindStringSubmatch(entry[1])
			version := versionPattern.FindStringSubmatch(entry[1])
			if source == nil || version == nil {
				continue
			}
			if floor := floorPattern.FindStringSubmatch(version[1]); floor != nil {
				found[strings.ToLower(source[1])] = floor[1]
			}
		}
	}
	return found
}

// braceBlocks returns the brace-balanced body of every `required_providers { ... }` block.
func braceBlocks(text string) []string {
	var blocks []string
	for _, match := range blockPattern.FindAllStringIndex(text, -1) {
		start := match[1] - 1
		depth := 0
		for index := start; index < len(text); index++ {
			switch text[index] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					blocks = append(blocks, text[start+1:index])
				}
			}
			if depth == 0 {
				break
			}
		}
	}
	return blocks
}

// constraintsFor resolves the nearest `required_providers` at or above a file's directory. A repo
// can hold several Terraform roots, each pinning independently.
func constraintsFor(path string, byDir map[string]map[string]string) map[string]string {
	directory := filepath.Dir(path)
	for {
		if declared, ok := byDir[directory]; ok {
			return declared
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
}

// normalizeNewlines reads text the way Python's universal newlines do: `\r\n` and `\r` become `\n`.
func normalizeNewlines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// splitLines matches Python's str.splitlines, so line numbers agree with the original script.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for index, r := range text {
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			lines = append(lines, text[start:index])
			start = index + len(string(r))
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}
