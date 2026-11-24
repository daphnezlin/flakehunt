"""Flaky cause: ORDER DEPENDENCE (shared state).

test_create_users (the "polluter") adds users to a module-level list and never
cleans up. test_no_users_initially (the "victim") assumes the list is empty.

The victim passes if it runs first and fails if it runs after the polluter.
With pytest-randomly shuffling test order, it fails about half the time.

Expected flakehunt diagnosis: order-dependent; polluter = test_create_users.
"""

USERS = []  # BUG: global state shared across tests


def create_user(name):
    USERS.append(name)


def test_create_users():
    create_user("alice")
    create_user("bob")
    assert len(USERS) >= 2


def test_no_users_initially():
    assert USERS == []


def test_unrelated_math():
    # A bystander test, so the polluter search has to rule something out.
    assert sum(range(10)) == 45
