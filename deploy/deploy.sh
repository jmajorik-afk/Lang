#!/usr/bin/env bash
# Deploy (or update) langekko on a Debian/Ubuntu server over SSH.
#
#   deploy/deploy.sh root@1.2.3.4
#
# Run from the repo root on your machine. Idempotent — re-run to ship a new
# version. What it does:
#   1. installs build deps + Go on the server (only if missing)
#   2. rsyncs this source tree there (no GitHub access needed on the server)
#   3. builds on the server (SQLite needs cgo, so we don't cross-compile)
#   4. installs the systemd unit and (re)starts the service
# Your local .env is copied only if the server has none yet — it is never
# printed. Put a FRESH Telegram token in it before the first deploy.
set -euo pipefail

SERVER="${1:?usage: deploy/deploy.sh user@host}"
APP_DIR=/opt/langekko
# ControlMaster: the first connection stays open and every later ssh/scp/rsync
# reuses it — with password auth you type the password once, not five times.
SSH_OPTS="-o StrictHostKeyChecking=accept-new -o ControlMaster=auto -o ControlPath=$HOME/.ssh/cm-%C -o ControlPersist=10m"
SSH="ssh $SSH_OPTS $SERVER"

cd "$(dirname "$0")/.."

echo "==> [1/4] server prerequisites"
$SSH bash -s <<'REMOTE'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
command -v apt-get >/dev/null || { echo "this script expects a Debian/Ubuntu server"; exit 1; }
apt-get update -qq
apt-get install -y -qq git build-essential rsync ca-certificates curl >/dev/null

# Go: install the current stable release unless a suitable one is present
need_go=1
if command -v /usr/local/go/bin/go >/dev/null; then
  have=$(/usr/local/go/bin/go version | awk '{print $3}')
  case "$have" in go1.2[3-9]*|go1.[3-9]*|go[2-9]*) need_go=0 ;; esac
fi
if [ "$need_go" = 1 ]; then
  ver="${GO_VERSION:-$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)}"
  echo "installing $ver"
  curl -fsSL "https://go.dev/dl/${ver}.linux-$(dpkg --print-architecture).tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
fi

id -u langekko >/dev/null 2>&1 || useradd --system --home /opt/langekko --shell /usr/sbin/nologin langekko
mkdir -p /opt/langekko/src /opt/langekko/backups
REMOTE

echo "==> [2/4] syncing source"
rsync -az --delete -e "ssh $SSH_OPTS" \
  --exclude .git --exclude '.env*' --exclude 'languagebot.db*' --exclude backups/ --exclude '*.backup-*' \
  ./ "$SERVER:$APP_DIR/src/"

if [ -f .env ]; then
  if $SSH test -f "$APP_DIR/.env"; then
    echo "    server already has .env — leaving it alone (edit it there if the token changed)"
  else
    echo "    copying local .env (first deploy)"
    scp -q $SSH_OPTS .env "$SERVER:$APP_DIR/.env"
  fi
else
  echo "    no local .env — create $APP_DIR/.env on the server before starting"
fi

echo "==> [3/4] building on the server"
$SSH bash -s <<'REMOTE'
set -euo pipefail
export PATH=/usr/local/go/bin:$PATH
cd /opt/langekko/src
CGO_ENABLED=1 go build -o /opt/langekko/langekko.new ./cmd
mv /opt/langekko/langekko.new /opt/langekko/langekko
rm -rf /opt/langekko/scripts /opt/langekko/templates
cp -r scripts templates /opt/langekko/
chown -R langekko:langekko /opt/langekko
chmod 600 /opt/langekko/.env 2>/dev/null || true
REMOTE

echo "==> [4/4] installing service"
scp -q $SSH_OPTS deploy/langekko.service "$SERVER:/etc/systemd/system/langekko.service"
$SSH bash -s <<'REMOTE'
set -euo pipefail
systemctl daemon-reload
systemctl enable --now langekko >/dev/null
systemctl restart langekko
sleep 2
systemctl --no-pager --lines=8 status langekko || true
REMOTE

echo
echo "done. logs:  ssh $SERVER journalctl -u langekko -f"
