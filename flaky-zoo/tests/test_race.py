"""Flaky cause: TIMING RACE.

A background worker does a fixed amount of CPU work and then publishes a
result. The test waits a fixed amount of time instead of waiting for the
worker to finish. On a normal machine it usually passes; on a slow or
throttled CPU the worker loses the race and the test fails.

Expected flakehunt diagnosis: failure rate rises sharply under CPU throttling,
and is NOT explained by the random seed or test order.

Calibration: the right wait depends on CPU speed. Run
    docker run --rm flaky-zoo python calibrate_race.py
and set FLAKY_ZOO_RACE_WAIT to the value it prints.
"""

import os
import sys
import threading
import time

# Python only switches threads every 5 ms by default, which would hide the race
# behind scheduling delays. A short interval makes the timing comparison real.
sys.setswitchinterval(0.0005)

WORK_ITERATIONS = 600_000
WAIT_SECONDS = float(os.environ.get("FLAKY_ZOO_RACE_WAIT", "0.025"))

results = {}


def do_work():
    # Fixed amount of CPU work: its duration depends only on how fast the CPU is.
    total = 0
    for i in range(WORK_ITERATIONS):
        total += i
    return total


def _worker():
    results["report"] = do_work()


def test_report_is_ready():
    results.clear()
    threading.Thread(target=_worker, daemon=True).start()

    time.sleep(WAIT_SECONDS)  # BUG: should join the thread or wait on an Event

    assert "report" in results
