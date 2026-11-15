#!/usr/bin/env bash
# Steps one k6 scenario through increasing arrival rates against the Compose
# stack and collects one markdown row per endpoint per rate.
#
#   loadtest/run.sh flow 50 100 200        # needs API_KEY, or creates a merchant
#   DURATION=30s loadtest/run.sh hot 25 50 100
#
# Raw k6 summaries go to loadtest/results/<scenario>-<rate>.json (gitignored).
set -euo pipefail
cd "$(dirname "$0")/.."
scenario=$1; shift
mkdir -p loadtest/results
if [[ -z "${API_KEY:-}" ]]; then
  API_KEY=$(docker compose run --rm -T serve merchant create -name "load-$scenario" | sed -n 's/.*"api_key": "\(.*\)".*/\1/p')
fi
export API_KEY DURATION="${DURATION:-30s}"
for rate in "$@"; do
  RATE=$rate RUN_ID="$scenario-$rate-$(date +%s)" k6 run --quiet --no-color "loadtest/$scenario.js" >/dev/null 2>loadtest/results/$scenario-$rate.log || true
  cat "loadtest/results/$scenario-$rate.row.md"
done
