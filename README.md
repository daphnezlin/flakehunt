# flakehunt

Finds flaky tests by running a test suite many times in parallel containers
and reporting which tests both pass and fail on the same code, with
confidence intervals and a command to reproduce each one.

Status: phase 1 (parallel reruns on Docker, pytest).

## Setup

Requires Go 1.24+ and Docker running.

```
go mod tidy
go build -o flakehunt .
go test -race ./...
```

## Try it on the flaky zoo

```
cd ../flaky-zoo
docker build -t flaky-zoo .
docker run --rm flaky-zoo python calibrate_race.py    # prints FLAKY_ZOO_RACE_WAIT=...

cd ../flakehunt
./flakehunt run --image flaky-zoo --runs 50 --env FLAKY_ZOO_RACE_WAIT=<value>
```

Compare a throttled CPU (the race test should fail much more often):

```
./flakehunt run --image flaky-zoo --runs 50 --cpus 0.5 --env FLAKY_ZOO_RACE_WAIT=<value>
```

## Design

| File | Purpose |
|---|---|
| `runner.go` | `Runner` interface: Docker now, Kubernetes later, without changing the rest |
| `docker_runner.go` | One fresh container per trial, CPU limit, per-trial timeout, always cleaned up |
| `orchestrator.go` | Bounded worker pool, cancellation on Ctrl+C, single-goroutine aggregation (no locks) |
| `junit.go` | Parses JUnit XML results |
| `stats.go` | Wilson 95% confidence intervals (accurate for rare failures) |

A failing test is data, not an error: only a missing results file or a
container that won't start counts as an infrastructure error, and those
trials are excluded from failure rates.
