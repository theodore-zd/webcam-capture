#!/usr/bin/env bash
#
# webcam-deploy.sh — deploy the webcam-capture service to a Linux box via git.
#
# Pushes the app's main branch to a bare repo on the box, checks it out to a
# worktree, builds the image and (re)starts the container. No sudo needed —
# requires docker usable by the ssh user (see scripts/box-setup.sh).
# Idempotent — re-push and rebuild on every run.
#
# Usage:
#   ./scripts/webcam-deploy.sh [ssh-host]           # default: pandoria
#   WEBCAM_TZ=America/Toronto ./scripts/webcam-deploy.sh mybox
#
set -euo pipefail

HOST="${1:-pandoria}"
WEBCAM_TZ="${WEBCAM_TZ:-America/Toronto}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="${APP_DIR:-$SCRIPT_DIR/../webcam-capture}"
REMOTE_DIR="${REMOTE_DIR:-webcam-capture}"       # worktree dir on the box (~/)
REMOTE_REPO="${REMOTE_REPO:-webcam-capture.git}" # bare repo on the box (~/)

log() { printf '\033[1;36m[deploy]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[deploy] FAIL:\033[0m %s\n' "$*" >&2; exit 1; }

[[ -d "$APP_DIR/.git" ]] || die "not a git repo: $APP_DIR (deploy pushes a branch)"
ssh -o BatchMode=yes "$HOST" 'command -v git >/dev/null' \
  || die "git missing on $HOST — run scripts/box-setup.sh first (installs git)"

# ---------------------------------------------------------------- preflight
log "preflight: $HOST"
ssh -o BatchMode=yes "$HOST" 'set -e; test -c /dev/video0 && echo "webcam: /dev/video0 OK"' \
  || die "no /dev/video0 on $HOST"
ssh -o BatchMode=yes "$HOST" 'docker version --format "docker: {{.Server.Version}} (server reachable)"' \
  || die "docker not usable by this user on $HOST — run scripts/box-setup.sh first (and re-login for group membership)"

# --------------------------------------------------- push branch to the box
BRANCH="$(git -C "$APP_DIR" rev-parse --abbrev-ref HEAD)"
log "pushing $APP_DIR ($BRANCH) -> $HOST:~/$REMOTE_REPO"
ssh -o BatchMode=yes "$HOST" "git init --bare -b main ~/$REMOTE_REPO" >/dev/null
git -C "$APP_DIR" push --force "$HOST:$REMOTE_REPO" "$BRANCH:main" 2>&1 \
  | grep -vE '^(Enumerating|Counting|Compressing|Writing|Total)' || true

# ------------------------------------------- checkout, build, start, verify
log "checking out worktree, building image, starting container"
ssh -o BatchMode=yes "$HOST" "bash -s" <<EOS
set -e
git --git-dir="\$HOME/$REMOTE_REPO" --work-tree="\$HOME/$REMOTE_DIR" checkout -f main
cd ~/$REMOTE_DIR
export WEBCAM_TZ='$WEBCAM_TZ'
docker compose build 2>&1 | tail -n 3
docker compose up -d
EOS

log "waiting ~30s for first capture..."
sleep 30
ssh -o BatchMode=yes "$HOST" bash -s <<'EOS'
set -e
cd ~/webcam-capture
echo "--- container:"
docker compose ps --format 'table {{.Name}}\t{{.Status}}'
echo "--- photos on disk:"
find data -name '*.jpeg' | head -n 3 || true
echo "--- total: $(find data -name '*.jpeg' | wc -l)"
echo "--- last logs:"
docker compose logs --tail 3 2>&1 | grep -v '^$' || true
EOS
log "deploy complete. Photos: ~/$REMOTE_DIR/data/<year>/<month>/<day>/ on the box; auto-zip on day rollover."
