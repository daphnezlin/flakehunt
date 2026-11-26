# Hang: two threads take the same two locks in opposite order. If they each
# grab their first lock before either grabs its second, both wait forever.
# Whether that happens depends on thread timing, so it only hangs sometimes.
# flakehunt sets a per-test timeout so the hang shows up as a failure.

import random
import threading
import time

_rng = random.SystemRandom()  # not reseeded by pytest-randomly, timing isn't seed driven


def test_transfer_between_accounts():
    a, b = threading.Lock(), threading.Lock()

    def one():
        time.sleep(_rng.uniform(0, 0.01))
        with a:
            time.sleep(0.001)
            with b:
                pass

    def two():
        time.sleep(_rng.uniform(0, 0.01))
        with b:
            time.sleep(0.001)
            with a:  # bug: takes the locks in the opposite order to one()
                pass

    threads = [threading.Thread(target=one, daemon=True), threading.Thread(target=two, daemon=True)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
