package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("TestCase%04d", i)
	}
	return out
}

func TestParseListKeepsTestsFuzzTargetsAndExamples(t *testing.T) {
	list := "TestB\nBenchmarkX\nFuzzParse\nTestA\nExampleMoney_String\nTestA\nok  \tgo.idp.xyz/idp-brant/internal/bootstrap\t0.031s\n"
	got, err := ParseList(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	if want := "ExampleMoney_String FuzzParse TestA TestB"; strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestParseListRefusesWhatIsNoTest(t *testing.T) {
	for _, list := range []string{
		"TestA\nconnected to the database\n", // something the package printed
		"TestA\nhelperFunc\n",
		"FAIL\tgo.idp.xyz/idp-brant/internal/bootstrap [build failed]\n",
		"ok  \tgo.idp.xyz/idp-brant/internal/kernel\t0.01s\n", // no tests at all
		"BenchmarkOnly\n",
	} {
		if got, err := ParseList(strings.NewReader(list)); err == nil {
			t.Errorf("ParseList(%q) = %v, want an error", list, got)
		}
	}
}

// The assignment is pinned: CI histories and reproductions rely on a test staying in its shard. The
// expected shards were computed independently (Python hashlib.sha256 on the same rule) for N = 1..6.
func TestShardOfIsPinned(t *testing.T) {
	for name, want := range map[string][6]int{
		"TestBRC001FullChainOverHTTP":                      {1, 1, 1, 2, 2, 2},
		"TestCNATSuite01SingleEntityExportInTwoShipments":  {1, 2, 3, 4, 4, 5},
		"TestCNATSuite02SplitShipmentsAndMultiLineExports": {1, 2, 3, 4, 5, 6},
		"TestCNATSuite03InvoiceUnitDiffersFromDeclaration": {1, 2, 2, 3, 4, 4},
	} {
		for n := 1; n <= 6; n++ {
			if got := ShardOf(name, n); got != want[n-1] {
				t.Errorf("ShardOf(%s, %d) = %d, want %d", name, n, got, want[n-1])
			}
		}
	}
}

func TestEveryShardGetsTestsAndNoneIsOutOfRange(t *testing.T) {
	tests := names(600)
	for n := 1; n <= 12; n++ {
		for k, s := range Split(tests, n) {
			// 600 tests in 12 shards: 50 expected per shard, far from 0 or from all.
			if len(s) < 600/n/2 || len(s) > 2*600/n {
				t.Errorf("%d shards: shard %d holds %d of 600 tests", n, k+1, len(s))
			}
		}
	}
}

func TestATestKeepsItsShardWhenOthersAreAddedOrRemoved(t *testing.T) {
	before := names(300)
	after := append(names(500)[100:], "TestNew", "TestAnotherNew")
	for n := 2; n <= 8; n++ {
		where := map[string]int{}
		for k, s := range Split(before, n) {
			for _, name := range s {
				where[name] = k
			}
		}
		for k, s := range Split(after, n) {
			for _, name := range s {
				if old, ok := where[name]; ok && old != k {
					t.Errorf("%d shards: %s moved from shard %d to %d", n, name, old+1, k+1)
				}
			}
		}
	}
}

func TestExpressionIsAnchoredAndNeverEmpty(t *testing.T) {
	re := regexp.MustCompile(Expression([]string{"TestA", "TestB_C"}))
	for name, want := range map[string]bool{"TestA": true, "TestB_C": true, "TestAB": false, "XTestA": false, "TestB": false} {
		if re.MatchString(name) != want {
			t.Errorf("%s: match = %v, want %v", name, !want, want)
		}
	}
	empty := Expression(nil)
	if empty == "" || regexp.MustCompile(empty).MatchString("TestA") {
		t.Errorf("the expression of an empty shard %q must select nothing", empty)
	}
}

func TestCheckPartitionAcceptsTheShards(t *testing.T) {
	tests := names(600)
	for n := 1; n <= 8; n++ {
		sizes, err := CheckPartition(tests, Expressions(tests, n))
		if err != nil {
			t.Fatalf("%d shards: %v", n, err)
		}
		sum := 0
		for _, s := range sizes {
			sum += s
		}
		if len(sizes) != n || sum != 600 {
			t.Fatalf("%d shards: sizes %v", n, sizes)
		}
	}
}

func TestCheckPartitionFindsGapsOverlapsAndStrays(t *testing.T) {
	tests := names(40)
	exprs := Expressions(tests, 3)
	gap := append([]string(nil), exprs...)
	gap[1] = "^$"
	overlap := append([]string(nil), exprs...)
	overlap[2] = "^Test"
	swapped := []string{exprs[1], exprs[0], exprs[2]}
	for what, e := range map[string][]string{"gap": gap, "overlap": overlap, "swapped": swapped} {
		if _, err := CheckPartition(tests, e); err == nil {
			t.Errorf("%s: CheckPartition accepted %v", what, e)
		}
	}
	long := make([]string, 4000)
	for i := range long {
		long[i] = fmt.Sprintf("TestAVeryLongNameThatDescribesTheBehaviourUnderTest%05d", i)
	}
	if _, err := CheckPartition(long, Expressions(long, 1)); err == nil || !strings.Contains(err.Error(), "more shards") {
		t.Errorf("an expression longer than a command-line argument must be refused, got %v", err)
	}
}

const verboseLog = `=== RUN   TestA
--- PASS: TestA (0.01s)
=== RUN   TestB
=== RUN   TestB/sub
    --- PASS: TestB/sub (0.00s)
partial output--- PASS: TestB (1.20s)
=== RUN   TestC
    c_test.go:9: boom
--- FAIL: TestC (0.00s)
=== RUN   TestD
--- SKIP: TestD (0.00s)
FAIL
FAIL	example.com/p	1.3s
`

func TestParseResultsReadsTopLevelOutcomes(t *testing.T) {
	got, err := ParseResults(strings.NewReader(verboseLog))
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{{"TestA", "PASS"}, {"TestB", "PASS"}, {"TestC", "FAIL"}, {"TestD", "SKIP"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func logOf(tests []string, status string) []Result {
	var out []Result
	for _, name := range tests {
		out = append(out, Result{name, status})
	}
	return out
}

func TestVerifyAcceptsACompleteRun(t *testing.T) {
	tests := names(60)
	split := Split(tests, 3)
	var logs [][]Result
	for _, s := range split {
		logs = append(logs, logOf(s, "PASS"))
	}
	summary, err := Verify(tests, 3, 0, logs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "all 60 tests passed") {
		t.Fatalf("summary %q", summary)
	}
	if _, err := Verify(tests, 3, 2, [][]Result{logs[1]}); err != nil {
		t.Fatalf("shard 2 alone: %v", err)
	}
}

func TestVerifyReportsEveryWayARunFallsShort(t *testing.T) {
	tests := names(60)
	split := Split(tests, 3)
	pass := func() [][]Result {
		var logs [][]Result
		for _, s := range split {
			logs = append(logs, logOf(s, "PASS"))
		}
		return logs
	}
	cases := map[string]func(logs [][]Result) [][]Result{
		"did not run": func(l [][]Result) [][]Result { l[0] = l[0][1:]; return l },
		"failed":      func(l [][]Result) [][]Result { l[1][0].Status = "FAIL"; return l },
		"skipped":     func(l [][]Result) [][]Result { l[2][0].Status = "SKIP"; return l },
		"more than once": func(l [][]Result) [][]Result {
			l[0] = append(l[0], l[0][0])
			return l
		},
		"wrong shard": func(l [][]Result) [][]Result { l[1] = append(l[1], l[0][0]); return l },
		"not in the test list": func(l [][]Result) [][]Result {
			l[0] = append(l[0], Result{"TestNotListed", "PASS"})
			return l
		},
	}
	for what, spoil := range cases {
		_, err := Verify(tests, 3, 0, spoil(pass()))
		if err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("%s: Verify = %v", what, err)
		}
	}
	if _, err := Verify(tests, 3, 1, [][]Result{logOf(split[1], "PASS")}); err == nil {
		t.Error("shard 2's log passed as shard 1's must fail")
	}
}

func TestRunRefusesUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"./p"},                                // no shard to print
		{"-shards", "0", "-check", "./p"},      // no shards
		{"-shards", "3", "-shard", "4", "./p"}, // shard out of range
		{"-names", "-check", "./p"},            // two modes
		{"-shards", "2", "-verify", "./p", "one.log"},
		{"-shards", "2", "-shard", "1", "./p", "extra"},
		{"-shards", "2", "-check"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (%s)", args, code, errs.String())
		}
	}
}

// The whole contract through the go command itself: a package's -list output, the -run expression
// of every shard as go test applies it, and the verification of the -v logs. The test names are
// prefixes of each other, so an unanchored or mis-split expression would show.
func TestShardsThroughGoTest(t *testing.T) {
	dir := t.TempDir()
	var src strings.Builder
	src.WriteString("package p\n\nimport \"testing\"\n\n")
	var want []string
	for i := 0; i < 24; i++ {
		name := fmt.Sprintf("TestCase%d", i) // TestCase1 is a prefix of TestCase10 and TestCase11
		want = append(want, name)
		fmt.Fprintf(&src, "func %s(t *testing.T) { t.Run(\"sub/level\", func(t *testing.T) {}) }\n", name)
	}
	src.WriteString("func BenchmarkNotSharded(b *testing.B) {}\n")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/p\n\ngo 1.26\n")
	write("p_test.go", src.String())
	gotest := func(args ...string) string {
		cmd := exec.Command("go", append([]string{"test"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go test %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	tests, err := ParseList(strings.NewReader(gotest("-list", ".", ".")))
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) != len(want) {
		t.Fatalf("listed %v, want %v", tests, want)
	}
	const n = 4
	exprs := Expressions(tests, n)
	if _, err := CheckPartition(tests, exprs); err != nil {
		t.Fatal(err)
	}
	var logs [][]Result
	for k, e := range exprs {
		rs, err := ParseResults(strings.NewReader(gotest("-count=1", "-v", "-run", e, ".")))
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) == 0 && e != "^$" {
			t.Fatalf("shard %d ran nothing", k+1)
		}
		logs = append(logs, rs)
	}
	if _, err := Verify(tests, n, 0, logs); err != nil {
		t.Fatal(err)
	}
}
