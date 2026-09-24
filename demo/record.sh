#!/usr/bin/env bash
# Records the README's demo GIF with asciinema and renders it with agg. Needs
# ANTHROPIC_API_KEY, like demo/run.sh. The GIF cuts stand-up and teardown but keeps the
# "Ready in" line, and caps idle time at 2s, which each agent's "answered in" line discloses.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Recording demo/run.sh, about two minutes..."
asciinema rec --quiet --overwrite --headless --window-size 120x40 \
  --command ./demo/run.sh demo/demo.cast

# asciicast v3 stores each event's delay since the previous one, so dropping events
# only needs the first kept delay zeroed. Each beat heading is held long enough to read.
trimmed=$(mktemp)
awk 'NR == 1 { print; next }
  /Ready in/ { on = 1; sub(/^\[[0-9.]+/, "[0") }
  /Tear down/ { on = 0 }
  on && /\[[0-9]+s\] / { sub(/^\[[0-9.]+/, "[2.5") }
  on' demo/demo.cast > "$trimmed"
agg --idle-time-limit 2 --last-frame-duration 10 "$trimmed" demo/demo.gif
rm "$trimmed"
echo "Wrote demo/demo.gif"
