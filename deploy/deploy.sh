#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

APP_DIR="/opt/telegram-gemini-bot"
DATA_DIR="/var/lib/telegram-gemini-bot"
SERVICE_NAME="telegram-gemini-bot"
APP_USER="telegrambot"
APP_GROUP="telegrambot"
REMOTE_CREDENTIALS_PATH="/etc/telegram-gemini-bot/service-account.json"

SSH_HOST=""
SSH_USER="${USER:-}"
SSH_PORT="22"
IDENTITY_FILE=""
ENV_FILE=""
SERVICE_FILE="$REPO_ROOT/deploy/telegram-gemini-bot.service"
CREDENTIALS_FILE=""
GOOS="linux"
GOARCH="$(go env GOARCH)"
SKIP_BUILD="false"

usage() {
	cat <<EOF
Usage:
  deploy/deploy.sh --host HOST --env-file FILE [options]

Required:
  --host HOST                 SSH host or user@host
  --env-file FILE             Local .env file to upload as $APP_DIR/.env

Options:
  --user USER                 SSH user (default: current user)
  --port PORT                 SSH port (default: 22)
  --identity FILE             SSH private key for ssh/scp
  --service-file FILE         Local systemd unit (default: deploy/telegram-gemini-bot.service)
  --credentials-file FILE     Optional service-account JSON to upload to $REMOTE_CREDENTIALS_PATH
  --goos OS                   Build target OS (default: linux)
  --goarch ARCH               Build target arch (default: current Go arch)
  --skip-build                Reuse existing local binary from bin/$SERVICE_NAME
  --help                      Show this help

Examples:
  deploy/deploy.sh --host my-vps --env-file .env
  deploy/deploy.sh --host root@homevpn --service-file deploy/telegram-gemini-bot-homevpn.service \
    --env-file .env.homevpn --credentials-file ~/service-account.json
EOF
}

fail() {
	printf 'Error: %s\n' "$1" >&2
	exit 1
}

require_file() {
	local path="$1"
	[[ -f "$path" ]] || fail "file not found: $path"
}

log() {
	printf '==> %s\n' "$1"
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--host)
			SSH_HOST="${2:-}"
			shift 2
			;;
		--user)
			SSH_USER="${2:-}"
			shift 2
			;;
		--port)
			SSH_PORT="${2:-}"
			shift 2
			;;
		--identity)
			IDENTITY_FILE="${2:-}"
			shift 2
			;;
		--env-file)
			ENV_FILE="${2:-}"
			shift 2
			;;
		--service-file)
			SERVICE_FILE="${2:-}"
			shift 2
			;;
		--credentials-file)
			CREDENTIALS_FILE="${2:-}"
			shift 2
			;;
		--goos)
			GOOS="${2:-}"
			shift 2
			;;
		--goarch)
			GOARCH="${2:-}"
			shift 2
			;;
		--skip-build)
			SKIP_BUILD="true"
			shift
			;;
		--help|-h)
			usage
			exit 0
			;;
		*)
			fail "unknown argument: $1"
			;;
	esac
done

[[ -n "$SSH_HOST" ]] || fail "--host is required"
[[ -n "$ENV_FILE" ]] || fail "--env-file is required"

require_file "$ENV_FILE"
require_file "$SERVICE_FILE"

if [[ -n "$IDENTITY_FILE" ]]; then
	require_file "$IDENTITY_FILE"
fi

if [[ -n "$CREDENTIALS_FILE" ]]; then
	require_file "$CREDENTIALS_FILE"
fi

BINARY_PATH="$REPO_ROOT/bin/$SERVICE_NAME"

if [[ "$SKIP_BUILD" != "true" ]]; then
	log "Building $SERVICE_NAME for $GOOS/$GOARCH"
	mkdir -p "$REPO_ROOT/bin"
	(
		cd "$REPO_ROOT"
		GOOS="$GOOS" GOARCH="$GOARCH" go build -o "$BINARY_PATH" .
	)
else
	log "Skipping build"
fi

require_file "$BINARY_PATH"

SSH_TARGET="$SSH_HOST"
if [[ "$SSH_HOST" != *@* && -n "$SSH_USER" ]]; then
	SSH_TARGET="$SSH_USER@$SSH_HOST"
fi

SSH_OPTS=(-p "$SSH_PORT" -o BatchMode=yes)
SCP_OPTS=(-P "$SSH_PORT" -o BatchMode=yes)
if [[ -n "$IDENTITY_FILE" ]]; then
	SSH_OPTS+=(-i "$IDENTITY_FILE")
	SCP_OPTS+=(-i "$IDENTITY_FILE")
fi

LOCAL_TMP="$(mktemp -d)"
REMOTE_TMP="/tmp/${SERVICE_NAME}-deploy-$(date +%s)-$$"
cleanup() {
	rm -rf "$LOCAL_TMP"
	ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "rm -rf '$REMOTE_TMP'" >/dev/null 2>&1 || true
}
trap cleanup EXIT

cp "$BINARY_PATH" "$LOCAL_TMP/$SERVICE_NAME"
cp "$ENV_FILE" "$LOCAL_TMP/.env"
cp "$SERVICE_FILE" "$LOCAL_TMP/${SERVICE_NAME}.service"
if [[ -n "$CREDENTIALS_FILE" ]]; then
	cp "$CREDENTIALS_FILE" "$LOCAL_TMP/service-account.json"
fi

log "Checking SSH connectivity"
	ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "true"

log "Uploading release bundle to $SSH_TARGET"
	ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "mkdir -p '$REMOTE_TMP'"
	scp "${SCP_OPTS[@]}" "$LOCAL_TMP/$SERVICE_NAME" "$LOCAL_TMP/.env" "$LOCAL_TMP/${SERVICE_NAME}.service" "$SSH_TARGET:$REMOTE_TMP/"
if [[ -n "$CREDENTIALS_FILE" ]]; then
	scp "${SCP_OPTS[@]}" "$LOCAL_TMP/service-account.json" "$SSH_TARGET:$REMOTE_TMP/"
fi

log "Installing release on remote host"
	ssh "${SSH_OPTS[@]}" "$SSH_TARGET" bash -s -- \
		"$REMOTE_TMP" \
		"$APP_DIR" \
		"$DATA_DIR" \
		"$SERVICE_NAME" \
		"$APP_USER" \
		"$APP_GROUP" \
		"$REMOTE_CREDENTIALS_PATH" \
		"$( [[ -n "$CREDENTIALS_FILE" ]] && printf 'true' || printf 'false' )" <<'EOF'
set -euo pipefail

REMOTE_TMP="$1"
APP_DIR="$2"
DATA_DIR="$3"
SERVICE_NAME="$4"
APP_USER="$5"
APP_GROUP="$6"
REMOTE_CREDENTIALS_PATH="$7"
UPLOAD_CREDENTIALS="$8"
SERVICE_UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"

if ! getent group "$APP_GROUP" >/dev/null 2>&1; then
	sudo groupadd --system "$APP_GROUP"
fi

if ! id -u "$APP_USER" >/dev/null 2>&1; then
	NOLOGIN_BIN="$(command -v nologin || true)"
	if [[ -z "$NOLOGIN_BIN" ]]; then
		NOLOGIN_BIN="/usr/sbin/nologin"
	fi
	sudo useradd --system --gid "$APP_GROUP" --home-dir "$APP_DIR" --shell "$NOLOGIN_BIN" "$APP_USER"
fi

sudo install -d -o "$APP_USER" -g "$APP_GROUP" -m 755 "$APP_DIR"
sudo install -d -o "$APP_USER" -g "$APP_GROUP" -m 755 "$DATA_DIR"
sudo install -d -o root -g root -m 755 /etc/systemd/system

sudo install -o "$APP_USER" -g "$APP_GROUP" -m 755 "$REMOTE_TMP/$SERVICE_NAME" "$APP_DIR/$SERVICE_NAME"
sudo install -o "$APP_USER" -g "$APP_GROUP" -m 640 "$REMOTE_TMP/.env" "$APP_DIR/.env"
sudo install -o root -g root -m 644 "$REMOTE_TMP/${SERVICE_NAME}.service" "$SERVICE_UNIT_PATH"

if [[ "$UPLOAD_CREDENTIALS" == "true" ]]; then
	REMOTE_CREDENTIALS_DIR="$(dirname "$REMOTE_CREDENTIALS_PATH")"
	sudo install -d -o root -g root -m 755 "$REMOTE_CREDENTIALS_DIR"
	sudo install -o root -g "$APP_GROUP" -m 640 "$REMOTE_TMP/service-account.json" "$REMOTE_CREDENTIALS_PATH"
fi

sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME"
sudo systemctl restart "$SERVICE_NAME"
sudo systemctl --no-pager --full status "$SERVICE_NAME"
EOF

log "Deploy finished"
printf 'Host: %s\nService: %s\n' "$SSH_TARGET" "$SERVICE_NAME"
