"""Flaky cause: RANDOM TEST DATA hitting an edge case.

The test generates a random discount between 0 and 20. The code under test
divides by the discount, so it crashes when the random value is exactly 0
(about 1 run in 21, roughly 5%).

pytest-randomly reseeds `random` from --randomly-seed, so any failing run is
reproducible from its seed.

Expected flakehunt diagnosis: depends on seed only; independent of CPU and order.
"""

import random


def price_per_percent(price, discount_percent):
    return price / discount_percent  # BUG: no guard for 0


def test_price_per_percent():
    discount = random.randint(0, 20)
    assert price_per_percent(100, discount) > 0
