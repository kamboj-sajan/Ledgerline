#!/usr/bin/env bash
# Downloads the two public datasets used by cmd/fraudeval into data/
# (gitignored; the datasets are never committed).
#
#   ULB Credit Card Fraud Detection (mlg-ulb/creditcardfraud)
#     Machine Learning Group, Université Libre de Bruxelles.
#     Licence: Open Database License (database) / Database Contents License (contents).
#     Cite: A. Dal Pozzolo, O. Caelen, R. A. Johnson, G. Bontempi, "Calibrating
#     Probability with Undersampling for Unbalanced Classification", IEEE SSCI 2015.
#   Credit Card Transactions Fraud Detection Dataset (kartik2112/fraud-detection)
#     Kartik Shenoy; simulated with Brandon Harris's Sparkov Data Generation tool.
#     Licence: CC0 Public Domain.
#
# Needs a Kaggle API token from kaggle.com/settings, in any of these forms:
#   - a new-style token (KGAT_...) in $KAGGLE_API_TOKEN or ~/.kaggle/access_token
#   - a legacy key in ~/.kaggle/kaggle.json or $KAGGLE_USERNAME / $KAGGLE_KEY
# Uses only curl and unzip. The token is passed to curl on a file descriptor,
# not on the command line, so it does not appear in the process list.
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p data

token="${KAGGLE_API_TOKEN:-}"
if [[ -z "$token" && -f "${HOME}/.kaggle/access_token" ]]; then
  token=$(tr -d '[:space:]' < "${HOME}/.kaggle/access_token")
fi
if [[ -z "$token" ]] && [[ -z "${KAGGLE_USERNAME:-}" || -z "${KAGGLE_KEY:-}" ]]; then
  creds="${HOME}/.kaggle/kaggle.json"
  if [[ ! -f "$creds" ]]; then
    echo "no Kaggle token: set KAGGLE_API_TOKEN, or create ~/.kaggle/access_token or ~/.kaggle/kaggle.json" >&2
    exit 1
  fi
  # kaggle.json is {"username":"...","key":"..."}; read it without a JSON tool.
  KAGGLE_USERNAME=$(sed -n 's/.*"username"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$creds")
  KAGGLE_KEY=$(sed -n 's/.*"key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$creds")
fi

# auth_header prints the HTTP header for whichever credential is available.
auth_header() {
  if [[ -n "$token" ]]; then
    printf 'Authorization: Bearer %s' "$token"
  else
    printf 'Authorization: Basic %s' "$(printf '%s:%s' "$KAGGLE_USERNAME" "$KAGGLE_KEY" | base64)"
  fi
}

fetch() { # owner/dataset target-dir expected-file...
  local slug=$1 dir=$2; shift 2
  local ok=1
  for f in "$@"; do [[ -f "data/$dir/$f" ]] || ok=0; done
  if [[ $ok == 1 ]]; then echo "data/$dir already present"; return; fi
  echo "downloading $slug"
  curl --fail --location --silent --show-error \
    --header @<(auth_header) \
    --output "data/$dir.zip" \
    "https://www.kaggle.com/api/v1/datasets/download/$slug"
  unzip -o -q "data/$dir.zip" -d "data/$dir"
  rm "data/$dir.zip"
  for f in "$@"; do
    [[ -f "data/$dir/$f" ]] || { echo "expected data/$dir/$f after unzip" >&2; exit 1; }
  done
}

fetch mlg-ulb/creditcardfraud ulb creditcard.csv
fetch kartik2112/fraud-detection sparkov fraudTrain.csv fraudTest.csv

echo
echo "rows (including header):"
wc -l data/ulb/creditcard.csv data/sparkov/fraudTrain.csv data/sparkov/fraudTest.csv
shasum -a 256 data/ulb/creditcard.csv data/sparkov/fraudTrain.csv data/sparkov/fraudTest.csv
