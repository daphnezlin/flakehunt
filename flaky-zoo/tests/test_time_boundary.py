# Time boundary: the test assumes the report starts and finishes in the same
# second. It fails whenever a second ticks over while it's running, so the
# longer the work takes (slower cpu), the more often it fails.

import time


def build_report():
    total = 0
    for i in range(2_000_000):
        total += i
    return total


def test_report_timestamp():
    started = time.time()
    build_report()
    finished = time.time()
    assert int(started) == int(finished)  # bug: should compare against a tolerance, not a second boundary
