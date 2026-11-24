#!/bin/sh
# Runs flakehunt on the zoo, with a service for the network test.
# Run from the repo root: ./flaky-zoo/run.sh --runs 135
# RACE_WAIT overrides the race test's wait (default 0.045).

./flakehunt run --image flaky-zoo \
  --env FLAKY_ZOO_RACE_WAIT=${RACE_WAIT:-0.045} \
  --service-image flaky-zoo \
  --service-cmd "python -m http.server 8000" \
  --service-port 8000 \
  --service-health "python -c 'import urllib.request; urllib.request.urlopen(\"http://localhost:8000\", timeout=1)'" \
  --service-env 'ZOO_API_URL=http://{addr}' \
  "$@"
