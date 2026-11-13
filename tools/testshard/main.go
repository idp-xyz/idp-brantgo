// testshard splits the top-level tests of one Go package into shards that run as separate go test
// processes: parallel CI jobs, or parallel local runs against databases of their own (make
// integration SHARDS=N, scripts/test-shards.sh). It uses the standard library only.
//
// A test's shard depends on its name and the shard count alone: of N shards, a test belongs to shard
// 1 + ⌊h·N / 2⁶⁴⌋, where h is the first 8 bytes (big-endian) of the SHA-256 of its name. A new test
// is placed without editing anything, a test keeps its shard when others are added, renamed or
// removed, and only a change of N moves tests. There is no durations file to keep current. The price
// is the luck of the draw: on internal/bootstrap as CI run 43357384395 measured it, the slowest of
// four or five shards is expected to run 13–17 % longer than their mean, which one more shard buys
// back. (FNV-1a would be cheaper, but its high bits barely depend on the last bytes of a name, and
// test names differ mostly at the end: TestCNAT01, TestCNAT02, ...)
//
//	testshard -names [-shards N -shard k] [-tags t] [-race] pkg
//	                                                       the tests go test -list finds (of shard k),
//	                                                       one per line
//	testshard -shards N -shard k [-tags t] [-race] pkg     shard k's -run expression
//	testshard -shards N -check [-tags t] [-race] pkg       prove the shards partition the package
//	testshard -shards N -verify [-shard k] [...] pkg log...
//	                                                       prove a run from its go test -v output: every
//	                                                       test of the shard(s) passed exactly once, in
//	                                                       its own shard
//
// -from file takes the tests from a file (the -names output, or go test -list output) instead of
// running go test -list. Tests, fuzz targets and examples are sharded; benchmarks are not, since -run
// never runs them. The exit status is 1 when a check or verification fails, 2 on a usage error.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// maxExpression bounds a -run expression: Linux limits one command-line argument to 131072 bytes
// (MAX_ARG_STRLEN), and go test hands the expression to the test binary as -test.run=<expression>.
const maxExpression = 120_000

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("testshard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	shards := fs.Int("shards", 1, "number of shards N")
	shard := fs.Int("shard", 0, "shard k, 1..N (with -verify: 0 means every shard, one log per shard in order)")
	names := fs.Bool("names", false, "print the package's tests (with -shard: shard k's), one per line")
	check := fs.Bool("check", false, "prove the shards' -run expressions partition the package")
	verifyLogs := fs.Bool("verify", false, "prove a run from the go test -v logs that follow the package")
	tags := fs.String("tags", "", "build tags for go test -list")
	race := fs.Bool("race", false, "list with -race, sharing the build of a race run")
	from := fs.String("from", "", "read the tests from this file instead of running go test -list")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "testshard: "+format+"\n", a...)
		return 2
	}
	modes := 0
	for _, m := range []bool{*names, *check, *verifyLogs} {
		if m {
			modes++
		}
	}
	switch {
	case modes > 1:
		return usage("-names, -check and -verify exclude each other")
	case fs.NArg() < 1:
		return usage("name the package, e.g. ./internal/bootstrap")
	case fs.NArg() > 1 && !*verifyLogs:
		return usage("only -verify takes logs after the package")
	case *shards < 1:
		return usage("-shards must be at least 1, got %d", *shards)
	case *shard < 0 || *shard > *shards:
		return usage("-shard must be in 1..%d, got %d", *shards, *shard)
	case modes == 0 && *shard == 0:
		return usage("-shard k selects the expression to print")
	case *verifyLogs && *shard == 0 && fs.NArg()-1 != *shards:
		return usage("-verify without -shard takes one log per shard (%d), got %d", *shards, fs.NArg()-1)
	case *verifyLogs && fs.NArg() < 2:
		return usage("-verify takes the go test -v log(s) after the package")
	}
	pkg := fs.Arg(0)
	tests, err := load(*from, pkg, *tags, *race)
	if err != nil {
		fmt.Fprintln(stderr, "testshard:", err)
		return 1
	}
	if *names {
		if *shard > 0 {
			tests = Split(tests, *shards)[*shard-1]
		}
		for _, t := range tests {
			fmt.Fprintln(stdout, t)
		}
		return 0
	}
	exprs := Expressions(tests, *shards)
	sizes, err := CheckPartition(tests, exprs)
	if err != nil {
		fmt.Fprintf(stderr, "testshard: %s: %v\n", pkg, err)
		return 1
	}
	switch {
	case *check:
		fmt.Fprintf(stdout, "testshard: %s: %d tests in %d shards (%s); each test matches exactly one shard's -run expression, its own\n",
			pkg, len(tests), *shards, joinInts(sizes))
	case *verifyLogs:
		var logs [][]Result
		for _, path := range fs.Args()[1:] {
			rs, err := readResults(path)
			if err != nil {
				fmt.Fprintln(stderr, "testshard:", err)
				return 1
			}
			logs = append(logs, rs)
		}
		summary, err := Verify(tests, *shards, *shard, logs)
		if err != nil {
			fmt.Fprintf(stderr, "testshard: %s: verify failed:\n%v\n", pkg, err)
			return 1
		}
		fmt.Fprintf(stdout, "testshard: %s: %s\n", pkg, summary)
	default:
		fmt.Fprintln(stdout, exprs[*shard-1])
	}
	return 0
}

func load(from, pkg, tags string, race bool) ([]string, error) {
	if from != "" {
		f, err := os.Open(from)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return ParseList(f)
	}
	args := []string{"test", "-list", "."}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	if race {
		args = append(args, "-race")
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return ParseList(strings.NewReader(string(out)))
}

var (
	identifier = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*$`)
	summary    = regexp.MustCompile(`^(ok|\?)\s`)
)

// ParseList reads go test -list output (or one name per line) and returns the tests, fuzz targets
// and examples, sorted. Benchmarks are left out. Any other line is an error, so nothing a package
// prints is silently taken for a test or dropped.
func ParseList(r io.Reader) ([]string, error) {
	seen := map[string]bool{}
	var tests []string
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || summary.MatchString(line):
		case !identifier.MatchString(line):
			return nil, fmt.Errorf("line %d of the test list is not a test name: %q", n, line)
		case strings.HasPrefix(line, "Benchmark"):
		case strings.HasPrefix(line, "Test"), strings.HasPrefix(line, "Fuzz"), strings.HasPrefix(line, "Example"):
			if !seen[line] {
				seen[line] = true
				tests = append(tests, line)
			}
		default:
			return nil, fmt.Errorf("line %d of the test list is no test, fuzz target, example or benchmark: %q", n, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(tests) == 0 {
		return nil, errors.New("the package lists no tests")
	}
	sort.Strings(tests)
	return tests, nil
}

// ShardOf returns the shard, 1..n, of the test name.
func ShardOf(name string, n int) int {
	sum := sha256.Sum256([]byte(name))
	hi, _ := bits.Mul64(binary.BigEndian.Uint64(sum[:8]), uint64(n))
	return int(hi) + 1
}

// Split returns the tests of each shard, in the order given.
func Split(tests []string, n int) [][]string {
	out := make([][]string, n)
	for _, t := range tests {
		k := ShardOf(t, n) - 1
		out[k] = append(out[k], t)
	}
	return out
}

// Expression returns the -run expression that selects exactly the given top-level tests: anchored,
// so TestA does not select TestAB, and never empty, since an empty -run selects everything.
func Expression(tests []string) string {
	if len(tests) == 0 {
		return "^$"
	}
	quoted := make([]string, len(tests))
	for i, t := range tests {
		quoted[i] = regexp.QuoteMeta(t)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// Expressions returns the -run expression of every shard, shard 1 first.
func Expressions(tests []string, n int) []string {
	var out []string
	for _, s := range Split(tests, n) {
		out = append(out, Expression(s))
	}
	return out
}

// CheckPartition proves that the expressions partition the tests: matched the way go test -run
// matches top-level names, every test is selected by exactly one expression, the one of its own
// shard, and every expression fits on a command line. It returns the number of tests per shard.
func CheckPartition(tests []string, exprs []string) ([]int, error) {
	res := make([]*regexp.Regexp, len(exprs))
	var problems []string
	for i, e := range exprs {
		if len(e) > maxExpression {
			problems = append(problems, fmt.Sprintf("shard %d's -run expression has %d bytes, more than %d: use more shards", i+1, len(e), maxExpression))
		}
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("shard %d: %w", i+1, err)
		}
		res[i] = re
	}
	sizes := make([]int, len(exprs))
	for _, t := range tests {
		var in []int
		for i, re := range res {
			if re.MatchString(t) {
				in = append(in, i+1)
			}
		}
		switch {
		case len(in) == 0:
			problems = append(problems, t+" is in no shard")
		case len(in) > 1:
			problems = append(problems, fmt.Sprintf("%s is in shards %s", t, joinInts(in)))
		case in[0] != ShardOf(t, len(exprs)):
			problems = append(problems, fmt.Sprintf("%s is in shard %d, not in its own shard %d", t, in[0], ShardOf(t, len(exprs))))
		default:
			sizes[in[0]-1]++
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return sizes, nil
}

// Result is one top-level test outcome in go test -v output.
type Result struct {
	Name, Status string // Status is PASS, FAIL or SKIP
}

// result matches "--- PASS: TestX (0.01s)" anywhere in a line, since output a test writes without a
// trailing newline lands in front of it. Subtests are told apart by the slash in their names.
var result = regexp.MustCompile(`--- (PASS|FAIL|SKIP): (\S+) \(\d+(?:\.\d+)?s\)`)

// ParseResults returns the top-level test outcomes in go test -v output, in order.
func ParseResults(r io.Reader) ([]Result, error) {
	var out []Result
	rd := bufio.NewReader(r)
	for {
		line, err := rd.ReadString('\n')
		for _, m := range result.FindAllStringSubmatch(line, -1) {
			if !strings.Contains(m[2], "/") {
				out = append(out, Result{Name: m[2], Status: m[1]})
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func readResults(path string) ([]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rs, err := ParseResults(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rs, nil
}

// Verify proves a sharded run from its go test -v results: logs[i] is shard i+1's output when shard
// is 0, or every log is shard's output. Every test of the shards run must have passed exactly once,
// in its own shard; a failure, a skip (it proves nothing), a missing test, a test run twice or in
// another shard, and a test the list does not know are all reported. It returns a one-line summary.
func Verify(tests []string, n, shard int, logs [][]Result) (string, error) {
	known := map[string]int{}
	for _, t := range tests {
		known[t] = ShardOf(t, n)
	}
	passed := map[string]int{}
	var failed, skipped, misplaced, unknown, twice, missing []string
	for i, rs := range logs {
		s := shard
		if s == 0 {
			s = i + 1
		}
		for _, r := range rs {
			own, ok := known[r.Name]
			switch {
			case !ok:
				unknown = append(unknown, fmt.Sprintf("%s (shard %d)", r.Name, s))
				continue
			case own != s:
				misplaced = append(misplaced, fmt.Sprintf("%s ran in shard %d, belongs to shard %d", r.Name, s, own))
				continue
			}
			switch r.Status {
			case "PASS":
				passed[r.Name]++
			case "FAIL":
				failed = append(failed, r.Name)
			case "SKIP":
				skipped = append(skipped, r.Name)
			}
		}
	}
	per := make([]int, n)
	want := 0
	for _, t := range tests {
		s := known[t]
		if shard != 0 && s != shard {
			continue
		}
		want++
		switch c := passed[t]; {
		case c == 1:
			per[s-1]++
		case c > 1:
			twice = append(twice, fmt.Sprintf("%s passed %d times", t, c))
		case !contains(failed, t) && !contains(skipped, t):
			missing = append(missing, fmt.Sprintf("%s (shard %d)", t, s))
		}
	}
	var b strings.Builder
	for _, p := range []struct {
		what  string
		names []string
	}{
		{"failed", failed}, {"skipped (a skip proves nothing)", skipped}, {"did not run", missing},
		{"ran more than once", twice}, {"ran in the wrong shard", misplaced}, {"ran but are not in the test list", unknown},
	} {
		if len(p.names) > 0 {
			fmt.Fprintf(&b, "  %d %s:\n", len(p.names), p.what)
			for _, name := range p.names {
				fmt.Fprintf(&b, "    %s\n", name)
			}
		}
	}
	if b.Len() > 0 {
		return "", errors.New(strings.TrimRight(b.String(), "\n"))
	}
	if shard != 0 {
		return fmt.Sprintf("shard %d of %d: all %d tests passed, each exactly once", shard, n, want), nil
	}
	return fmt.Sprintf("%d shards: all %d tests passed, each exactly once, in its own shard (%s)", n, want, joinInts(per)), nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprint(x)
	}
	return strings.Join(s, " + ")
}
