#!/usr/bin/env sh
set -eu

REPO_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TARGET_DIR="$REPO_DIR/frontend/dist"

git -C "$REPO_DIR" submodule update --init --recursive
pnpm --dir "$REPO_DIR/web" typecheck
pnpm --dir "$REPO_DIR/web" build

mkdir -p "$TARGET_DIR"
find "$TARGET_DIR" -mindepth 1 -maxdepth 1 ! -name '.gitkeep' -exec rm -rf {} +
cp -R "$REPO_DIR/web/dist/." "$TARGET_DIR/"
