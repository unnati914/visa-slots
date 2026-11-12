#!/bin/bash
# Deploy to the Oracle free VM. Run from your Mac:
#   ./deploy/deploy.sh <server-ip> [telegram-bot-token]
# The first run installs everything (2-3 min). Later runs just update the app (~10 s).
set -euo pipefail
cd "$(dirname "$0")/.."
IP="${1:?usage: ./deploy/deploy.sh <server-ip> [telegram-bot-token]}"
TOKEN="${2:-}"
KEY="$HOME/.ssh/visa_slots_oracle"
SSH="ssh -i $KEY -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 ubuntu@$IP"
DOMAIN="$(echo "$IP" | tr . -).sslip.io"   # free domain that points at the IP, so HTTPS works

ARCH=$($SSH uname -m)
case "$ARCH" in aarch64) GOARCH=arm64 ;; x86_64) GOARCH=amd64 ;; *) echo "unknown arch $ARCH"; exit 1 ;; esac
echo "→ building for linux/$GOARCH"
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -trimpath -ldflags="-s -w" -o bin/visa-slots-linux .
echo "→ uploading"
scp -i "$KEY" -q bin/visa-slots-linux ubuntu@"$IP":/tmp/visa-slots
scp -i "$KEY" -q deploy/server-setup.sh ubuntu@"$IP":/tmp/server-setup.sh
echo "→ installing on server"
$SSH "bash /tmp/server-setup.sh '$DOMAIN' '$TOKEN'"
echo "→ checking https://$DOMAIN"
for i in $(seq 1 30); do
  if curl -fs "https://$DOMAIN/api/config" >/dev/null; then echo "✅ Live: https://$DOMAIN"; exit 0; fi
  sleep 3
done
echo "⚠️  App is running but HTTPS isn't reachable yet. Usually the Oracle Security List is missing ports 80/443 (see deploy/ORACLE.md)."
exit 1
