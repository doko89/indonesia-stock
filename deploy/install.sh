#!/bin/bash
set -e
# Deploy indostock ke ubuntu@ai.nex.my.id:/apps/indostock (arm64, systemd, redis+timescaledb)
APP_DIR=/apps/indostock
BIN=bin/indostock-linux-arm64
HOST=ubuntu@ai.nex.my.id

echo "== Build arm64 (already built) =="
# GOOS=linux GOARCH=arm64 GOCACHE=/tmp/gocache go build -buildvcs=false -o bin/indostock-linux-arm64 ./cmd/indostock

echo "== Copy ke server (/usr/local/bin) =="
ssh $HOST "sudo mkdir -p $APP_DIR && sudo chown -R ubuntu:ubuntu $APP_DIR"
scp $BIN $HOST:/tmp/indostock-linux-arm64
ssh $HOST "sudo cp /tmp/indostock-linux-arm64 /usr/local/bin/indostock && sudo chmod +x /usr/local/bin/indostock && sudo ln -sf /usr/local/bin/indostock $APP_DIR/bin/indostock 2>/dev/null || sudo mkdir -p $APP_DIR/bin && sudo ln -sf /usr/local/bin/indostock $APP_DIR/bin/indostock; ls -lh /usr/local/bin/indostock"
scp deploy/indostock.service $HOST:/tmp/indostock.service
scp deploy/install-timescaledb.sh $HOST:/tmp/install-timescaledb.sh

echo "== Install Redis + TimescaleDB di server =="
ssh $HOST "chmod +x /tmp/install-timescaledb.sh && /tmp/install-timescaledb.sh"

echo "== Pasang systemd =="
ssh $HOST "sudo mv /tmp/indostock.service /etc/systemd/system/indostock.service && sudo systemctl daemon-reload && sudo systemctl enable --now indostock && sudo systemctl status indostock --no-pager | head -n 40"

echo "== Test =="
ssh $HOST "indostock --help | head -n 20"
ssh $HOST "indostock whale BBCA --mock --compact | head -c 300; echo"
ssh $HOST "curl -s http://localhost:18080/health 2>&1 | head -c 300; echo"
