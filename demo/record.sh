#!/usr/bin/env bash
# Records the README's demo GIF with asciinema and renders it with agg.
# Real time throughout, so the stopwatch on screen is the one a reader gets.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Recording demo/run.sh, about a minute..."
# PAUSE holds the recording on the final diff.
PAUSE=8 asciinema rec --quiet --overwrite --headless --window-size 120x40 \
  --command ./demo/run.sh demo/demo.cast
agg --idle-time-limit 60 --last-frame-duration 5 demo/demo.cast demo/demo.gif
echo "Wrote demo/demo.gif"
