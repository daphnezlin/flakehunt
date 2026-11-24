package main

import (
	"context"
	"fmt"
	"math/rand"
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
		image    string
		runs     int
		workers  int
		cpus     float64
		timeout  time.Duration
		env      []string
		outDir   string
		baseSeed int64
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

			runner, err := NewDockerRunner(image, outDir, env, cpus, timeout)
			if err != nil {
				return err
			}

			runID, err := store.CreateRun(RunInfo{
				StartedAt: time.Now(), Image: image, BaseSeed: baseSeed,
				Workers: workers, CPUs: cpus, Env: env,
			})
			if err != nil {
				return fmt.Errorf("save run: %w", err)
			}

			// derive seeds from one base seed so a whole run is reproducible
			rng := rand.New(rand.NewSource(baseSeed))
			trials := make([]Trial, runs)
			for i := range trials {
				trials[i] = Trial{ID: i + 1, Seed: rng.Int63n(1 << 31)}
			}

			fmt.Fprintf(os.Stderr, "run #%d: %d trials of %s with %d workers (base seed %d)\n",
				runID, runs, image, workers, baseSeed)
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
			fmt.Fprintf(os.Stderr, "\n  finished in %s\n\n", time.Since(start).Round(time.Second))

			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "interrupted, run #%d has partial results\n\n", runID)
			} else if err := store.FinishRun(runID); err != nil {
				return err
			}

			printReport(agg, image, env)
			fmt.Printf("\nsaved as run #%d in %s (see it again with: flakehunt report %d)\n", runID, dbPath, runID)
			return ctx.Err()
		},
	}

	f := cmd.Flags()
	f.StringVar(&image, "image", "", "Docker image containing the project and its tests (required)")
	f.IntVar(&runs, "runs", 50, "number of trials")
	f.IntVar(&workers, "workers", max(1, runtime.NumCPU()/2), "trials running at once")
	f.Float64Var(&cpus, "cpus", 0, "CPU limit per trial, e.g. 0.5 (0 = unlimited)")
	f.DurationVar(&timeout, "timeout", 5*time.Minute, "per-trial timeout; longer runs count as hangs")
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
			fmt.Fprintln(w, "ID\tSTARTED\tIMAGE\tTRIALS\tWORKERS\tCPUS\tSTATUS")
			for _, r := range runs {
				status := "done"
				if !r.Finished {
					status = "incomplete"
				}
				cpus := "-"
				if r.CPUs > 0 {
					cpus = strconv.FormatFloat(r.CPUs, 'g', -1, 64)
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%s\t%s\n",
					r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"), r.Image, r.Trials, r.Workers, cpus, status)
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
			fmt.Printf("run #%d, %s, %d trials, %d workers\n\n",
				run.ID, run.StartedAt.Local().Format("2006-01-02 15:04"), agg.Trials, run.Workers)
			printReport(agg, run.Image, run.Env)
			return nil
		},
	}
}

func printReport(agg *Aggregate, image string, env []string) {
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
	var envFlags strings.Builder
	for _, e := range env {
		envFlags.WriteString(" -e " + e)
	}
	fmt.Println("\nReproduce (a seed where each flaky test failed):")
	for _, s := range flaky {
		fmt.Printf("  %s\n    docker run --rm%s %s pytest -q -p no:cacheprovider --randomly-seed=%d\n",
			s.Name, envFlags.String(), image, s.FailingSeeds[0])
	}
	fmt.Println("\nNote: a seed reproduces order and random data, not timing. Timing races may need several tries.")
}
