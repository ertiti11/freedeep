#!/usr/bin/env sh
set -eu

root=$(git rev-parse --show-toplevel)
git -C "$root" config core.hooksPath .githooks

gitleaks_bin=$(command -v gitleaks || true)
if [ -z "$gitleaks_bin" ]; then
  echo "Gitleaks is required. Install it from https://github.com/gitleaks/gitleaks/releases and rerun this script." >&2
  exit 1
fi

git -C "$root" config freedeep.gitleaksPath "$gitleaks_bin"
chmod +x "$root/.githooks/pre-commit" "$root/.githooks/pre-push"

echo "freedeep hooks installed."
echo "Every commit and push will now be checked automatically."
