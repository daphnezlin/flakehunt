FROM python:3.12-slim

RUN pip install --no-cache-dir pytest==8.* pytest-randomly==3.*

WORKDIR /app
COPY tests/ tests/
COPY calibrate_race.py .

# flakehunt supplies the full pytest command per trial.
CMD ["pytest", "-q"]
