# flaky-zoo

Tests that are flaky on purpose, each for a known reason. Used to check that
flakehunt finds them and gets the cause right. The expected answers are in
[`expected.json`](expected.json), which `flakehunt score` reads.

| Test | Cause | Expected verdict |
|---|---|---|
| `test_race.py::test_report_is_ready` | sleeps a fixed time instead of waiting for a thread | timing |
| `test_time_boundary.py::test_report_timestamp` | assumes work starts and ends in the same second, slow cpu makes it worse | timing |
| `test_network.py::test_fetch_status` | 100ms timeout talking to a service | network |
| `test_deadlock.py::test_transfer_between_accounts` | two threads take two locks in opposite order | hang |
| `test_order.py::test_no_users_initially` | global state left behind by `test_create_users` | order-dependent |
| `test_random_data.py::test_price_per_percent` | divides by a random number that's sometimes 0 | random data |
| everything else | not flaky, control group | stable |

`test_network.py` only runs when `ZOO_API_URL` is set, which `run.sh` does by starting a
service. `test_deadlock.py` needs a per-test timeout (`--test-timeout`), otherwise a deadlock
stalls the whole trial.

Not covered: bugs that depend on the date or time zone (month ends, DST). Those don't flake
within a run, they flake between days, so catching them needs control over the clock.

## Setup

```
docker build -t flaky-zoo .
docker run --rm flaky-zoo python calibrate_race.py
```

The race test's wait depends on cpu speed. Use the printed value with `RACE_WAIT`, e.g.
`RACE_WAIT=0.045 ./flaky-zoo/run.sh` (0.045 is the default).

## Run

From the repo root:

```
./flaky-zoo/run.sh
./flakehunt score <run id>
```
