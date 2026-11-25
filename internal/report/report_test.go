package report

import (
	"github.com/daphnezlin/flakehunt/internal/analysis"
	"github.com/daphnezlin/flakehunt/internal/runner"
	"github.com/daphnezlin/flakehunt/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zooRun builds results shaped like the flaky zoo: an order-dependent test
// with a known polluter, a network test, and a stable test.
func zooRun() (store.RunInfo, *analysis.Aggregate, []analysis.Cause) {
	agg := analysis.NewAggregate()
	for _, cpu := range []float64{0, 0.5} {
		for _, lat := range []int{0, 150} {
			for i := 0; i < 10; i++ {
				seed := int64(100*int(cpu*10) + lat + i)
				agg.Add(runner.TrialResult{Trial: runner.Trial{Seed: seed, CPUs: cpu, LatencyMS: lat}, Tests: []runner.TestOutcome{
					{Name: "m::test_create", NodeID: "t.py::test_create"},
					{Name: "m::test_empty", NodeID: "t.py::test_empty", Failed: i%2 == 0},
					{Name: "m::test_api", NodeID: "t.py::test_api", Failed: lat == 150 && i < 8},
					{Name: "m::test_ok", NodeID: "t.py::test_ok"},
				}})
			}
		}
	}
	replays := map[string]*analysis.Count{"m::test_empty": {Runs: 5, Failures: 5}, "m::test_api": {Runs: 5}}
	polluters := map[string]analysis.PolluterResult{"m::test_empty": {
		Polluter: "m::test_create", PolluterNode: "t.py::test_create", VictimNode: "t.py::test_empty", Probes: 3,
	}}
	run := store.RunInfo{ID: 8, StartedAt: time.Now(), Image: "zoo", Workers: 5, BaseSeed: 42,
		CPULevels: []float64{0, 0.5}, LatencyLevels: []int{0, 150}, Service: "api", Env: []string{"A=1"}}
	return run, agg, analysis.Analyze(agg, replays, polluters)
}

func TestWriteMarkdown(t *testing.T) {
	run, agg, causes := zooRun()
	var b strings.Builder
	if err := WriteMarkdown(&b, run, agg, causes); err != nil {
		t.Fatal(err)
	}
	md := b.String()

	for _, want := range []string{
		"# flakehunt report: run #8",
		"2 of 4 tests are flaky",
		"| `m::test_empty` |",
		"order-dependent",
		"Fails when it runs after m::test_create",
		// minimal repro: just the polluter and the victim
		"--randomly-dont-reorganize t.py::test_create t.py::test_empty",
		"**Likely cause: network.**",
		"| 150+/-75ms | 16/20 (80%) |",
		"### Suggested fix",
		"Reset the shared state",
		"<summary>2 tests that never or always failed</summary>",
		"-e A=1",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	// every table row needs the same number of columns or github won't render it
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| `") && strings.Count(line, "|") != 5 {
			t.Errorf("summary row has wrong column count: %s", line)
		}
	}
}

func TestWriteMarkdownNoFlaky(t *testing.T) {
	agg := analysis.NewAggregate()
	agg.Add(runner.TrialResult{Trial: runner.Trial{}, Tests: []runner.TestOutcome{{Name: "m::ok"}}})
	var b strings.Builder
	if err := WriteMarkdown(&b, store.RunInfo{ID: 1, Image: "x"}, agg, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "No flaky tests found") {
		t.Errorf("got %s", b.String())
	}
}

func TestWriteMarkdownFile(t *testing.T) {
	run, agg, causes := zooRun()
	dir := t.TempDir()
	path, err := WriteMarkdownFile(filepath.Join(dir, "reports", "run-{id}.md"), run, agg, causes)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "run-8.md" {
		t.Errorf("path: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}
}

func TestReproForLatency(t *testing.T) {
	s := &analysis.TestStats{Failing: []runner.Trial{{Seed: 5, CPUs: 0.5, LatencyMS: 150}}}
	network := &analysis.Cause{NetTested: true, NetQ: 0.001}
	r := reproFor(s, network, "zoo", nil)
	if !strings.Contains(r.Cmd, "--cpus 0.5") || !strings.Contains(r.Note, "--latency-levels 150") || r.Minimal {
		t.Errorf("got %+v", r)
	}
	// a timing test that happened to fail with latency shouldn't point at the network
	timing := &analysis.Cause{NetTested: true, NetQ: 0.8, CPUTested: true, CPUq: 0.001}
	if r := reproFor(s, timing, "zoo", nil); r.Note != "" {
		t.Errorf("latency note on a non-network test: %q", r.Note)
	}
}

func TestFixForEveryLabel(t *testing.T) {
	for _, l := range []string{"timing", "network", "timing + network", "order-dependent", "random data", "seed + timing"} {
		if fixFor(l) == fixFor("unknown") {
			t.Errorf("no specific fix for %q", l)
		}
	}
}
