#!/bin/bash
# Runs ON the Oracle VM (deploy.sh copies it there). Safe to run again on every deploy.
# Installs the app as a service that restarts itself, plus Caddy for free HTTPS.
set -euo pipefail
DOMAIN="$1"
TOKEN="${2:-}"

# App user, data folder, binary
id visa >/dev/null 2>&1 || sudo useradd --system --home /var/lib/visa-slots --create-home --shell /usr/sbin/nologin visa
sudo install -m 755 /tmp/visa-slots /usr/local/bin/visa-slots

# Settings. Keep an existing Telegram token unless a new one is given.
OLD_TOKEN=$(sudo sed -n 's/^TELEGRAM_BOT_TOKEN=//p' /etc/visa-slots.env 2>/dev/null || true)
[ -z "$TOKEN" ] && TOKEN="$OLD_TOKEN"
sudo tee /etc/visa-slots.env >/dev/null <<ENV
SITE_URL=https://$DOMAIN
DATA_DIR=/var/lib/visa-slots/data
TELEGRAM_BOT_TOKEN=$TOKEN
ENV
sudo chmod 600 /etc/visa-slots.env

sudo tee /etc/systemd/system/visa-slots.service >/dev/null <<'UNIT'
[Unit]
Description=Visa Slots
After=network-online.target
Wants=network-online.target

[Service]
User=visa
EnvironmentFile=/etc/visa-slots.env
ExecStart=/usr/local/bin/visa-slots -addr 127.0.0.1:8080
Restart=always
RestartSec=2
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT

# Caddy: HTTPS certificate is fetched and renewed automatically.
if ! command -v caddy >/dev/null; then
  sudo apt-get update -qq
  sudo apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl gnupg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
  sudo apt-get update -qq && sudo apt-get install -y -qq caddy
fi
sudo tee /etc/caddy/Caddyfile >/dev/null <<CADDY
$DOMAIN {
	encode gzip
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
CADDY

# Oracle's Ubuntu image blocks web ports in its own firewall. Open 80 and 443.
for p in 80 443; do
  sudo iptables -C INPUT -p tcp --dport $p -j ACCEPT 2>/dev/null || sudo iptables -I INPUT 5 -p tcp --dport $p -j ACCEPT
done
command -v netfilter-persistent >/dev/null || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq iptables-persistent
sudo netfilter-persistent save >/dev/null 2>&1 || true

sudo systemctl daemon-reload
sudo systemctl enable --now visa-slots >/dev/null
sudo systemctl restart visa-slots
sudo systemctl enable caddy >/dev/null
sudo systemctl reload caddy || sudo systemctl restart caddy
sleep 2
systemctl is-active visa-slots caddy
