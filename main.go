package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
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
	root.AddCommand(runCmd(), runsCmd(), reportCmd())
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
		replays   int
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

			store, err := OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer store.Close()

			runner, err := NewDockerRunner(ctx, image, outDir, env, workers, timeout)
			if err != nil {
				return err
			}

			runID, err := store.CreateRun(RunInfo{
				StartedAt: time.Now(), Image: image, BaseSeed: baseSeed,
				Workers: workers, CPULevels: cpuLevels, Env: env,
			})
			if err != nil {
				return fmt.Errorf("save run: %w", err)
			}

			// everything comes from one base seed so a whole run is reproducible
			trials := MakeTrials(runs, cpuLevels, baseSeed)

			fmt.Fprintf(os.Stderr, "run #%d: %d trials of %s with %d workers, cpu levels %s (base seed %d)\n",
				runID, runs, image, workers, formatLevels(cpuLevels), baseSeed)
			start := time.Now()

			// results are only handled here, so this loop is the single db writer
			agg := NewAggregate()
			for res := range RunAll(ctx, runner, trials, workers) {
				if err := store.SaveTrial(runID, res); err != nil {
					return fmt.Errorf("save trial %d: %w", res.Trial.ID, err)
				}
				agg.Add(res)
				if res.Err != nil {
					fmt.Fprintf(os.Stderr, "  trial %d: infrastructure error: %v\n", res.Trial.ID, res.Err)
				}
				fmt.Fprintf(os.Stderr, "\r  %d/%d trials done", agg.Trials, runs)
			}
			fmt.Fprintf(os.Stderr, "\n  finished in %s\n", time.Since(start).Round(time.Second))

			var replayCounts map[string]*Count
			if ctx.Err() == nil && replays > 0 {
				replayCounts, err = runReplays(ctx, store, runID, runner, agg, replays, workers, len(trials))
				if err != nil {
					return err
				}
			}
			fmt.Fprintln(os.Stderr)

			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "interrupted, run #%d has partial results\n\n", runID)
			} else if err := store.FinishRun(runID); err != nil {
				return err
			}

			printReport(agg, Analyze(agg, replayCounts), image, env)
			fmt.Printf("\nsaved as run #%d in %s (see it again with: flakehunt report %d)\n", runID, dbPath, runID)
			return ctx.Err()
		},
	}

	f := cmd.Flags()
	f.StringVar(&image, "image", "", "Docker image containing the project and its tests (required)")
	f.IntVar(&runs, "runs", 50, "number of trials")
	f.IntVar(&workers, "workers", max(1, runtime.NumCPU()/2), "trials running at once")
	f.Float64SliceVar(&cpuLevels, "cpu-levels", []float64{0, 1, 0.5}, "CPU limits to spread across trials, 0 = no limit")
	f.IntVar(&replays, "replays", 5, "reruns of each flaky test's failing seed, to check if the seed alone causes it (0 = off)")
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
func runReplays(ctx context.Context, store *Store, runID int64, runner Runner, agg *Aggregate,
	per, workers, lastID int) (map[string]*Count, error) {

	var trials []Trial
	for _, s := range agg.Sorted() {
		if !s.Flaky() {
			continue
		}
		for i := 0; i < per; i++ {
			lastID++
			trials = append(trials, Trial{ID: lastID, Seed: s.Failing[0].Seed, ReplayFor: s.Name})
		}
	}
	if len(trials) == 0 {
		return nil, nil
	}

	fmt.Fprintf(os.Stderr, "  replaying failing seeds\n")
	counts := map[string]*Count{}
	done := 0
	for res := range RunAll(ctx, runner, trials, workers) {
		if err := store.SaveTrial(runID, res); err != nil {
			return nil, fmt.Errorf("save replay %d: %w", res.Trial.ID, err)
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
				c = &Count{}
				counts[o.Name] = c
			}
			c.Runs++
			if o.Failed {
				c.Failures++
			}
		}
	}
	fmt.Fprintln(os.Stderr)
	return counts, nil
}

func runsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runs",
		Short: "List saved runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer store.Close()

			runs, err := store.ListRuns()
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				fmt.Println("no runs yet")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTARTED\tIMAGE\tTRIALS\tWORKERS\tCPU LEVELS\tSTATUS")
			for _, r := range runs {
				status := "done"
				if !r.Finished {
					status = "incomplete"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%s\t%s\n",
					r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"), r.Image, r.Trials, r.Workers,
					formatLevels(r.CPULevels), status)
			}
			return w.Flush()
		},
	}
}

func reportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report <run-id>",
		Short: "Show the report for a saved run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("run id should be a number, got %q", args[0])
			}

			store, err := OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer store.Close()

			run, err := store.GetRun(runID)
			if err != nil {
				return err
			}
			agg, err := store.LoadAggregate(runID)
			if err != nil {
				return err
			}
			replays, err := store.LoadReplays(runID)
			if err != nil {
				return err
			}
			fmt.Printf("run #%d, %s, %d trials, %d workers\n\n",
				run.ID, run.StartedAt.Local().Format("2006-01-02 15:04"), agg.Trials, run.Workers)
			printReport(agg, Analyze(agg, replays), run.Image, run.Env)
			return nil
		},
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
			c := s.ByCPU[l]
			fmt.Printf("  %s: %d/%d (%.0f%%)", formatLevel(l), c.Failures, c.Runs, 100*c.Rate())
		}
		fmt.Println()
	}

	fmt.Println("\nLikely causes:")
	for _, c := range causes {
		fmt.Printf("  %s\n    %s: %s\n", c.Test, c.Label, c.Detail)
		var evidence []string
		if c.CPUTested {
			evidence = append(evidence, fmt.Sprintf("cpu trend p=%s (adjusted %s)", formatP(c.CPUp), formatP(c.CPUq)))
		}
		if c.Replays.Runs > 0 {
			evidence = append(evidence, fmt.Sprintf("failing seed rerun %d/%d failed", c.Replays.Failures, c.Replays.Runs))
		}
		if len(evidence) > 0 {
			fmt.Printf("    %s\n", strings.Join(evidence, ", "))
		}
	}

	var envFlags strings.Builder
	for _, e := range env {
		envFlags.WriteString(" -e " + e)
	}
	fmt.Println("\nReproduce (a trial where each flaky test failed):")
	for _, s := range flaky {
		t := s.Failing[0]
		cpuFlag := ""
		if t.CPUs > 0 {
			cpuFlag = " --cpus " + formatLevel(t.CPUs)
		}
		fmt.Printf("  %s\n    docker run --rm%s%s %s pytest -q -p no:cacheprovider --randomly-seed=%d\n",
			s.Name, cpuFlag, envFlags.String(), image, t.Seed)
	}
	fmt.Println("\nNote: a seed reproduces order and random data, not timing. Timing races may need several tries.")
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
