#!/usr/bin/env bash
# Opens the release PR for X.Y.Z. Merging it publishes the release: see release.yaml.
# It runs as you rather than in CI, because a PR opened by GITHUB_TOKEN triggers no
# checks, and main requires them.
set -euo pipefail

version=${1:?usage: scripts/release.sh X.Y.Z}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not X.Y.Z: $version" >&2; exit 1; }
cd "$(dirname "${BASH_SOURCE[0]}")/.."
grep -q '^## Unreleased' CHANGELOG.md || { echo "CHANGELOG.md has no Unreleased section" >&2; exit 1; }

git fetch -q origin main
git switch -c "release-$version" origin/main
sed -i.bak -E "s/^version: .*/version: $version/; s/^appVersion: .*/appVersion: \"$version\"/" charts/keepsake/Chart.yaml
sed -i.bak -E "s|(ghcr.io/[a-z0-9-]+/keepsake:)[0-9.]+|\1$version|" compose.yaml
sed -i.bak -E "s/--version [0-9.]+/--version $version/; s|(cmd/keepsake@v)[0-9.]+|\1$version|" README.md docs/operations.md
sed -i.bak "s/^## Unreleased$/## $version — $(date -u +%F)/" CHANGELOG.md
rm charts/keepsake/Chart.yaml.bak compose.yaml.bak README.md.bak docs/operations.md.bak CHANGELOG.md.bak

git commit -qam "Release $version"
git push -qu origin "release-$version"
gh pr create --base main --title "Release $version" --body "Merging this publishes v$version: the image, the chart, an SBOM and a GitHub Release."
