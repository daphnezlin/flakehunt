# Network timeout: 100ms is fine on a fast connection but too tight once the
# service is slow to respond. Fails more as latency goes up.
# Needs ZOO_API_URL, which flakehunt sets when run with --service-image.

import os
import urllib.request

import pytest

API = os.environ.get("ZOO_API_URL")


@pytest.mark.skipif(not API, reason="ZOO_API_URL not set")
def test_fetch_status():
    with urllib.request.urlopen(API + "/", timeout=0.1) as resp:  # bug: timeout too tight
        assert resp.status == 200
