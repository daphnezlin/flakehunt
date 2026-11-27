package store

import (
	"database/sql"
	"fmt"
	"github.com/daphnezlin/flakehunt/internal/analysis"
	"github.com/daphnezlin/flakehunt/internal/runner"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// migrations run in order, once each. user_version in the db file tracks
// how many have been applied, so old db files get upgraded instead of breaking.
// never edit an old migration, add a new one.
var migrations = []string{
	// 1: initial schema
	`
	CREATE TABLE IF NOT EXISTS runs (
		id          INTEGER PRIMARY KEY,
		started_at  TEXT NOT NULL,
		finished_at TEXT,
		image       TEXT NOT NULL,
		base_seed   INTEGER NOT NULL,
		workers     INTEGER NOT NULL,
		cpus        REAL NOT NULL,
		env         TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS trials (
		run_id   INTEGER NOT NULL REFERENCES runs(id),
		trial_id INTEGER NOT NULL,
		seed     INTEGER NOT NULL,
		status   TEXT NOT NULL, -- ok, timeout, error
		error    TEXT,
		PRIMARY KEY (run_id, trial_id)
	);
	CREATE TABLE IF NOT EXISTS results (
		run_id   INTEGER NOT NULL,
		trial_id INTEGER NOT NULL,
		test     TEXT NOT NULL,
		outcome  TEXT NOT NULL, -- pass, fail, skip
		FOREIGN KEY (run_id, trial_id) REFERENCES trials(run_id, trial_id)
	);
	CREATE INDEX IF NOT EXISTS results_by_test ON results(run_id, test);
	`,
	// 2: per-trial cpu limit and pinning. runs.cpus is unused from here on
	`
	ALTER TABLE runs ADD COLUMN cpu_levels TEXT NOT NULL DEFAULT '';
	ALTER TABLE trials ADD COLUMN cpus REAL NOT NULL DEFAULT 0;
	ALTER TABLE trials ADD COLUMN cpuset TEXT NOT NULL DEFAULT '';
	`,
	// 3: replay trials ('' = normal trial)
	`
	ALTER TABLE trials ADD COLUMN replay_for TEXT NOT NULL DEFAULT '';
	`,
	// 4: polluter search runs and what they found
	`
	ALTER TABLE trials ADD COLUMN probe TEXT NOT NULL DEFAULT '';
	CREATE TABLE polluters (
		run_id        INTEGER NOT NULL REFERENCES runs(id),
		test          TEXT NOT NULL,
		victim_node   TEXT NOT NULL,
		fails_alone   INTEGER NOT NULL,
		polluter      TEXT NOT NULL,
		polluter_node TEXT NOT NULL,
		note          TEXT NOT NULL,
		probes        INTEGER NOT NULL,
		PRIMARY KEY (run_id, test)
	);
	`,
	// 5: network latency per trial
	`
	ALTER TABLE runs ADD COLUMN latency_levels TEXT NOT NULL DEFAULT '';
	ALTER TABLE runs ADD COLUMN service TEXT NOT NULL DEFAULT '';
	ALTER TABLE trials ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT 0;
	`,
	// 6: significance threshold, stricter for runs that stop early
	`
	ALTER TABLE runs ADD COLUMN alpha REAL NOT NULL DEFAULT 0.05;
	`,
	// 7: polluter search couldn't reproduce the failure in its original order
	`
	ALTER TABLE polluters ADD COLUMN unreproduced INTEGER NOT NULL DEFAULT 0;
	`,
	// 8: a replay can check several tests that failed with the same seed.
	// trials.replay_for keeps the first one, so replay_for = '' still means a
	// normal trial
	`
	CREATE TABLE replay_targets (
		run_id   INTEGER NOT NULL,
		trial_id INTEGER NOT NULL,
		test     TEXT NOT NULL,
		PRIMARY KEY (run_id, trial_id, test),
		FOREIGN KEY (run_id, trial_id) REFERENCES trials(run_id, trial_id)
	);
	INSERT INTO replay_targets (run_id, trial_id, test)
		SELECT run_id, trial_id, replay_for FROM trials WHERE replay_for != '';
	`,
}

type Store struct {
	db *sql.DB
}

type RunInfo struct {
	ID            int64
	StartedAt     time.Time
	Finished      bool
	Image         string
	BaseSeed      int64
	Workers       int
	CPULevels     []float64
	LatencyLevels []int
	Service       string  // service image, "" if none
	Alpha         float64 // significance threshold the run's verdicts use
	Env           []string
	Trials        int // only filled in by ListRuns
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// only one goroutine writes anyway, and this avoids "database is locked"
	db.SetMaxOpenConns(1)

	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("init db: %w", err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		// PRAGMA can't take ? params, i+1 is our own int so this is safe
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func joinFloats(fs []float64) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strings.Join(parts, ",")
}

func intsToFloats(is []int) []float64 {
	out := make([]float64, len(is))
	for i, v := range is {
		out[i] = float64(v)
	}
	return out
}

func floatsToInts(fs []float64) []int {
	out := make([]int, len(fs))
	for i, v := range fs {
		out[i] = int(v)
	}
	return out
}

func splitFloats(s string) []float64 {
	if s == "" {
		return nil
	}
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if f, err := strconv.ParseFloat(p, 64); err == nil {
			out = append(out, f)
		}
	}
	return out
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateRun(r RunInfo) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO runs (started_at, image, base_seed, workers, cpus, cpu_levels, latency_levels, service, alpha, env)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
		r.StartedAt.UTC().Format(time.RFC3339), r.Image, r.BaseSeed, r.Workers, joinFloats(r.CPULevels),
		joinFloats(intsToFloats(r.LatencyLevels)), r.Service, alphaOrDefault(r.Alpha), strings.Join(r.Env, "\n"),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishRun(runID int64) error {
	_, err := s.db.Exec(`UPDATE runs SET finished_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), runID)
	return err
}

// SaveTrial writes a trial and all its test results in one transaction,
// so a crash never leaves a trial half saved.
func (s *Store) SaveTrial(runID int64, res runner.TrialResult) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	status, errMsg := "ok", ""
	switch {
	case res.TimedOut:
		status = "timeout"
	case res.Err != nil:
		status, errMsg = "error", res.Err.Error()
	}
	replayFor := ""
	if len(res.Trial.ReplayFor) > 0 {
		replayFor = res.Trial.ReplayFor[0]
	}
	if _, err := tx.Exec(
		`INSERT INTO trials (run_id, trial_id, seed, cpus, latency_ms, cpuset, replay_for, probe, status, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, res.Trial.ID, res.Trial.Seed, res.Trial.CPUs, res.Trial.LatencyMS, res.CPUSet,
		replayFor, res.Trial.Probe, status, errMsg,
	); err != nil {
		return err
	}
	for _, test := range res.Trial.ReplayFor {
		if _, err := tx.Exec(`INSERT INTO replay_targets (run_id, trial_id, test) VALUES (?, ?, ?)`,
			runID, res.Trial.ID, test); err != nil {
			return err
		}
	}

	stmt, err := tx.Prepare(`INSERT INTO results (run_id, trial_id, test, outcome) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range res.Tests {
		outcome := "pass"
		switch {
		case t.Skip:
			outcome = "skip"
		case t.Hung:
			outcome = "timeout"
		case t.Failed:
			outcome = "fail"
		}
		if _, err := stmt.Exec(runID, res.Trial.ID, t.Name, outcome); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListRuns() ([]RunInfo, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.started_at, r.finished_at IS NOT NULL, r.image, r.workers, r.cpu_levels, r.latency_levels,
			COUNT(CASE WHEN t.replay_for = '' THEN 1 END)
		FROM runs r LEFT JOIN trials t ON t.run_id = r.id
		GROUP BY r.id ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunInfo
	for rows.Next() {
		var r RunInfo
		var started, levels, latencies string
		if err := rows.Scan(&r.ID, &started, &r.Finished, &r.Image, &r.Workers, &levels, &latencies, &r.Trials); err != nil {
			return nil, err
		}
		r.StartedAt, _ = time.Parse(time.RFC3339, started)
		r.CPULevels = splitFloats(levels)
		r.LatencyLevels = floatsToInts(splitFloats(latencies))
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *Store) GetRun(runID int64) (RunInfo, error) {
	var r RunInfo
	var started, levels, latencies, env string
	err := s.db.QueryRow(
		`SELECT id, started_at, finished_at IS NOT NULL, image, base_seed, workers, cpu_levels, latency_levels, service, alpha, env
		FROM runs WHERE id = ?`, runID,
	).Scan(&r.ID, &started, &r.Finished, &r.Image, &r.BaseSeed, &r.Workers, &levels, &latencies, &r.Service, &r.Alpha, &env)
	if err == sql.ErrNoRows {
		return r, fmt.Errorf("no run with id %d", runID)
	}
	if err != nil {
		return r, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339, started)
	r.CPULevels = splitFloats(levels)
	r.LatencyLevels = floatsToInts(splitFloats(latencies))
	if env != "" {
		r.Env = strings.Split(env, "\n")
	}
	return r, nil
}

// LoadAggregate rebuilds the TrialResults for a run and feeds them through
// Aggregate.Add, so a saved report always matches what the live run printed.
func (s *Store) LoadAggregate(runID int64) (*analysis.Aggregate, error) {
	trials := map[int]*runner.TrialResult{}
	var order []int

	rows, err := s.db.Query(`SELECT trial_id, seed, cpus, latency_ms, cpuset, probe, status, error FROM trials WHERE run_id = ? AND replay_for = '' ORDER BY trial_id`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int
		var seed int64
		var cpus float64
		var latency int
		var cpuset, probe, status string
		var errMsg sql.NullString
		if err := rows.Scan(&id, &seed, &cpus, &latency, &cpuset, &probe, &status, &errMsg); err != nil {
			rows.Close()
			return nil, err
		}
		tr := &runner.TrialResult{Trial: runner.Trial{ID: id, Seed: seed, CPUs: cpus, LatencyMS: latency, Probe: probe}, CPUSet: cpuset}
		switch status {
		case "timeout":
			tr.TimedOut = true
		case "error":
			tr.Err = fmt.Errorf("%s", errMsg.String)
		}
		trials[id] = tr
		order = append(order, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT trial_id, test, outcome FROM results WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var test, outcome string
		if err := rows.Scan(&id, &test, &outcome); err != nil {
			return nil, err
		}
		if tr := trials[id]; tr != nil { // nil for replay trials
			tr.Tests = append(tr.Tests, runner.TestOutcome{Name: test, Failed: outcome == "fail" || outcome == "timeout",
				Hung: outcome == "timeout", Skip: outcome == "skip"})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	agg := analysis.NewAggregate()
	for _, id := range order {
		agg.Add(*trials[id])
	}
	return agg, nil
}

// LoadReplays counts how the target test did in each replay trial, keyed by test name.
func (s *Store) LoadReplays(runID int64) (map[string]*analysis.Count, error) {
	rows, err := s.db.Query(`
		SELECT rt.test, r.outcome
		FROM replay_targets rt
		JOIN trials t ON t.run_id = rt.run_id AND t.trial_id = rt.trial_id
		JOIN results r ON r.run_id = rt.run_id AND r.trial_id = rt.trial_id AND r.test = rt.test
		WHERE rt.run_id = ? AND t.probe = ''`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	replays := map[string]*analysis.Count{}
	for rows.Next() {
		var test, outcome string
		if err := rows.Scan(&test, &outcome); err != nil {
			return nil, err
		}
		if outcome == "skip" {
			continue
		}
		c := replays[test]
		if c == nil {
			c = &analysis.Count{}
			replays[test] = c
		}
		c.Runs++
		if outcome == "fail" || outcome == "timeout" {
			c.Failures++
		}
	}
	return replays, rows.Err()
}

func (s *Store) SavePolluter(runID int64, test string, p analysis.PolluterResult) error {
	_, err := s.db.Exec(`INSERT INTO polluters
		(run_id, test, victim_node, fails_alone, polluter, polluter_node, note, probes, unreproduced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, test, p.VictimNode, p.FailsAlone, p.Polluter, p.PolluterNode, p.Note, p.Probes, p.Unreproduced)
	return err
}

func (s *Store) LoadPolluters(runID int64) (map[string]analysis.PolluterResult, error) {
	rows, err := s.db.Query(`SELECT test, victim_node, fails_alone, polluter, polluter_node, note, probes, unreproduced
		FROM polluters WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]analysis.PolluterResult{}
	for rows.Next() {
		var test string
		var p analysis.PolluterResult
		if err := rows.Scan(&test, &p.VictimNode, &p.FailsAlone, &p.Polluter, &p.PolluterNode, &p.Note, &p.Probes, &p.Unreproduced); err != nil {
			return nil, err
		}
		out[test] = p
	}
	return out, rows.Err()
}

func alphaOrDefault(a float64) float64 {
	if a == 0 {
		return analysis.DefaultAlpha
	}
	return a
}
