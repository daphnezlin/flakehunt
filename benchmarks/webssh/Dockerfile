# webssh at the commit IDoFT found its flaky tests in.
# Build from the repo root: docker build -t flakehunt-webssh benchmarks/webssh
FROM python:3.8-slim

# every dependency has an arm64 and amd64 wheel, so no compiler or apt packages
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt

WORKDIR /app
ADD https://github.com/huashengdun/webssh/archive/51d527fe75a62aed239126a6697749a30baecb30.tar.gz /tmp/webssh.tar.gz
RUN tar xzf /tmp/webssh.tar.gz --strip-components=1 && rm /tmp/webssh.tar.gz

# flakehunt supplies the full pytest command per trial.
CMD ["pytest", "-q"]
