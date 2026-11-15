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
# Needs a Kaggle API token (kaggle.com/settings → Create New Token), either in
# ~/.kaggle/kaggle.json or as KAGGLE_USERNAME and KAGGLE_KEY. Uses only curl
# and unzip.
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p data

if [[ -z "${KAGGLE_USERNAME:-}" || -z "${KAGGLE_KEY:-}" ]]; then
  creds="${HOME}/.kaggle/kaggle.json"
  if [[ ! -f "$creds" ]]; then
    echo "no Kaggle token: set KAGGLE_USERNAME/KAGGLE_KEY or create $creds" >&2
    exit 1
  fi
  # kaggle.json is {"username":"...","key":"..."}; read it without a JSON tool.
  KAGGLE_USERNAME=$(sed -n 's/.*"username"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$creds")
  KAGGLE_KEY=$(sed -n 's/.*"key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$creds")
fi

fetch() { # owner/dataset target-dir expected-file...
  local slug=$1 dir=$2; shift 2
  local ok=1
  for f in "$@"; do [[ -f "data/$dir/$f" ]] || ok=0; done
  if [[ $ok == 1 ]]; then echo "data/$dir already present"; return; fi
  echo "downloading $slug"
  curl --fail --location --silent --show-error \
    --user "$KAGGLE_USERNAME:$KAGGLE_KEY" \
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
