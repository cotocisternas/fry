// Command coverage reports statement coverage with explicit generated accounting.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type counts struct {
	Covered uint64 `json:"covered"`
	Total   uint64 `json:"total"`
}

type packageReport struct {
	Package     string `json:"package"`
	Handwritten counts `json:"handwritten"`
	Generated   counts `json:"generated"`
	Unfiltered  counts `json:"unfiltered"`
}

type report struct {
	Mode         string             `json:"mode"`
	Module       string             `json:"module"`
	Dependencies []dependencyReport `json:"dependencies,omitempty"`
	Packages     []packageReport    `json:"packages"`
	Violations   []string           `json:"violations,omitempty"`
}

type block struct {
	statements uint64
	covered    bool
}
type source struct {
	generated bool
	lines     []string
}

var recordPattern = regexp.MustCompile(`^([^\s]+):([0-9]+)\.([0-9]+),([0-9]+)\.([0-9]+) ([0-9]+) ([0-9]+)$`)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(2) // A diagnostic write failure must remain distinguishable.
		}
		os.Exit(1)
	}
}

func run(args []string, out, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("coverage", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	profile := flags.String("profile", "", "Go coverprofile (required unless -print-coverpkg)")
	repo := flags.String("repo", ".", "root of the measured Go module")
	floors := flags.String("floors", "", "optional JSON root-package handwritten percentage floors")
	printScope := flags.Bool("print-coverpkg", false, "print the go test -coverpkg value for the measured modules")
	var mappings []string
	flags.Func(
		"module",
		"local dependency import=repo-relative-directory (repeatable)",
		func(mapping string) error { mappings = append(mappings, mapping); return nil },
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	switch {
	case flags.NArg() != 0:
	case *printScope && *profile == "" && *floors == "":
		return printCoverPackages(out, *repo, mappings)
	case !*printScope && *profile != "":
		return writeReport(out, *profile, *repo, *floors, mappings)
	}
	return errors.New("usage: coverage -profile FILE [-repo DIR] [-floors FILE] [-module IMPORT=DIR ...]\n" +
		"       coverage -print-coverpkg [-repo DIR] [-module IMPORT=DIR ...]")
}

func writeReport(out io.Writer, profile, repo, floors string, mappings []string) error {
	result, err := measure(profile, repo, mappings...)
	if err != nil {
		return err
	}
	if floors != "" {
		manifest, err := readFloors(floors)
		if err != nil {
			return err
		}
		result.Violations = check(result.Packages, manifest)
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if len(result.Violations) != 0 {
		return errors.New("coverage floor check failed (see JSON violations)")
	}
	return nil
}

// printCoverPackages writes the go test -coverpkg value that instruments exactly
// the modules measure accounts for. Import-path patterns select by module
// identity. A relative ./... pattern instead selects every loaded package whose
// directory is below the checkout, including downloaded modules when GOMODCACHE
// is placed there; no mapping can account for those records.
func printCoverPackages(out io.Writer, repo string, mappings []string) (err error) {
	modules, err := openModules(repo, mappings)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeModules(modules)) }()
	patterns := make([]string, len(modules))
	for i, module := range modules {
		patterns[i] = module.name + "/..."
	}
	if _, err := fmt.Fprintln(out, strings.Join(patterns, ",")); err != nil {
		return fmt.Errorf("write coverpkg: %w", err)
	}
	return nil
}

func measure(profile, repo string, mappings ...string) (result report, err error) {
	modules, err := openModules(repo, mappings)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, closeModules(modules)) }()
	result.Module = modules[0].name
	scanner, mode, err := openProfile(profile)
	if err != nil {
		return result, err
	}
	result.Mode = mode
	accounting := profileAccounting{
		mode:    result.Mode,
		modules: modules,
		sources: map[string]source{},
		blocks:  map[string]map[string]block{},
	}
	if err := accounting.scan(scanner); err != nil {
		return result, err
	}
	packages, err := accounting.packages()
	if err != nil {
		return result, err
	}
	return scopeReport(result, modules, packages)
}

func openProfile(profile string) (*bufio.Scanner, string, error) {
	// Developer-selected profile may be outside the repo; read-only CLI input. Owner: FRY maintainers;
	// expires: 2027-01-18.
	//nolint:gosec // Reviewed call; rationale above.
	data, err := os.ReadFile(profile)
	if err != nil {
		return nil, "", err
	}
	// Go writes a newline after every record. Require it to catch partial last records.
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, "", errors.New("empty or truncated coverage profile: missing final newline")
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	if !scanner.Scan() {
		return nil, "", errors.New("missing coverage mode")
	}
	header := scanner.Text()
	switch header {
	case "mode: set", "mode: count", "mode: atomic":
		return scanner, strings.TrimPrefix(header, "mode: "), nil
	default:
		return nil, "", fmt.Errorf("invalid coverage header %q", header)
	}
}

type profileAccounting struct {
	mode    string
	modules []localModule
	sources map[string]source
	blocks  map[string]map[string]block
}

func (a *profileAccounting) scan(scanner *bufio.Scanner) error {
	lineNo := 1
	for scanner.Scan() {
		lineNo++
		if err := a.record(scanner.Text(), lineNo); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	if len(a.blocks) == 0 {
		return errors.New("coverage profile contains no measured blocks")
	}
	return nil
}

func (a *profileAccounting) record(line string, lineNo int) error {
	match := recordPattern.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("invalid coverage record at line %d", lineNo)
	}
	filename := match[1]
	src, err := a.source(filename)
	if err != nil {
		return err
	}
	nums, err := parseRecordNumbers(match, lineNo)
	if err != nil {
		return err
	}
	sl, sc, el, ec := nums[0], nums[1], nums[2], nums[3]
	if !validSourceRange(src, nums, a.mode) {
		return fmt.Errorf("line %d: invalid source range or count", lineNo)
	}
	key := fmt.Sprintf("%d.%d,%d.%d", sl, sc, el, ec)
	if a.blocks[filename] == nil {
		a.blocks[filename] = map[string]block{}
	}
	previous, exists := a.blocks[filename][key]
	if exists && previous.statements != nums[4] {
		return fmt.Errorf("line %d: inconsistent duplicate block", lineNo)
	}
	a.blocks[filename][key] = block{nums[4], previous.covered || nums[5] > 0}
	return nil
}

func (a *profileAccounting) source(filename string) (source, error) {
	scope := moduleForFile(a.modules, filename)
	if scope < 0 {
		return source{}, fmt.Errorf("profile path outside module: %q", filename)
	}
	relative := strings.TrimPrefix(filename, a.modules[scope].name+"/")
	if !canonicalRelative(relative) || !strings.HasSuffix(relative, ".go") {
		return source{}, fmt.Errorf("noncanonical source path: %q", filename)
	}
	src, ok := a.sources[filename]
	if !ok {
		var err error
		src, err = loadSource(a.modules[scope].root, relative)
		if err != nil {
			return source{}, fmt.Errorf("source %s: %w", filename, err)
		}
		a.sources[filename] = src
	}
	return src, nil
}

func (a *profileAccounting) packages() (map[string]*packageReport, error) {
	packages := map[string]*packageReport{}
	for filename, entries := range a.blocks {
		name := path.Dir(filename)
		p := packages[name]
		if p == nil {
			p = &packageReport{Package: name}
			packages[name] = p
		}
		for _, entry := range entries {
			category := &p.Handwritten
			if a.sources[filename].generated {
				category = &p.Generated
			}
			if err := add(category, entry); err != nil {
				return nil, err
			}
			if err := add(&p.Unfiltered, entry); err != nil {
				return nil, err
			}
		}
	}
	return packages, nil
}

func scopeReport(result report, modules []localModule, packages map[string]*packageReport) (report, error) {
	scoped := make([][]packageReport, len(modules))
	for _, p := range packages {
		// Package import paths do not contain the filename separator needed by the
		// resolver; append it to also resolve packages at a module's root.
		scope := moduleForFile(modules, p.Package+"/")
		scoped[scope] = append(scoped[scope], *p)
	}
	for i := range scoped {
		sort.Slice(scoped[i], func(a, b int) bool { return scoped[i][a].Package < scoped[i][b].Package })
		if i == 0 {
			result.Packages = scoped[i]
			continue
		}
		if scoped[i] == nil {
			scoped[i] = []packageReport{}
		}
		result.Dependencies = append(
			result.Dependencies,
			dependencyReport{
				Module:    modules[i].name,
				Directory: modules[i].directory,
				Packages:  scoped[i],
			},
		)
	}
	if len(result.Packages) == 0 {
		return result, errors.New("coverage profile contains no measured root-module packages")
	}
	sort.Slice(
		result.Dependencies,
		func(i, j int) bool { return result.Dependencies[i].Module < result.Dependencies[j].Module },
	)
	return result, nil
}

func loadSource(root *os.Root, relative string) (source, error) {
	filename := filepath.FromSlash(relative)
	if err := rejectSymlinks(root, filename); err != nil {
		return source{}, err
	}
	// Nested modules belong in their own reports, including local replacements.
	for dir := filepath.Dir(filename); dir != "."; dir = filepath.Dir(dir) {
		if _, err := root.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return source{}, errors.New("source belongs to a nested module")
		} else if !errors.Is(err, os.ErrNotExist) {
			return source{}, err
		}
	}
	data, err := root.ReadFile(filename)
	if err != nil {
		return source{}, err
	}
	file, err := parser.ParseFile(token.NewFileSet(), filename, data, parser.ParseComments)
	if err != nil {
		return source{}, err
	}
	return source{
		generated: ast.IsGenerated(file),
		lines:     strings.Split(string(data), "\n"),
	}, nil
}

func add(c *counts, b block) error {
	if math.MaxUint64-c.Total < b.statements {
		return errors.New("statement total overflow")
	}
	c.Total += b.statements
	if b.covered {
		c.Covered += b.statements
	}
	return nil
}

func readFloors(filename string) (map[string]float64, error) {
	// Developer-selected floor manifest may be outside the repo; read-only CLI input. Owner: FRY
	// maintainers; expires: 2027-01-18.
	//nolint:gosec // Reviewed call; rationale above.
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Packages map[string]float64 `json:"packages"`
	}
	// Reject duplicate JSON keys rather than letting a later value lower a floor.
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := uniqueJSON(decoder); err != nil {
		return nil, fmt.Errorf("invalid floor manifest: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid trailing floor manifest data")
	}
	decoder = json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("invalid floor manifest: %w", err)
	}
	if len(manifest.Packages) == 0 {
		return nil, errors.New("floor manifest must track at least one package")
	}
	for name, floor := range manifest.Packages {
		if name == "" || floor < 0 || floor > 100 {
			return nil, fmt.Errorf("invalid floor for %q (expected percentage 0..100)", name)
		}
	}
	return manifest.Packages, nil
}

func uniqueJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		if t == nil {
			return errors.New("null values are not allowed")
		}
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			if err := uniqueJSONKey(d, seen); err != nil {
				return err
			}
		}
		if err := uniqueJSON(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func check(packages []packageReport, floors map[string]float64) []string {
	var violations []string
	measured := map[string]bool{}
	for _, p := range packages {
		measured[p.Package] = true
		floor, ok := floors[p.Package]
		if !ok {
			violations = append(violations, "untracked measured package: "+p.Package)
			continue
		}
		c := p.Handwritten
		if (c.Total == 0 && floor > 0) || float64(c.Covered)*100 < floor*float64(c.Total) {
			violations = append(
				violations,
				fmt.Sprintf("package %s: handwritten %d/%d below floor %g%%", p.Package, c.Covered, c.Total, floor),
			)
		}
	}
	for name := range floors {
		if !measured[name] {
			violations = append(violations, "tracked package missing from profile: "+name)
		}
	}
	sort.Strings(violations)
	return violations
}

func parseRecordNumbers(match []string, lineNo int) ([6]uint64, error) {
	var nums [6]uint64
	for i := range nums {
		n, err := strconv.ParseUint(match[i+2], 10, 64)
		if err != nil {
			return nums, fmt.Errorf("line %d: invalid integer: %w", lineNo, err)
		}
		nums[i] = n
	}
	return nums, nil
}

// Equal endpoints are legal only for Go's empty zero-statement functions.
func validSourceRange(src source, nums [6]uint64, mode string) bool {
	sl, sc, el, ec := nums[0], nums[1], nums[2], nums[3]
	if !validSourcePosition(src, sl, sc) || !validSourcePosition(src, el, ec) {
		return false
	}
	if sl > el || (sl == el && (sc > ec || (sc == ec && nums[4] > 0))) {
		return false
	}
	return mode != "set" || nums[5] <= 1
}

func validSourcePosition(src source, line, column uint64) bool {
	return line > 0 && line <= uint64(len(src.lines)) && column > 0 && column <= uint64(len(src.lines[line-1])+1)
}

func uniqueJSONKey(d *json.Decoder, seen map[string]bool) error {
	key, err := d.Token()
	if err != nil {
		return err
	}
	name, ok := key.(string)
	if !ok || seen[name] {
		return fmt.Errorf("duplicate or invalid key %q", key)
	}
	seen[name] = true
	return nil
}

func openRepositoryRoot(repo string) (*os.Root, error) {
	root, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	repoRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return repoRoot, nil
}

func duplicateModule(modules []localModule, mapped localModule) bool {
	for _, existing := range modules {
		if existing.name == mapped.name || existing.directory == mapped.directory {
			return true
		}
	}
	return false
}
