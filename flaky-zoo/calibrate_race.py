"""Find a FLAKY_ZOO_RACE_WAIT value that makes test_race.py flaky on this machine.

It runs the real test under pytest at increasing wait values and picks the
first one where the test fails only occasionally (target: about 5-20% of runs).
Throttled CPUs then push the failure rate much higher.

Run it in the same environment flakehunt uses:

    docker run --rm flaky-zoo python calibrate_race.py
"""

import os
import subprocess
import sys

RUNS_PER_CANDIDATE = 20
TARGET_MAX_FAILURE_RATE = 0.20
START_WAIT = 0.010   # seconds
STEP = 1.15          # multiply the wait by this each round
MAX_WAIT = 0.200


def failure_rate(wait):
    env = {**os.environ, "FLAKY_ZOO_RACE_WAIT": f"{wait:.4f}"}
    failures = 0
    for _ in range(RUNS_PER_CANDIDATE):
        result = subprocess.run(
            [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider",
             "-p", "no:randomly", "tests/test_race.py"],
            env=env, capture_output=True,
        )
        failures += result.returncode != 0
    return failures / RUNS_PER_CANDIDATE


wait = START_WAIT
while wait <= MAX_WAIT:
    rate = failure_rate(wait)
    print(f"wait {wait * 1000:6.1f} ms -> fails {rate:.0%}")
    if rate <= TARGET_MAX_FAILURE_RATE:
        print(f"\nFLAKY_ZOO_RACE_WAIT={wait:.4f}")
        break
    wait *= STEP
else:
    print("No suitable wait found; increase MAX_WAIT.")
