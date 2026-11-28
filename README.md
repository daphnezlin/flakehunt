# flakehunt

[![ci](https://github.com/daphnezlin/flakehunt/actions/workflows/ci.yml/badge.svg)](https://github.com/daphnezlin/flakehunt/actions/workflows/ci.yml)

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
    CPU trend p=0.059 (adjusted 0.234), latency trend p=0.137 (adjusted 0.410), failing seed rerun 5/5 failed (3% chance at its 48% failure rate), polluter search used 10 runs
  tests.test_race::test_report_is_ready
    timing: fails more with less cpu, likely a race or a timeout that's too tight
    CPU trend p=<0.001 (adjusted <0.001), latency trend p=0.320 (adjusted 0.664), failing seed rerun 2/5 failed
  tests.test_network::test_fetch_status
    network: fails more when the service responds slowly, likely a network timeout that's too tight
    CPU trend p=0.541 (adjusted 0.699), latency trend p=<0.001 (adjusted <0.001), failing seed rerun 0/5 failed
  tests.test_deadlock::test_transfer_between_accounts
    hang: never finished in 17 of its 17 failures (hit the per-test timeout), likely a deadlock or waiting on something that doesn't come
    CPU trend p=0.952 (adjusted 1.000), latency trend p=0.424 (adjusted 0.699), failing seed rerun 1/5 failed, hit the per-test timeout in 17 of 17 failures

Reproduce:
  tests.test_order::test_no_users_initially
    docker run --rm flaky-zoo pytest -q -p no:cacheprovider --randomly-seed=388634590 \
      --randomly-dont-reorganize tests/test_order.py::test_create_users tests/test_order.py::test_no_users_initially

tests marked stable passed all 60 runs, so one failing more than 4.9% of the time had a 95%
chance of being caught. Rarer flakiness can still hide (--catch-rate to look harder).
```

Each run also writes a markdown report with a section per flaky test that can be
pasted into a GitHub issue.

## Results

### flaky-zoo

On [`flaky-zoo`](flaky-zoo), a set of tests that are flaky on purpose for known reasons: a timing
race, a network timeout, an order dependency, a random data edge case, a deadlock that hangs, and
a timestamp check that breaks when a second ticks over mid-test. 3 runs each of the default
adaptive design and a fixed design (135 full-suite trials, no focus trials) on a MacBook Air:

| | Adaptive, default | Fixed, 135 trials |
|---|---|---|
| Flaky tests found | 18/18 | 18/18 |
| Stable tests wrongly flagged | 0/15 | 0/15 |
| Causes correct | 17/18 | 16/18 |
| Polluting test named correctly | 3/3 | 3/3 |
| Test executions per run | 1,173 to 1,607 | 1,857 to 1,901 |
| Wall time per run | 3m27s to 7m48s | 3m8s to 3m30s |

Every miss is the timestamp test. Its failure rate only goes from about 10% to 20% at half a CPU.
Adaptive got it in 2 of 3 runs and fixed in 1 of 3. In the adaptive miss it came out flat (15% vs
16%, roughly a 4% chance given measured timings). In the fixed misses the trend was there but too
weak to survive the correction (in one, p = 0.018, adjusted 0.074). Both called it
nondeterministic, which is the honest answer for that data.

Adaptive ran about a quarter fewer test executions and got one more cause right, since its extra
trials go to the tests that need them instead of rerunning the whole suite. It's slower in wall
time, because focus batches run one after another with a check in between, and on the zoo every
test takes milliseconds so container startup dominates. The run that needed all 10 focus batches
took 7m48s. On a real suite with hundreds of tests, a focused trial runs 1 or 2 tests instead of
all of them, so the savings should be much bigger. That still needs a real suite with timing
flakiness to check: webssh's flaky tests are all order-dependent, so it needed no focus trials.

The fixed runs also turned up a bug: in one, the deadlock test was labelled order-dependent even
though every failure was a timeout (see [Design notes](#design-notes)). The numbers above are after
the fix, re-scored from the saved runs.

### webssh

[webssh](https://github.com/huashengdun/webssh) at the commit where
[IDoFT](https://github.com/TestingResearchIllinois/idoft) found its flaky tests (setup in
[`benchmarks/webssh`](benchmarks/webssh)). IDoFT lists 29 flaky tests for it: 9 order-dependent
and 20 not order-dependent (NOD). One run with the defaults:

| | |
|---|---|
| IDoFT's flaky tests found | 27/29 |
| Labelled order-dependent, with the polluting test named | 27/27 |
| Other tests flagged flaky | 0/60 |
| Causes matching IDoFT | 9/29 |

All 27 fail together. `TestAppWithTooManyConnections` sets the global `maxconn` option to 1 and
`TestAppBasic` never sets it back. It also leaves a connection open, so when it runs first,
every Basic test that opens a connection gets "Too many live connections". The bug was still in
webssh's latest code, so I sent a one-line fix upstream:
[huashengdun/webssh#423](https://github.com/huashengdun/webssh/pull/423).

The low cause score is mostly IDoFT's labels. It calls 18 of the 27 NOD, but they don't look it:
the repro command fails every time, each test passes on its own, and none of them failed in any
other trial, including at half a CPU. So flakehunt found no sign of flakiness besides test order.
The 2 tests it missed passed all 60 trials, so if they're flaky, it's rarer than about 5%.

The first version of the run cost far more than it needed to, because it handled each of the 27
victims separately. Sharing that work (see [Design notes](#design-notes)) gave the same results
for a third of the cost:

| | Before | After |
|---|---|---|
| Replays | 135 | 5 |
| Polluter search runs | 224 | 64 |
| Test executions | 18,682 | 6,031 |
| Wall time | 8m48s | 4m15s |

## Usage

Needs Go 1.24+ and Docker.

```
go build -o flakehunt .
./flakehunt run --image <image with your project and its tests>
```

| Command | What it does |
|---|---|
| `run` | Runs the suite, analyzes it, saves the run and writes a markdown report |
| `runs` | Lists saved runs |
| `report <id>` | Shows a saved run again (`--markdown` to write the report file) |
| `score <id>...` | Compares runs against known causes in a json file |

Useful `run` flags:

| Flag | Default | |
|---|---|---|
| `--runs` | 60 | full-suite trials. 60 catches a test that fails 5% of the time with 95% probability |
| `--catch-rate` | | set `--runs` for you, e.g. `0.01` to catch 1% flaky tests (299 trials) |
| `--cpu-levels` | `0,1,0.5` | CPU limits, 0 = no limit |
| `--latency-levels` | `0,50,150` | network latency in ms (needs a service) |
| `--focus-batch` | 36 | trials per focus batch, 0 turns focusing off |
| `--focus-batches` | 10 | most focus batches before giving up on a test |
| `--replays` | 5 | reruns of each flaky test's failing seed, up to 3 rounds for tests that fail often anyway |
| `--test-timeout` | 1m | per-test timeout, a test that hits it counts as a hang |
| `--workers` | half your cores | trials at once |

If the tests talk to a service (an api, a database), flakehunt can add network
latency in front of it:

```
./flakehunt run --image my-tests \
  --service-image postgres:16 --service-port 5432 \
  --service-health "pg_isready" \
  --service-env 'DATABASE_URL=postgres://postgres@{addr}/test'
```

`{addr}` becomes the address the tests should connect to.

## How it works

1. **Detect.** Full-suite trials, each in a fresh container with a pytest-randomly seed, which
   controls test order and random data. Trials get a CPU limit and a network latency, with every
   combination getting the same number of trials. A test that both passed and failed on the same
   code is flaky. Failure rates come with Wilson confidence intervals.
2. **Isolate.** Each trial is pinned to its own CPU cores. With a service, each trial gets its own
   private Docker network and its own copy of the service, with a
   [Toxiproxy](https://github.com/Shopify/toxiproxy) in front of it when latency is added.
3. **Explain.** For each flaky test:
   - a one-sided permutation trend test checks if it fails more with less CPU or more latency,
     with Benjamini-Hochberg correction across every check
   - tests that hit `--test-timeout` in at least half their failures are labelled hangs, even if
     their seed seems to explain them too
   - its failing seed is rerun with no limits. Failing every time means the seed alone causes it,
     but only if that streak is unlikely given how often the test fails anyway. If it isn't yet,
     the seed gets more reruns. Tests that first failed in the same trial share these reruns
   - for those, it's run alone. Failing alone means random data. Passing alone means another test
     breaks it. Polluters already found for other tests are tried first, then a binary search over
     the tests that ran before it finds which one. If rerunning the exact failing order passes, the
     seed wasn't the cause after all
4. **Focus.** Flaky tests whose cause is still unclear get extra trials running only those tests,
   in batches, at the mildest and harshest CPU limit. Latency is only varied for tests with some
   sign of a network effect. Each batch is followed by another check. Tests drop out as soon as
   their cause is clear, or after 2 batches with no sign of a CPU or network effect at all.
5. **Store.** Every trial is saved to SQLite, so any run can be re-reported or scored later.

## Layout

```
main.go             commands and flags
internal/campaign   one run end to end: detect, replays, polluter search, focus
internal/runner     running trials: docker, service + toxiproxy, pytest, junit parsing
internal/analysis   counting results, statistics, causes, polluter search, scoring
internal/store      sqlite storage and migrations
internal/report     terminal and markdown reports
flaky-zoo/          tests with known causes, used as a benchmark
benchmarks/         open source suites from IDoFT, with Dockerfiles and expected causes
```

## Design notes

- **SQLite with a pure Go driver.** It's a single process CLI, so an embedded db fits, and
  the pure Go driver keeps it a single binary with no C toolchain.
- **CPU pinning.** Without it, parallel trials slowed each other down and a race test failed
  100% of the time with 5 workers but 0% with 1. Pinning makes the CPU limit the only variable.
- **Balanced conditions.** Every CPU and latency combination gets the same number of trials, so
  the two are independent and each can be tested on its own.
- **Permutation test instead of chi-square.** Counts per level are small (10 to 50 trials), which
  is where the textbook approximations get unreliable.
- **Early stopping without extra false positives.** Checking after every focus batch and stopping
  when something looks significant gives chance more tries. The first version split the threshold
  evenly across checks (Bonferroni) and missed a real CPU effect. It now uses a Haybittle-Peto
  boundary: early checks need p < 0.001 to stop, and the final check uses 0.047, which keeps the
  overall false positive rate at 5%.
- **Stratified trend test.** A test can fail at a different base rate when it runs alone in a focus
  trial than in the full suite. Mixing the two blurred the CPU comparison, so failures are only
  shuffled within each kind of trial.
- **Seed evidence beats a chance trend.** Conditions are assigned at random, independently of seeds.
  So if a test's failing seed fails every rerun with no limits, any CPU or latency trend for it can
  only be chance. Those tests are labelled from the polluter search, with the trend as a note.
- **A hang beats seed evidence.** Timing out on most failures is seen directly, while "the seed
  causes it" is inferred from a few reruns. In a zoo run, the deadlock test's failing seed failed 5
  reruns in a row by chance (about 0.5% at its 35% failure rate), so it was labelled order-dependent
  despite timing out on all 47 of its failures. Hangs are now labelled hangs, with what the seed and
  polluter search found in the detail, so a hang caused by another test still names that test.
- **Replays are judged against the test's own failure rate.** A test that fails 90% of the time
  fails 5 reruns in a row 59% of the time, seed or no seed. So a failing-every-time streak only
  counts when its chance is under 5% (failure rate to the power of reruns), and tests that fail
  often get more reruns until it's clear either way. This came from a zoo run where a slow
  timing test was blamed on test order.
- **CPU levels are scored by slowdown.** The trend test weights each CPU limit by how much it slows
  a test down: 2 for 0.5, 1 for no limit or 1 CPU. Python test code holds the GIL, so extra
  cores don't speed it up. Scoring the levels as even steps instead let noise between no limit
  and 1 hide a real effect at 0.5.
- **Focus trials at the extremes.** For a trend, the difference between the mildest and harshest
  condition is what carries the evidence, so focus trials skip the middle levels. On a test going
  from 19% to 33% failures at 0.5 CPU, this plus slowdown scoring and more batches took the chance
  of labelling it timing from about 30% to about 80% in simulation. Tests with no sign of any
  effect are dropped early, which can only make false timing or network labels rarer.
- **Work is shared between victims.** One test that leaves state behind often breaks a whole
  class. On webssh, 27 tests failed together, and checking each one separately reran the same seed
  135 times and found the same polluter 27 times. Replays now go per failing seed instead of per
  test, and the polluter search tries polluters it already found before doing a binary search.
  That cut the run's test executions by 68% with the same results.
- **Honest "stable".** Passing every one of n runs only rules out failure rates above roughly 3/n,
  so every report says what rate the run could have caught.
- **Toxiproxy per trial, only when needed.** A shared proxy would let one trial's latency affect
  another's. Trials with no added latency connect to the service directly, one container fewer.
- **Migrations.** Old db files upgrade on open (`PRAGMA user_version`) instead of breaking.

## Limitations

- pytest only.
- Catching rare flakiness takes a lot of full-suite trials: 59 for a test that fails 5% of the time,
  299 for 1% (at 95% probability).
- Network faults need the tests to read the service address from config (like `DATABASE_URL`).
- On laptops with mixed fast and slow cores and no fan, parallel trials still affect each other's
  speed even when pinned, so CPU results compare levels within a run rather than exact timings.
- The polluter search assumes one test is enough to break the victim. If it takes several together,
  it says so instead of guessing.
- A test with two separate problems, like an order dependency and a timing race, is reported as
  order-dependent, with the timing trend treated as chance.
- Tests with no sign of a CPU or network effect after 2 focus batches stop being focused. That saves
  a lot on tests that are flaky for some other reason, but on unlucky data it occasionally gives up
  on a small real effect, which then gets reported as nondeterministic.
- Tests that fail when run twice in the same process (NIO in the IDoFT dataset) aren't detected,
  since each trial runs each test once.

## Trying it on the benchmarks

flaky-zoo:

```
cd flaky-zoo && docker build -t flaky-zoo . && cd ..
./flaky-zoo/run.sh
./flakehunt score <run id>
```

webssh:

```
docker build -t flakehunt-webssh benchmarks/webssh
./benchmarks/webssh/run.sh
./flakehunt score --expected benchmarks/webssh/expected.json <run id>
```

[`benchmarks/idoft_expected.py`](benchmarks/idoft_expected.py) makes an `expected.json` for any
project from IDoFT's `py-data.csv`.

## Tests

```
go test -race ./...
```

CI runs gofmt, `go vet`, the tests with the race detector, and a build on every push to main
and every pull request. The unit tests don't need Docker, so they run on a plain Linux runner.

## Status

Done: detection, cause analysis, hang detection, polluter search, network faults, adaptive
sampling, markdown reports, scoring, CI, and a first real project (webssh) scored against IDoFT.

Next: more real projects, including one with timing or network flakiness, and releases.

Upstream: [huashengdun/webssh#423](https://github.com/huashengdun/webssh/pull/423) fixes the
order-dependent tests flakehunt found in webssh.