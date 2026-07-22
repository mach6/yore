#!/bin/sh
# Seed the client's yore config from the compose environment so `yore setup`
# needs no prompts (server URL and integration mode are pre-filled), then run
# the container command.
#
# No credential is written here: config.toml holds no secrets. Enrollment is
# authorized by a single-use ticket, which `yore setup` picks up from
# $YORE_TICKET (set in compose) or from --ticket.
set -e
mkdir -p /root/.config/yore
cat > /root/.config/yore/config.toml <<EOF
server_url = "${YORE_SERVER:-http://server:8080}"
integration = "${YORE_INTEGRATION:-takeover}"
EOF
chmod 600 /root/.config/yore/config.toml
exec "$@"
