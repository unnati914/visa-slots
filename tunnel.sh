#!/bin/sh
# Free public link to the dashboard running on this Mac (Cloudflare quick tunnel).
# Works while this window stays open. Close it to take the dashboard offline.
cd "$(dirname "$0")"
curl -s -o /dev/null http://localhost:8080/ || (./bin/server >> server.log 2>&1 &)
echo "Starting tunnel... your public link (https://....trycloudflare.com) appears below in a few seconds."
caffeinate -is cloudflared tunnel --url http://localhost:8080
