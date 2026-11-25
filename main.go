package main

import (
	"context"
	"fmt"
	"github.com/daphnezlin/flakehunt/internal/analysis"
	"github.com/daphnezlin/flakehunt/internal/report"
	"github.com/daphnezlin/flakehunt/internal/runner"
	"github.com/daphnezlin/flakehunt/internal/store"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var dbPath string

func main() {
	root := &cobra.Command{
		Use:   "flakehunt",
		Short: "Find flaky tests by running a suite many times under controlled conditions",
	}
	root.PersistentFlags().StringVar(&dbPath, "db", "flakehunt.db", "SQLite file where runs are saved")
	root.AddCommand(runCmd(), runsCmd(), reportCmd(), scoreCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func runCmd() *cobra.Command {
	var (
		image     string
		runs      int
		workers   int
		cpuLevels []float64
		latencies []int
		svc       runner.ServiceConfig
		svcCmd    string
		toxiImage string
		replays   int
		polluters bool
		markdown  string
		timeout   time.Duration
		env       []string
		outDir    string
		baseSeed  int64
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the test suite in parallel containers and report flaky tests",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			db, err := store.OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()

			var service *runner.ServiceConfig
			if svc.Image != "" {
				svc.Cmd = strings.Fields(svcCmd)
				service = &svc
			} else {
				for _, l := range latencies {
					if l != 0 {
						return fmt.Errorf("--latency-levels needs --service-image: latency is added between the tests and a service")
					}
				}
				latencies = nil
			}

			dr, err := runner.NewDockerRunner(ctx, image, outDir, env, workers, timeout, service, toxiImage)
			if err != nil {
				return err
			}

			runID, err := db.CreateRun(store.RunInfo{
				StartedAt: time.Now(), Image: image, BaseSeed: baseSeed, Workers: workers,
				CPULevels: cpuLevels, LatencyLevels: latencies, Service: svc.Image, Env: env,
			})
			if err != nil {
				return fmt.Errorf("save run: %w", err)
			}

			// everything comes from one base seed so a whole run is reproducible
			trials := runner.MakeTrials(runs, cpuLevels, latencies, baseSeed)

			fmt.Fprintf(os.Stderr, "run #%d: %d trials of %s with %d workers, cpu levels %s",
				runID, runs, image, workers, report.FormatLevels(cpuLevels))
			if service != nil {
				fmt.Fprintf(os.Stderr, ", latency %s (service %s)", report.FormatLatencies(latencies), svc.Image)
			}
			fmt.Fprintf(os.Stderr, " (base seed %d)\n", baseSeed)
			start := time.Now()

			// results are only handled here, so this loop is the single db writer
			agg := analysis.NewAggregate()
			for res := range runner.RunAll(ctx, dr, trials, workers) {
				if err := db.SaveTrial(runID, res); err != nil {
					return fmt.Errorf("save trial %d: %w", res.Trial.ID, err)
				}
				agg.Add(res)
				if res.Err != nil {
					fmt.Fprintf(os.Stderr, "  trial %d: infrastructure error: %v\n", res.Trial.ID, res.Err)
				}
				fmt.Fprintf(os.Stderr, "\r  %d/%d trials done", agg.Trials, runs)
			}
			fmt.Fprintf(os.Stderr, "\n  finished in %s\n", time.Since(start).Round(time.Second))

			lastID := len(trials) // replays and probes get ids after the normal trials
			var replayCounts map[string]*analysis.Count
			if ctx.Err() == nil && replays > 0 {
				replayCounts, lastID, err = runReplays(ctx, db, runID, dr, agg, replays, workers, lastID)
				if err != nil {
					return err
				}
			}

			var found map[string]analysis.PolluterResult
			if ctx.Err() == nil && polluters {
				found, err = runPolluterSearch(ctx, db, runID, dr, agg, replayCounts, lastID)
				if err != nil {
					return err
				}
			}
			fmt.Fprintln(os.Stderr)

			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "interrupted, run #%d has partial results\n\n", runID)
			} else if err := db.FinishRun(runID); err != nil {
				return err
			}

			causes := analysis.Analyze(agg, replayCounts, found)
			report.Print(agg, causes, image, env)
			fmt.Printf("\nsaved as run #%d in %s (see it again with: %s report %d)\n", runID, dbPath, os.Args[0], runID)

			if markdown != "" {
				run, err := db.GetRun(runID)
				if err != nil {
					return err
				}
				path, err := report.WriteMarkdownFile(markdown, run, agg, causes)
				if err != nil {
					return fmt.Errorf("write markdown report: %w", err)
				}
				fmt.Printf("markdown report: %s\n", path)
			}
			return ctx.Err()
		},
	}

	f := cmd.Flags()
	f.StringVar(&image, "image", "", "Docker image containing the project and its tests (required)")
	f.IntVar(&runs, "runs", 50, "number of trials")
	f.IntVar(&workers, "workers", max(1, runtime.NumCPU()/2), "trials running at once")
	f.Float64SliceVar(&cpuLevels, "cpu-levels", []float64{0, 1, 0.5}, "CPU limits to spread across trials, 0 = no limit")
	f.IntSliceVar(&latencies, "latency-levels", []int{0, 50, 150}, "network latency in ms added to the service, spread across trials (needs --service-image)")
	f.StringVar(&svc.Image, "service-image", "", "image for a service the tests talk to, e.g. an api or database")
	f.StringVar(&svcCmd, "service-cmd", "", "command for the service container (split on spaces, no quoting)")
	f.IntVar(&svc.Port, "service-port", 0, "port the service listens on")
	f.StringVar(&svc.Health, "service-health", "", "shell command that succeeds once the service is ready")
	f.StringArrayVar(&svc.Env, "service-env", nil, "env var telling tests where the service is, {addr} becomes host:port (repeatable)")
	f.StringVar(&toxiImage, "toxiproxy-image", "ghcr.io/shopify/toxiproxy:2.13.0", "toxiproxy image used to add latency")
	f.IntVar(&replays, "replays", 5, "reruns of each flaky test's failing seed, to check if the seed alone causes it (0 = off)")
	f.BoolVar(&polluters, "find-polluters", true, "for seed-dependent tests, check if they fail alone and find which earlier test breaks them")
	f.StringVar(&markdown, "markdown", defaultMarkdownPath, "where to write a markdown report, {id} is the run id (\"\" = don't write one)")
	f.DurationVar(&timeout, "timeout", 5*time.Minute, "per-trial timeout; longer runs count as hangs")
	f.StringArrayVar(&env, "env", nil, "env var for every trial, KEY=VALUE (repeatable)")
	f.StringVar(&outDir, "out", "flakehunt-out", "folder for per-trial results")
	f.Int64Var(&baseSeed, "seed", time.Now().UnixNano(), "base seed for the campaign")
	_ = cmd.MarkFlagRequired("image")
	return cmd
}

// runReplays reruns each flaky test's first failing seed with no cpu limit.
// If it fails every time, the seed (test order or random data) is enough to
// cause it. If it sometimes passes, something else like timing is involved.
func runReplays(ctx context.Context, db *store.Store, runID int64, rn runner.Runner, agg *analysis.Aggregate,
	per, workers, lastID int) (map[string]*analysis.Count, int, error) {

	var trials []runner.Trial
	for _, s := range agg.Sorted() {
		if !s.Flaky() {
			continue
		}
		for i := 0; i < per; i++ {
			lastID++
			trials = append(trials, runner.Trial{ID: lastID, Seed: s.Failing[0].Seed, ReplayFor: s.Name})
		}
	}
	if len(trials) == 0 {
		return nil, lastID, nil
	}

	fmt.Fprintf(os.Stderr, "  replaying failing seeds\n")
	counts := map[string]*analysis.Count{}
	done := 0
	for res := range runner.RunAll(ctx, rn, trials, workers) {
		if err := db.SaveTrial(runID, res); err != nil {
			return nil, lastID, fmt.Errorf("save replay %d: %w", res.Trial.ID, err)
		}
		done++
		fmt.Fprintf(os.Stderr, "\r  %d/%d replays done", done, len(trials))

		// same counting as Store.LoadReplays so live and saved reports match
		for _, o := range res.Tests {
			if o.Name != res.Trial.ReplayFor || o.Skip {
				continue
			}
			c := counts[o.Name]
			if c == nil {
				c = &analysis.Count{}
				counts[o.Name] = c
			}
			c.Runs++
			if o.Failed {
				c.Failures++
			}
		}
	}
	fmt.Fprintln(os.Stderr)
	return counts, lastID, nil
}

// runPolluterSearch runs FindPolluter for every test whose failing seed failed
// every replay. Searches run one probe at a time since each step depends on the
// last one. lastID is the highest trial id used so far.
func runPolluterSearch(ctx context.Context, db *store.Store, runID int64, rn runner.Runner, agg *analysis.Aggregate,
	replays map[string]*analysis.Count, lastID int) (map[string]analysis.PolluterResult, error) {

	found := map[string]analysis.PolluterResult{}
	nextID := lastID
	for _, c := range analysis.Analyze(agg, replays, nil) {
		if c.Label != "seed-dependent" {
			continue
		}
		s := agg.Tests[c.Test]
		seed := s.Failing[0].Seed

		var victim runner.TestOutcome
		var before []runner.TestOutcome
		for i, o := range s.FirstFailure {
			if o.Name == c.Test {
				victim, before = o, s.FirstFailure[:i]
				break
			}
		}

		fmt.Fprintf(os.Stderr, "  searching for what breaks %s\n", c.Test)
		var saveErr error
		probe := func(nodeIDs []string) (bool, bool, error) {
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			nextID++
			res := rn.Run(ctx, runner.Trial{ID: nextID, Seed: seed, ReplayFor: c.Test, Probe: "polluter", Tests: nodeIDs})
			if err := db.SaveTrial(runID, res); err != nil {
				saveErr = err
				return false, false, err
			}
			if res.Err != nil {
				return false, false, res.Err
			}
			if res.TimedOut {
				return false, false, fmt.Errorf("timed out")
			}
			for _, o := range res.Tests {
				if o.Name == c.Test && !o.Skip {
					return o.Failed, true, nil
				}
			}
			return false, false, nil
		}

		result := analysis.FindPolluter(victim, before, probe)
		if saveErr != nil {
			return nil, fmt.Errorf("save probe: %w", saveErr)
		}
		if err := db.SavePolluter(runID, c.Test, result); err != nil {
			return nil, err
		}
		found[c.Test] = result
	}
	return found, nil
}

func runsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runs",
		Short: "List saved runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := store.OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()

			runs, err := db.ListRuns()
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				fmt.Println("no runs yet")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTARTED\tIMAGE\tTRIALS\tWORKERS\tCPU LEVELS\tLATENCY\tSTATUS")
			for _, r := range runs {
				status := "done"
				if !r.Finished {
					status = "incomplete"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
					r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"), r.Image, r.Trials, r.Workers,
					report.FormatLevels(r.CPULevels), report.FormatLatencies(r.LatencyLevels), status)
			}
			return w.Flush()
		},
	}
}

const defaultMarkdownPath = "flakehunt-reports/run-{id}.md"

func reportCmd() *cobra.Command {
	var markdown string
	cmd := &cobra.Command{
		Use:   "report <run-id>",
		Short: "Show the report for a saved run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("run id should be a number, got %q", args[0])
			}

			db, err := store.OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()

			run, err := db.GetRun(runID)
			if err != nil {
				return err
			}
			agg, err := db.LoadAggregate(runID)
			if err != nil {
				return err
			}
			replays, err := db.LoadReplays(runID)
			if err != nil {
				return err
			}
			polluters, err := db.LoadPolluters(runID)
			if err != nil {
				return err
			}
			causes := analysis.Analyze(agg, replays, polluters)

			if markdown != "" {
				path, err := report.WriteMarkdownFile(markdown, run, agg, causes)
				if err != nil {
					return fmt.Errorf("write markdown report: %w", err)
				}
				fmt.Printf("markdown report: %s\n", path)
				return nil
			}
			fmt.Printf("run #%d, %s, %d trials, %d workers\n\n",
				run.ID, run.StartedAt.Local().Format("2006-01-02 15:04"), agg.Trials, run.Workers)
			report.Print(agg, causes, run.Image, run.Env)
			return nil
		},
	}
	cmd.Flags().StringVar(&markdown, "markdown", "", "write a markdown report to this path instead of printing ({id} is the run id)")
	// --markdown on its own uses the default path
	cmd.Flags().Lookup("markdown").NoOptDefVal = defaultMarkdownPath
	return cmd
}

func scoreCmd() *cobra.Command {
	var expectedPath string
	cmd := &cobra.Command{
		Use:   "score <run-id>...",
		Short: "Check saved runs against known causes, e.g. the flaky zoo",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			expected, err := analysis.LoadExpected(expectedPath)
			if err != nil {
				return err
			}
			db, err := store.OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()

			var total analysis.ScoreSummary
			right := map[string]int{} // runs where each test was scored correctly
			got := map[string][]string{}
			for _, arg := range args {
				runID, err := strconv.ParseInt(arg, 10, 64)
				if err != nil {
					return fmt.Errorf("run id should be a number, got %q", arg)
				}
				agg, err := db.LoadAggregate(runID)
				if err != nil {
					return err
				}
				replays, err := db.LoadReplays(runID)
				if err != nil {
					return err
				}
				polluters, err := db.LoadPolluters(runID)
				if err != nil {
					return err
				}
				verdicts := analysis.Score(agg, analysis.Analyze(agg, replays, polluters), expected)
				total.Add(analysis.Summarize(verdicts, expected))
				for _, v := range verdicts {
					if v.Correct {
						right[v.Test]++
					}
					got[v.Test] = append(got[v.Test], v.Got)
				}
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TEST\tEXPECTED\tCORRECT\tFLAKEHUNT SAID")
			names := make([]string, 0, len(got))
			for n := range got {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				want := expected[n].Cause
				if want == "" {
					want = "(not in file)"
				}
				fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\n", n, want, right[n], len(args), summarizeLabels(got[n]))
			}
			w.Flush()

			fmt.Printf("\nacross %d run(s):\n", len(args))
			fmt.Printf("  flaky tests found:   %d/%d (%s)\n", total.FlakyFound, total.FlakyTotal, pct(total.FlakyFound, total.FlakyTotal))
			fmt.Printf("  false positives:     %d/%d stable tests flagged\n", total.FalsePositives, total.Stable)
			fmt.Printf("  causes correct:      %d/%d (%s)\n", total.CausesRight, total.CausesTotal, pct(total.CausesRight, total.CausesTotal))
			if total.Polluted > 0 {
				fmt.Printf("  polluters correct:   %d/%d (%s)\n", total.PollutersRight, total.Polluted, pct(total.PollutersRight, total.Polluted))
			}
			if total.NotRun > 0 {
				fmt.Printf("  not run:             %d (in the expected file but missing from a run, not scored)\n", total.NotRun)
			}
			if total.Unexpected > 0 {
				fmt.Printf("  unexpected flaky:    %d (flagged but not in the expected file)\n", total.Unexpected)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&expectedPath, "expected", "flaky-zoo/expected.json", "json file with each test's true cause")
	return cmd
}

// summarizeLabels turns ["timing","timing","nondeterministic"] into "timing x2, nondeterministic x1".
func summarizeLabels(labels []string) string {
	counts := map[string]int{}
	var order []string
	for _, l := range labels {
		if counts[l] == 0 {
			order = append(order, l)
		}
		counts[l]++
	}
	parts := make([]string, len(order))
	for i, l := range order {
		parts[i] = fmt.Sprintf("%s x%d", l, counts[l])
		if len(labels) == 1 {
			parts[i] = l
		}
	}
	return strings.Join(parts, ", ")
}

func pct(a, b int) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(a)/float64(b))
}
