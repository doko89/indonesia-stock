#!/bin/bash
set -e
export DEBIAN_FRONTEND=noninteractive
echo "== Redis =="
if ! command -v redis-server >/dev/null 2>&1; then
  sudo apt update && sudo apt install -y redis-server
  sudo systemctl enable --now redis-server || sudo systemctl enable --now redis
fi
redis-cli ping || echo "redis belum ready"

echo "== PostgreSQL + TimescaleDB (arm64) =="
if ! dpkg -l | grep -q timescaledb; then
  sudo apt install -y gnupg postgresql-common apt-transport-https lsb-release wget
  # TimescaleDB repo (fail on redirect: pinned https URL, never downgrade to http)
  wget --max-redirect=0 -qO- https://packagecloud.io/timescale/timescaledb/gpgkey | gpg --dearmor | sudo tee /etc/apt/trusted.gpg.d/timescaledb.gpg >/dev/null
  echo "deb https://packagecloud.io/timescale/timescaledb/ubuntu/ $(lsb_release -c -s) main" | sudo tee /etc/apt/sources.list.d/timescaledb.list
  sudo apt update
  # Install PG15 + timescaledb
  sudo apt install -y timescaledb-2-postgresql-15 postgresql-15 || sudo apt install -y timescaledb-2-postgresql-14 postgresql-14
  sudo timescaledb-tune --quiet --yes || true
  sudo systemctl restart postgresql || true
fi

echo "== Buat DB indostock =="
# password comes from env (or .env), never hardcoded (Sonar: secret in code)
if [ -z "${POSTGRES_PASSWORD:-}" ]; then
  echo "ERROR: export POSTGRES_PASSWORD first (or source .env)"; exit 1
fi
sudo -u postgres psql -tc "SELECT 1 FROM pg_database WHERE datname='indostock'" | grep -q 1 || sudo -u postgres createdb indostock
sudo -u postgres psql -d indostock -c "CREATE EXTENSION IF NOT EXISTS timescaledb;"
sudo -u postgres psql -v pw="$POSTGRES_PASSWORD" -c "ALTER USER indostock WITH PASSWORD :'pw';" 2>/dev/null || sudo -u postgres psql -v pw="$POSTGRES_PASSWORD" -c "CREATE USER indostock WITH PASSWORD :'pw' SUPERUSER;"
sudo -u postgres psql -c "GRANT ALL PRIVILEGES ON DATABASE indostock TO indostock;"

echo "== Enable TimescaleDB =="
sudo -u postgres psql -d indostock -c "CREATE TABLE IF NOT EXISTS running_trades (symbol TEXT, time TIMESTAMPTZ, open DOUBLE PRECISION, high DOUBLE PRECISION, low DOUBLE PRECISION, close DOUBLE PRECISION, volume BIGINT); SELECT create_hypertable('running_trades','time', if_not_exists=>TRUE);"

echo "TimescaleDB & Redis ready"
