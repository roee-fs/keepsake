#!/usr/bin/env bash
# Records the README's demo GIF with asciinema and renders it with agg.
# The GIF cuts stand-up and teardown but keeps the "Ready in" line, so the cut is on screen.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Recording demo/run.sh, about a minute..."
asciinema rec --quiet --overwrite --headless --window-size 120x40 \
  --command ./demo/run.sh demo/demo.cast

# asciicast v3 stores each event's delay since the previous one, so dropping events
# only needs the first kept delay zeroed. The middle runs in about a second, so each
# beat and MCP call is held long enough to read. The on-screen stopwatch stays real.
trimmed=$(mktemp)
awk 'NR == 1 { print; next }
  /Ready in/ { on = 1; sub(/^\[[0-9.]+/, "[0") }
  /Walk away/ { on = 0 }
  on && /\[[0-9]+s\] / { sub(/^\[[0-9.]+/, "[2.5") }
  on && /\$ curl/ { sub(/^\[[0-9.]+/, "[0.8") }
  on' demo/demo.cast > "$trimmed"
agg --idle-time-limit 60 --last-frame-duration 10 "$trimmed" demo/demo.gif
rm "$trimmed"
echo "Wrote demo/demo.gif"
