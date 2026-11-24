package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
)

// repro is how to reproduce one flaky test.
type repro struct {
	Cmd     string
	Note    string // extra instructions, "" if the command is enough
	Minimal bool   // only the tests needed, from the polluter search
}

func reproFor(s *TestStats, c *Cause, image string, env []string) repro {
	var envFlags strings.Builder
	for _, e := range env {
		envFlags.WriteString(" -e " + e)
	}
	t := s.Failing[0]
	base := fmt.Sprintf("docker run --rm%%s%s %s pytest -q -p no:cacheprovider --randomly-seed=%d",
		envFlags.String(), image, t.Seed)

	if c != nil && c.Polluter != nil {
		p := c.Polluter
		tests := ""
		switch {
		case p.FailsAlone:
			tests = p.VictimNode
		case p.PolluterNode != "":
			tests = p.PolluterNode + " " + p.VictimNode
		}
		if tests != "" {
			return repro{Cmd: fmt.Sprintf(base, "") + " --randomly-dont-reorganize " + tests, Minimal: true}
		}
	}

	cpuFlag := ""
	if t.CPUs > 0 {
		cpuFlag = " --cpus " + formatLevel(t.CPUs)
	}
	r := repro{Cmd: fmt.Sprintf(base, cpuFlag)}
	if t.LatencyMS > 0 && c != nil && c.NetTested && c.NetQ < alpha {
		r.Note = fmt.Sprintf("failed with %dms latency to the service, which docker run alone won't add: "+
			"rerun flakehunt with --latency-levels %d to reproduce", t.LatencyMS, t.LatencyMS)
	}
	return r
}

// fixFor is a starting point for fixing each kind of cause.
func fixFor(label string) string {
	switch label {
	case "timing":
		return "Wait for the work to actually finish (join the thread, wait on an event, poll with a deadline) " +
			"instead of sleeping a fixed time, or loosen the timeout if it's tighter than slow machines can manage."
	case "network":
		return "Raise the timeout, retry with backoff, or replace the real service with a mock in tests that " +
			"don't need to check network behaviour."
	case "timing + network":
		return "Find the timeout the test depends on and raise it, or wait on a condition instead of a fixed time. " +
			"It's sensitive to both a slow cpu and a slow service."
	case "order-dependent":
		return "Reset the shared state between tests, e.g. a fixture that clears it, or stop using module-level " +
			"globals. Running the two tests in the repro command makes it fail every time."
	case "random data":
		return "Run the repro to see which value breaks it, then fix the code for that value and add a test for " +
			"that edge case on purpose instead of relying on random data to hit it."
	case "seed + timing":
		return "Start with the seed-dependent part (rerun with the seed to see what breaks), then check for a " +
			"timeout or race that slow conditions make worse."
	default:
		return "Run more trials to narrow it down."
	}
}

func printReport(agg *Aggregate, causes []Cause, image string, env []string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TEST\tFAILED\tRATE\t95% CI\tVERDICT")
	var flaky []*TestStats
	for _, s := range agg.Sorted() {
		low, high := Wilson(s.Failures, s.Runs)
		verdict := "stable"
		switch {
		case s.Flaky():
			verdict = "FLAKY"
			flaky = append(flaky, s)
		case s.Failures == s.Runs && s.Runs > 0:
			verdict = "always fails"
		}
		fmt.Fprintf(w, "%s\t%d/%d\t%.1f%%\t%.1f-%.1f%%\t%s\n",
			s.Name, s.Failures, s.Runs, 100*s.Rate(), 100*low, 100*high, verdict)
	}
	w.Flush()

	if agg.InfraFails > 0 || agg.Timeouts > 0 {
		fmt.Printf("\n%d trials had infrastructure errors, %d timed out (excluded from rates)\n",
			agg.InfraFails, agg.Timeouts)
	}
	if len(flaky) == 0 {
		fmt.Println("\nNo flaky tests found.")
		return
	}

	fmt.Println("\nFailure rate by CPU limit:")
	for _, s := range flaky {
		fmt.Printf("  %s\n   ", s.Name)
		for _, l := range s.CPULevels() {
			fmt.Printf("  %s: %s", formatLevel(l), formatCount(*s.ByCPU[l]))
		}
		fmt.Println()
	}
	if showLatency(flaky) {
		fmt.Println("\nFailure rate by network latency:")
		for _, s := range flaky {
			fmt.Printf("  %s\n   ", s.Name)
			for _, l := range s.LatencyLevels() {
				fmt.Printf("  %s: %s", formatLatency(int(l)), formatCount(*s.ByLatency[l]))
			}
			fmt.Println()
		}
	}

	fmt.Println("\nLikely causes:")
	for _, c := range causes {
		fmt.Printf("  %s\n    %s: %s\n", c.Test, c.Label, c.Detail)
		if ev := evidence(c); len(ev) > 0 {
			fmt.Printf("    %s\n", strings.Join(ev, ", "))
		}
	}

	byName := causesByName(causes)
	fmt.Println("\nReproduce (a trial where each flaky test failed):")
	for _, s := range flaky {
		r := reproFor(s, byName[s.Name], image, env)
		fmt.Printf("  %s\n    %s\n", s.Name, r.Cmd)
		if r.Note != "" {
			fmt.Printf("    (%s)\n", r.Note)
		}
	}
	fmt.Println("\nNote: a seed reproduces order and random data, not timing. Timing races may need several tries.")
}

// WriteMarkdown writes a report meant to be read on GitHub. Each flaky test
// gets its own section that can be pasted into an issue as is.
func WriteMarkdown(w io.Writer, run RunInfo, agg *Aggregate, causes []Cause) error {
	var b strings.Builder
	sorted := agg.Sorted()
	var flaky []*TestStats
	for _, s := range sorted {
		if s.Flaky() {
			flaky = append(flaky, s)
		}
	}
	byName := causesByName(causes)

	fmt.Fprintf(&b, "# flakehunt report: run #%d\n\n", run.ID)
	fmt.Fprintf(&b, "- **Image:** `%s`\n", run.Image)
	fmt.Fprintf(&b, "- **Started:** %s\n", run.StartedAt.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "- **Trials:** %d (%d at a time), base seed `%d`\n", agg.Trials, run.Workers, run.BaseSeed)
	fmt.Fprintf(&b, "- **CPU limits:** %s\n", formatLevels(run.CPULevels))
	if run.Service != "" {
		fmt.Fprintf(&b, "- **Network latency:** %s (service `%s`)\n", formatLatencies(run.LatencyLevels), run.Service)
	}
	if agg.InfraFails > 0 || agg.Timeouts > 0 {
		fmt.Fprintf(&b, "- **Excluded:** %d trials with infrastructure errors, %d timed out\n", agg.InfraFails, agg.Timeouts)
	}
	fmt.Fprintf(&b, "- **Result:** %d of %d tests are flaky\n\n", len(flaky), len(sorted))

	if len(flaky) == 0 {
		b.WriteString("No flaky tests found.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	b.WriteString("## Summary\n\n")
	b.WriteString("| Test | Failed | 95% CI | Likely cause |\n|---|---|---|---|\n")
	for _, s := range flaky {
		low, high := Wilson(s.Failures, s.Runs)
		label := "unknown"
		if c := byName[s.Name]; c != nil {
			label = c.Label
		}
		fmt.Fprintf(&b, "| `%s` | %d/%d (%.1f%%) | %.1f-%.1f%% | %s |\n",
			s.Name, s.Failures, s.Runs, 100*s.Rate(), 100*low, 100*high, label)
	}
	b.WriteString("\n")

	for _, s := range flaky {
		c := byName[s.Name]
		low, high := Wilson(s.Failures, s.Runs)
		fmt.Fprintf(&b, "## `%s`\n\n", s.Name)
		if c != nil {
			fmt.Fprintf(&b, "**Likely cause: %s.** %s.\n\n", c.Label, capitalize(c.Detail))
		}
		fmt.Fprintf(&b, "Failed %d of %d runs (%.1f%%, 95%% CI %.1f-%.1f%%) on the same code.\n\n",
			s.Failures, s.Runs, 100*s.Rate(), 100*low, 100*high)

		r := reproFor(s, c, run.Image, run.Env)
		b.WriteString("### Reproduce\n\n")
		if r.Minimal {
			b.WriteString("Only the tests needed to trigger it:\n\n")
		}
		fmt.Fprintf(&b, "```sh\n%s\n```\n\n", r.Cmd)
		if r.Note != "" {
			fmt.Fprintf(&b, "%s.\n\n", capitalize(r.Note))
		}

		b.WriteString("### Evidence\n\n")
		b.WriteString("| CPU limit | Failed |\n|---|---|\n")
		for _, l := range s.CPULevels() {
			fmt.Fprintf(&b, "| %s | %s |\n", formatLevel(l), formatCount(*s.ByCPU[l]))
		}
		b.WriteString("\n")
		if len(s.ByLatency) > 1 {
			b.WriteString("| Network latency | Failed |\n|---|---|\n")
			for _, l := range s.LatencyLevels() {
				fmt.Fprintf(&b, "| %s | %s |\n", formatLatency(int(l)), formatCount(*s.ByLatency[l]))
			}
			b.WriteString("\n")
		}
		if c != nil {
			for _, e := range evidence(*c) {
				fmt.Fprintf(&b, "- %s\n", capitalize(e))
			}
			b.WriteString("\n")
			fmt.Fprintf(&b, "### Suggested fix\n\n%s\n\n", fixFor(c.Label))
		}
	}

	if stable := len(sorted) - len(flaky); stable > 0 {
		fmt.Fprintf(&b, "<details>\n<summary>%d tests that never or always failed</summary>\n\n", stable)
		for _, s := range sorted {
			if !s.Flaky() {
				fmt.Fprintf(&b, "- `%s`: %d/%d failed\n", s.Name, s.Failures, s.Runs)
			}
		}
		b.WriteString("\n</details>\n\n")
	}
	b.WriteString("---\n\nGenerated by [flakehunt](https://github.com/daphnezlin/flakehunt). " +
		"p-values are from a one-sided permutation trend test, adjusted with Benjamini-Hochberg.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// writeMarkdownFile fills in {id} in the path and writes the report there.
func writeMarkdownFile(path string, run RunInfo, agg *Aggregate, causes []Cause) (string, error) {
	path = strings.ReplaceAll(path, "{id}", strconv.FormatInt(run.ID, 10))
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if err := WriteMarkdown(f, run, agg, causes); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

func evidence(c Cause) []string {
	var ev []string
	if c.CPUTested {
		ev = append(ev, fmt.Sprintf("CPU trend p=%s (adjusted %s)", formatP(c.CPUp), formatP(c.CPUq)))
	}
	if c.NetTested {
		ev = append(ev, fmt.Sprintf("latency trend p=%s (adjusted %s)", formatP(c.NetP), formatP(c.NetQ)))
	}
	if c.Replays.Runs > 0 {
		ev = append(ev, fmt.Sprintf("failing seed rerun %d/%d failed", c.Replays.Failures, c.Replays.Runs))
	}
	if c.Polluter != nil {
		ev = append(ev, fmt.Sprintf("polluter search used %d runs", c.Polluter.Probes))
	}
	return ev
}

func causesByName(causes []Cause) map[string]*Cause {
	m := map[string]*Cause{}
	for i := range causes {
		m[causes[i].Test] = &causes[i]
	}
	return m
}

func showLatency(flaky []*TestStats) bool {
	for _, s := range flaky {
		if len(s.ByLatency) > 1 {
			return true
		}
	}
	return false
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func formatCount(c Count) string {
	return fmt.Sprintf("%d/%d (%.0f%%)", c.Failures, c.Runs, 100*c.Rate())
}

func formatP(p float64) string {
	if p < 0.001 {
		return "<0.001"
	}
	return strconv.FormatFloat(p, 'f', 3, 64)
}

func formatLevel(l float64) string {
	if l == 0 {
		return "no limit"
	}
	return strconv.FormatFloat(l, 'g', -1, 64)
}

func formatLatency(ms int) string {
	if ms == 0 {
		return "none"
	}
	return fmt.Sprintf("%d+/-%dms", ms, Jitter(ms))
}

func formatLatencies(ls []int) string {
	if len(ls) == 0 {
		return "-"
	}
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = formatLatency(l)
	}
	return strings.Join(parts, ", ")
}

func formatLevels(ls []float64) string {
	if len(ls) == 0 {
		return "-"
	}
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = formatLevel(l)
	}
	return strings.Join(parts, ", ")
}
