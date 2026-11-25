# flakehunt

Finds flaky tests and works out why they're flaky.

It runs a test suite many times in parallel Docker containers under different
conditions (CPU limits, network latency, test order), then reports which tests
are flaky, the likely cause, and the smallest command that reproduces each one.

Supports pytest for now.

## Example

```
Likely causes:
  tests.test_order::test_no_users_initially
    order-dependent: fails when it runs after tests.test_order::test_create_users, which leaves state behind
    CPU trend p=0.997, latency trend p=0.703, failing seed rerun 5/5 failed, polluter search used 7 runs
  tests.test_race::test_report_is_ready
    timing: fails more with less cpu, likely a race or a timeout that's too tight
    CPU trend p=0.002 (adjusted 0.010), latency trend p=0.625, failing seed rerun 3/5 failed
  tests.test_network::test_fetch_status
    network: fails more when the service responds slowly, likely a network timeout that's too tight
    CPU trend p=0.457, latency trend p<0.001, failing seed rerun 0/5 failed

Reproduce:
  tests.test_order::test_no_users_initially
    docker run --rm flaky-zoo pytest -q -p no:cacheprovider --randomly-seed=1259942384 \
      --randomly-dont-reorganize tests/test_order.py::test_create_users tests/test_order.py::test_no_users_initially
```

Each run also writes a markdown report with a section per flaky test that can be
pasted into a GitHub issue.

## Results

On [`flaky-zoo`](flaky-zoo), a set of tests that are flaky on purpose for known
reasons, across 3 runs of 135 trials:

| | |
|---|---|
| Flaky tests found | 12/12 |
| Stable tests wrongly flagged | 0/15 |
| Causes correct | 12/12 |
| Polluting test named correctly | 3/3 |

This is a synthetic benchmark, so it mostly checks that nothing is broken.
Real-world accuracy is next (see [Status](#status)).

## Usage

Needs Go 1.24+ and Docker.

```
go build -o flakehunt .
./flakehunt run --image <image with your project and its tests> --runs 135
```

| Command | What it does |
|---|---|
| `run` | Runs the suite, analyzes it, saves the run and writes a markdown report |
| `runs` | Lists saved runs |
| `report <id>` | Shows a saved run again (`--markdown` to write the report file) |
| `score <id>...` | Compares runs against known causes in a json file |

Useful `run` flags: `--runs`, `--workers`, `--cpu-levels` (default `0,1,0.5`),
`--latency-levels` (default `0,50,150` ms), `--replays`, `--env KEY=VALUE`.

If the tests talk to a service (an api, a database), flakehunt can add network
latency in front of it:

```
./flakehunt run --image my-tests \
  --service-image postgres:16 --service-port 5432 \
  --service-health "pg_isready" \
  --service-env 'DATABASE_URL=postgres://postgres@{addr}/test'
```

`{addr}` becomes the address of the proxy the tests should connect to.

## How it works

1. **Trials.** Each trial runs the whole suite in a fresh container with a pytest-randomly
   seed, which controls test order and random data. Trials get a CPU limit and a network
   latency, with every combination getting the same number of trials.
2. **Isolation.** Each trial is pinned to its own CPU cores. With a service, each trial gets its
   own private Docker network with its own copy of the service and a
   [Toxiproxy](https://github.com/Shopify/toxiproxy) in front of it that adds the latency.
3. **Detection.** A test that both passed and failed on the same code is flaky. Failure rates
   come with Wilson confidence intervals.
4. **Cause.** For each flaky test:
   - a one-sided permutation trend test checks if it fails more with less CPU or more latency,
     with Benjamini-Hochberg correction across every check
   - its failing seed is rerun with no limits. Failing every time means the seed alone causes it
   - for seed-dependent tests, it's run alone. If it passes alone, a binary search over the tests
     that ran before it finds the one that breaks it
5. **Storage.** Every trial is saved to SQLite, so any run can be re-reported or scored later.

## Layout

```
main.go             commands and flags
internal/runner     running trials: docker, service + toxiproxy, pytest, junit parsing
internal/analysis   counting results, statistics, causes, polluter search, scoring
internal/store      sqlite storage and migrations
internal/report     terminal and markdown reports
flaky-zoo/          tests with known causes, used as a benchmark
```

## Design notes

- **SQLite with a pure Go driver.** It's a single process CLI, so an embedded db fits, and
  the pure Go driver keeps it a single binary with no C toolchain.
- **CPU pinning.** Without it, parallel trials slowed each other down and a race test failed
  100% of the time with 5 workers but 0% with 1. Pinning makes the CPU limit the only variable.
- **Balanced conditions.** Every CPU and latency combination gets the same number of trials, so
  the two are independent and each can be tested on its own.
- **Permutation test instead of chi-square.** Counts per level are small (10 to 45 trials), which is
  where the textbook approximations get unreliable.
- **Toxiproxy per trial.** A shared proxy would let one trial's latency affect another's.
- **Migrations.** Old db files upgrade on open (`PRAGMA user_version`) instead of breaking.

## Limitations

- pytest only.
- Network faults need the tests to read the service address from config (like `DATABASE_URL`).
- On laptops with mixed fast and slow cores and no fan, parallel trials still affect each other's
  speed even when pinned, so CPU results compare levels within a run rather than exact timings.
- The polluter search assumes one test is enough to break the victim. If it takes several together,
  it says so instead of guessing.
- Tests that fail when run twice in the same process (NIO in the IDoFT dataset) aren't detected,
  since each trial runs each test once.

## Trying it on flaky-zoo

```
cd flaky-zoo && docker build -t flaky-zoo . && cd ..
./flaky-zoo/run.sh --runs 135
./flakehunt score <run id>
```

## Tests

```
go test -race ./...
```

## Status

Done: detection, cause analysis, polluter search, network faults, markdown reports, scoring.

Next: adaptive sampling (stop rerunning tests that are clearly stable), running on real
open source projects and scoring against [IDoFT](https://github.com/TestingResearchIllinois/idoft),
CI, and releases.