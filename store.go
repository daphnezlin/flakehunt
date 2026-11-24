package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
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
`

type Store struct {
	db *sql.DB
}

type RunInfo struct {
	ID        int64
	StartedAt time.Time
	Finished  bool
	Image     string
	BaseSeed  int64
	Workers   int
	CPUs      float64
	Env       []string
	Trials    int // only filled in by ListRuns
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// only one goroutine writes anyway, and this avoids "database is locked"
	db.SetMaxOpenConns(1)

	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", schema} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("init db: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateRun(r RunInfo) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO runs (started_at, image, base_seed, workers, cpus, env) VALUES (?, ?, ?, ?, ?, ?)`,
		r.StartedAt.UTC().Format(time.RFC3339), r.Image, r.BaseSeed, r.Workers, r.CPUs, strings.Join(r.Env, "\n"),
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
func (s *Store) SaveTrial(runID int64, res TrialResult) error {
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
	if _, err := tx.Exec(
		`INSERT INTO trials (run_id, trial_id, seed, status, error) VALUES (?, ?, ?, ?, ?)`,
		runID, res.Trial.ID, res.Trial.Seed, status, errMsg,
	); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO results (run_id, trial_id, test, outcome) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range res.Tests {
		outcome := "pass"
		if t.Skip {
			outcome = "skip"
		} else if t.Failed {
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
		SELECT r.id, r.started_at, r.finished_at IS NOT NULL, r.image, r.workers, r.cpus, COUNT(t.trial_id)
		FROM runs r LEFT JOIN trials t ON t.run_id = r.id
		GROUP BY r.id ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []RunInfo
	for rows.Next() {
		var r RunInfo
		var started string
		if err := rows.Scan(&r.ID, &started, &r.Finished, &r.Image, &r.Workers, &r.CPUs, &r.Trials); err != nil {
			return nil, err
		}
		r.StartedAt, _ = time.Parse(time.RFC3339, started)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *Store) GetRun(runID int64) (RunInfo, error) {
	var r RunInfo
	var started, env string
	err := s.db.QueryRow(
		`SELECT id, started_at, finished_at IS NOT NULL, image, base_seed, workers, cpus, env FROM runs WHERE id = ?`, runID,
	).Scan(&r.ID, &started, &r.Finished, &r.Image, &r.BaseSeed, &r.Workers, &r.CPUs, &env)
	if err == sql.ErrNoRows {
		return r, fmt.Errorf("no run with id %d", runID)
	}
	if err != nil {
		return r, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339, started)
	if env != "" {
		r.Env = strings.Split(env, "\n")
	}
	return r, nil
}

// LoadAggregate rebuilds the TrialResults for a run and feeds them through
// Aggregate.Add, so a saved report always matches what the live run printed.
func (s *Store) LoadAggregate(runID int64) (*Aggregate, error) {
	trials := map[int]*TrialResult{}
	var order []int

	rows, err := s.db.Query(`SELECT trial_id, seed, status, error FROM trials WHERE run_id = ? ORDER BY trial_id`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int
		var seed int64
		var status string
		var errMsg sql.NullString
		if err := rows.Scan(&id, &seed, &status, &errMsg); err != nil {
			rows.Close()
			return nil, err
		}
		tr := &TrialResult{Trial: Trial{ID: id, Seed: seed}}
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

	rows, err = s.db.Query(`SELECT trial_id, test, outcome FROM results WHERE run_id = ?`, runID)
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
		if tr := trials[id]; tr != nil {
			tr.Tests = append(tr.Tests, TestOutcome{Name: test, Failed: outcome == "fail", Skip: outcome == "skip"})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	agg := NewAggregate()
	for _, id := range order {
		agg.Add(*trials[id])
	}
	return agg, nil
}
