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
		latencies []int
		svc       ServiceConfig
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

			store, err := OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer store.Close()

			var service *ServiceConfig
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

			runner, err := NewDockerRunner(ctx, image, outDir, env, workers, timeout, service, toxiImage)
			if err != nil {
				return err
			}

			runID, err := store.CreateRun(RunInfo{
				StartedAt: time.Now(), Image: image, BaseSeed: baseSeed, Workers: workers,
				CPULevels: cpuLevels, LatencyLevels: latencies, Service: svc.Image, Env: env,
			})
			if err != nil {
				return fmt.Errorf("save run: %w", err)
			}

			// everything comes from one base seed so a whole run is reproducible
			trials := MakeTrials(runs, cpuLevels, latencies, baseSeed)

			fmt.Fprintf(os.Stderr, "run #%d: %d trials of %s with %d workers, cpu levels %s",
				runID, runs, image, workers, formatLevels(cpuLevels))
			if service != nil {
				fmt.Fprintf(os.Stderr, ", latency %s (service %s)", formatLatencies(latencies), svc.Image)
			}
			fmt.Fprintf(os.Stderr, " (base seed %d)\n", baseSeed)
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

			lastID := len(trials) // replays and probes get ids after the normal trials
			var replayCounts map[string]*Count
			if ctx.Err() == nil && replays > 0 {
				replayCounts, lastID, err = runReplays(ctx, store, runID, runner, agg, replays, workers, lastID)
				if err != nil {
					return err
				}
			}

			var found map[string]PolluterResult
			if ctx.Err() == nil && polluters {
				found, err = runPolluterSearch(ctx, store, runID, runner, agg, replayCounts, lastID)
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

			causes := Analyze(agg, replayCounts, found)
			printReport(agg, causes, image, env)
			fmt.Printf("\nsaved as run #%d in %s (see it again with: %s report %d)\n", runID, dbPath, os.Args[0], runID)

			if markdown != "" {
				run, err := store.GetRun(runID)
				if err != nil {
					return err
				}
				path, err := writeMarkdownFile(markdown, run, agg, causes)
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
func runReplays(ctx context.Context, store *Store, runID int64, runner Runner, agg *Aggregate,
	per, workers, lastID int) (map[string]*Count, int, error) {

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
		return nil, lastID, nil
	}

	fmt.Fprintf(os.Stderr, "  replaying failing seeds\n")
	counts := map[string]*Count{}
	done := 0
	for res := range RunAll(ctx, runner, trials, workers) {
		if err := store.SaveTrial(runID, res); err != nil {
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
	return counts, lastID, nil
}

// runPolluterSearch runs FindPolluter for every test whose failing seed failed
// every replay. Searches run one probe at a time since each step depends on the
// last one. lastID is the highest trial id used so far.
func runPolluterSearch(ctx context.Context, store *Store, runID int64, runner Runner, agg *Aggregate,
	replays map[string]*Count, lastID int) (map[string]PolluterResult, error) {

	found := map[string]PolluterResult{}
	nextID := lastID
	for _, c := range Analyze(agg, replays, nil) {
		if c.Label != "seed-dependent" {
			continue
		}
		s := agg.Tests[c.Test]
		seed := s.Failing[0].Seed

		var victim TestOutcome
		var before []TestOutcome
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
			res := runner.Run(ctx, Trial{ID: nextID, Seed: seed, ReplayFor: c.Test, Probe: "polluter", Tests: nodeIDs})
			if err := store.SaveTrial(runID, res); err != nil {
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

		result := FindPolluter(victim, before, probe)
		if saveErr != nil {
			return nil, fmt.Errorf("save probe: %w", saveErr)
		}
		if err := store.SavePolluter(runID, c.Test, result); err != nil {
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
			fmt.Fprintln(w, "ID\tSTARTED\tIMAGE\tTRIALS\tWORKERS\tCPU LEVELS\tLATENCY\tSTATUS")
			for _, r := range runs {
				status := "done"
				if !r.Finished {
					status = "incomplete"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
					r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"), r.Image, r.Trials, r.Workers,
					formatLevels(r.CPULevels), formatLatencies(r.LatencyLevels), status)
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
			polluters, err := store.LoadPolluters(runID)
			if err != nil {
				return err
			}
			causes := Analyze(agg, replays, polluters)

			if markdown != "" {
				path, err := writeMarkdownFile(markdown, run, agg, causes)
				if err != nil {
					return fmt.Errorf("write markdown report: %w", err)
				}
				fmt.Printf("markdown report: %s\n", path)
				return nil
			}
			fmt.Printf("run #%d, %s, %d trials, %d workers\n\n",
				run.ID, run.StartedAt.Local().Format("2006-01-02 15:04"), agg.Trials, run.Workers)
			printReport(agg, causes, run.Image, run.Env)
			return nil
		},
	}
	cmd.Flags().StringVar(&markdown, "markdown", "", "write a markdown report to this path instead of printing ({id} is the run id)")
	// --markdown on its own uses the default path
	cmd.Flags().Lookup("markdown").NoOptDefVal = defaultMarkdownPath
	return cmd
}
