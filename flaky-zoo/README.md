# flaky-zoo

Deliberately flaky tests with **known causes**, used to check that flakehunt
detects them and diagnoses the right cause.

| Test | Cause | Expected diagnosis |
|---|---|---|
| `test_race.py::test_report_is_ready` | Timing race (fixed sleep instead of waiting) | Fails more under CPU throttling |
| `test_order.py::test_no_users_initially` | Order dependence (global state) | Polluter: `test_create_users` |
| `test_random_data.py::test_price_per_percent` | Random data edge case | Depends on seed only |
| `test_stable.py::*` | None (control group) | Never flagged |

Planned additions: time/date, network timeout (via Toxiproxy), hang.

## Build

```
docker build -t flaky-zoo .
```

## Calibrate the race test

The race test's timing depends on CPU speed. Find the right wait for your machine:

```
docker run --rm flaky-zoo python calibrate_race.py
```

Pass the printed value to every run, e.g. `-e FLAKY_ZOO_RACE_WAIT=0.0201`.

## Run once by hand

```
docker run --rm -e FLAKY_ZOO_RACE_WAIT=0.0201 flaky-zoo pytest -q --randomly-seed=1234
```
