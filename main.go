package main

import (
	"context"
	"fmt"
	"github.com/daphnezlin/flakehunt/internal/analysis"
	"github.com/daphnezlin/flakehunt/internal/campaign"
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
		focusN    int
		focusMax  int
		catchRate float64
		markdown  string
		timeout   time.Duration
		testTO    time.Duration
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

			runs, err := detectTrials(runs, cmd.Flags().Changed("runs"), catchRate)
			if err != nil {
				return err
			}

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

			dr, err := runner.NewDockerRunner(ctx, image, outDir, env, workers, timeout, testTO, service, toxiImage)
			if err != nil {
				return err
			}

			cfg := campaign.Config{
				DetectTrials: runs, Workers: workers, CPULevels: cpuLevels, LatencyLevels: latencies,
				BaseSeed: baseSeed, Replays: replays, FindPolluters: polluters,
				FocusBatch: focusN, FocusLooks: focusMax, Log: os.Stderr,
			}
			runID, err := db.CreateRun(store.RunInfo{
				StartedAt: time.Now(), Image: image, BaseSeed: baseSeed, Workers: workers,
				CPULevels: cpuLevels, LatencyLevels: latencies, Service: svc.Image, Alpha: cfg.Alpha(), Env: env,
			})
			if err != nil {
				return fmt.Errorf("save run: %w", err)
			}

			fmt.Fprintf(os.Stderr, "run #%d: %d trials of %s with %d workers, cpu levels %s",
				runID, runs, image, workers, report.FormatLevels(cpuLevels))
			if service != nil {
				fmt.Fprintf(os.Stderr, ", latency %s (service %s)", report.FormatLatencies(latencies), svc.Image)
			}
			fmt.Fprintf(os.Stderr, " (base seed %d)\n", baseSeed)
			start := time.Now()

			// everything comes from one base seed so a whole run is reproducible
			res, err := campaign.New(cfg, db, runID, dr).Run(ctx)
			if err != nil {
				return err
			}
			st := res.Stats
			fmt.Fprintf(os.Stderr, "  finished in %s: %d trials (%d full suite, %d replays, %d polluter probes, %d focused), %d test executions\n\n",
				time.Since(start).Round(time.Second), st.Trials(), st.Detect, st.Replay, st.Probe, st.Focus, st.TestExecutions)

			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "interrupted, run #%d has partial results\n\n", runID)
			} else if err := db.FinishRun(runID); err != nil {
				return err
			}

			agg := res.Agg
			causes := res.Causes()
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
	f.IntVar(&runs, "runs", 60, "full-suite trials used to find flaky tests (60 catches a test that fails 5% of the time with 95% probability)")
	f.Float64Var(&catchRate, "catch-rate", 0, "set --runs so a test failing this often, e.g. 0.01, gets caught with 95% probability")
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
	f.IntVar(&focusN, "focus-batch", 36, "trials per focus batch: extra trials of only the tests whose cause is unclear (0 = off)")
	f.IntVar(&focusMax, "focus-batches", 10, "most focus batches to run before giving up on a test")
	f.StringVar(&markdown, "markdown", defaultMarkdownPath, "where to write a markdown report, {id} is the run id (\"\" = don't write one)")
	f.DurationVar(&timeout, "timeout", 5*time.Minute, "per-trial timeout, a safety net for the whole suite")
	f.DurationVar(&testTO, "test-timeout", time.Minute, "per-test timeout through pytest-timeout, so a hung test fails on its own (0 = off)")
	f.StringArrayVar(&env, "env", nil, "env var for every trial, KEY=VALUE (repeatable)")
	f.StringVar(&outDir, "out", "flakehunt-out", "folder for per-trial results")
	f.Int64Var(&baseSeed, "seed", time.Now().UnixNano(), "base seed for the campaign")
	_ = cmd.MarkFlagRequired("image")
	return cmd
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
			causes := analysis.AnalyzeAt(agg, replays, polluters, run.Alpha)

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
				verdicts := analysis.Score(agg, analysis.AnalyzeAt(agg, replays, polluters, run.Alpha), expected)
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

// detectTrials works out how many full-suite trials to run. --catch-rate picks
// the count for you; --runs sets it directly. Both at once is ambiguous.
func detectTrials(runs int, runsSet bool, catchRate float64) (int, error) {
	if catchRate == 0 {
		if runs < 1 {
			return 0, fmt.Errorf("--runs must be at least 1")
		}
		return runs, nil
	}
	if runsSet {
		return 0, fmt.Errorf("use --runs or --catch-rate, not both")
	}
	if catchRate <= 0 || catchRate >= 1 {
		return 0, fmt.Errorf("--catch-rate should be between 0 and 1, e.g. 0.02 for 2%%")
	}
	n := analysis.TrialsToCatch(catchRate, 0.95)
	fmt.Fprintf(os.Stderr, "--catch-rate %g: %d full-suite trials\n", catchRate, n)
	return n, nil
}
