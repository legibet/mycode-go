#!/usr/bin/env sh
set -eu

REPO_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
WAILS_VERSION="v2.12.0"
WAILS_BIN="$(GOWORK=off go env GOPATH)/bin/wails"

if [ ! -x "$WAILS_BIN" ]; then
  echo "Wails CLI not found at $WAILS_BIN" >&2
  echo "Install with: GOWORK=off go install github.com/wailsapp/wails/v2/cmd/wails@${WAILS_VERSION}" >&2
  exit 1
fi

git -C "$REPO_DIR" submodule update --init --recursive
pnpm --dir "$REPO_DIR/web" install --frozen-lockfile

cd "$REPO_DIR"
GOWORK=off "$WAILS_BIN" build -clean "$@"

APP_PATH="$REPO_DIR/build/bin/mycode.app"
if [ -d "$APP_PATH" ]; then
  codesign --sign - --deep --force "$APP_PATH" >/dev/null 2>&1 || true
  xattr -cr "$APP_PATH" 2>/dev/null || true
  echo "Built: $APP_PATH"
fi
