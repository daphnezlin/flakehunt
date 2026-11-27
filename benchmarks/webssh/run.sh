#!/bin/sh
# Runs flakehunt on webssh. Run from the repo root: ./benchmarks/webssh/run.sh
# (extra flags go to flakehunt run). The tests start their own ssh and web
# servers inside the container, so there's no --service-image.

./flakehunt run --image flakehunt-webssh --test-timeout 30s "$@"
