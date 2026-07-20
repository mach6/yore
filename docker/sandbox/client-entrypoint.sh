#!/bin/sh
# Seed the client's yore config from the compose environment so `yore setup`
# needs no prompts (server URL, token, and integration mode are pre-filled),
# then run the container command.
set -e
mkdir -p /root/.config/yore
cat > /root/.config/yore/config.json <<EOF
{
  "server_url": "${YORE_SERVER:-http://server:8080}",
  "token": "${YORE_TOKEN:-sandbox-token}",
  "integration": "${YORE_INTEGRATION:-takeover}"
}
EOF
chmod 600 /root/.config/yore/config.json
exec "$@"
