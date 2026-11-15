#!/usr/bin/env bash
# The scripted terminal demo behind docs/demo.gif. Re-record with:
#   asciinema rec --headless -i 2 --window-size 118x36 -c scripts/demo.sh docs/demo.cast
#   agg --theme monokai --font-size 14 docs/demo.cast docs/demo.gif
# Needs Docker, jq, Go, and a built Compose stack (docker compose build).
set -euo pipefail
cd "$(dirname "$0")/.."

say() { printf '\n\033[1;36m# %s\033[0m\n' "$*"; sleep 1.2; }
run() { printf '\033[1;32m$\033[0m %s\n' "$*"; sleep 0.6; eval "$@"; }

say "Ledgerline: a payments API and double-entry ledger in Go"
run "docker compose up -d --wait serve dispatch 2>/dev/null | tail -1"

say "A merchant and its API key (stored only as a SHA-256 hash)"
KEY=$(docker compose run --rm -T serve merchant create -name "Demo Shop" 2>/dev/null | jq -r .api_key)
printf 'KEY=%s…\n' "${KEY:0:12}"
H=(-H "Authorization: Bearer $KEY" -H "Content-Type: application/json")
api() { curl -s "${H[@]}" "$@"; }

say "Create, authorize and capture a \$25.00 payment; every POST carries an Idempotency-Key"
PID=$(api -X POST localhost:8080/v1/payments -H "Idempotency-Key: order-1001" \
  -d '{"amount": 2500, "currency": "USD", "customer_id": "cus_42", "card": {"fingerprint": "fp_visa_4242", "country": "US"}, "ip_country": "US"}' | jq -r .id)
echo "created $PID"
run "api -X POST localhost:8080/v1/payments/\$PID/authorize -H 'Idempotency-Key: auth-1001' | jq -c '{status, amount_authorized}'"
run "api -X POST localhost:8080/v1/payments/\$PID/capture -H 'Idempotency-Key: cap-1001' | jq -c '{status, amount_captured, fee}'"

say "Retrying the same request replays the stored response; it does not capture twice"
run "curl -si \"\${H[@]}\" -X POST localhost:8080/v1/payments/\$PID/capture -H 'Idempotency-Key: cap-1001' | grep -E '^HTTP|Idempotent-Replayed'"

say "A new capture with a different key is refused by the state machine"
run "api -X POST localhost:8080/v1/payments/\$PID/capture -H 'Idempotency-Key: cap-1002' | jq -c .error"

say "The double-entry postings: debits positive, credits negative, summing to zero"
run "api localhost:8080/v1/payments/\$PID/ledger | jq -r '.data[].entries[] | \"\\(.amount)\\t\\(.account)\"'"

say "The audit checks every ledger invariant against the stored rows"
run "docker compose run --rm -T serve audit 2>/dev/null | tail -1"

say "100 goroutines capturing one payment, 100 retrying one key, 50 racing refunds (race detector on)"
run "go test -race -count=1 -tags=integration -run 'TestConcurrency' -v ./test/integration/ 2>&1 | grep -E '^(--- |ok)'"

say "Chaos: SIGKILL the API, dispatcher and receiver at failpoints and at random, then check everything"
run "go test -tags chaos -count=1 -run TestChaos -v ./test/chaos/ 2>&1 | grep -E 'flows|^(--- |ok)' | sed -E 's/restarts.*client/… client/'"

say "Done. Everything above is in the README and docs/"
