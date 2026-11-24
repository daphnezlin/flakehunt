"""Control group: tests that are NOT flaky.

flakehunt must never flag these. They catch false positives.
"""


def test_addition():
    assert 2 + 2 == 4


def test_string_upper():
    assert "flaky".upper() == "FLAKY"


def test_sorted():
    assert sorted([3, 1, 2]) == [1, 2, 3]
