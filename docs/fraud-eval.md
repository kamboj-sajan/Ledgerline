# Fraud rules evaluation

This report is **generated** by `cmd/fraudeval` from the datasets downloaded with
`scripts/download-datasets.sh`. Every number below comes from that run; rerun it to
reproduce them. The harness calls the same `fraud.Config.Signals` and
`fraud.Config.Fires`/`Decide` functions the API runs at authorization.

**Method.** Each dataset is replayed in time order. For each rule, every candidate threshold is
scored on the *tuning* split (the earlier data), and the one with the best F1 there is chosen.
Results are then reported on the *held-out* split (the later data), which plays no part in the
choice. "Flagged" means the rule fired (review or block). Both datasets are heavily imbalanced
(well under 1% fraud), so **accuracy is not reported**: flagging nothing would score above 99%.
Precision, recall and F1 are reported instead.

## ULB credit-card fraud (Kaggle mlg-ulb/creditcardfraud)

European cardholders over two days in September 2013 (per the dataset's description). Only `Time` and `Amount` are raw
fields. `V1`–`V28` are anonymised PCA components, and **there is no card identifier and no
location**. So velocity, relative-amount and geography rules cannot be computed, and **this
dataset can only test the absolute amount rule.** Split by time: the first 70% of
transactions for tuning, the last 30% held out.

| Split | Transactions | Fraud | Base rate |
|---|---|---|---|
| tune (first 70%) | 199364 | 384 | 0.193% |
| test (last 30%) | 85443 | 108 | 0.126% |

| Rule | Chosen threshold | Tune F1 | Test TP | Test FP | Test FN | Test TN | Test precision | Test recall | Test F1 | Test flag rate |
|---|---|---|---|---|---|---|---|---|---|---|
| amount_absolute (amount > threshold, EUR) | €699.00 | 0.007 | 7 | 1609 | 101 | 83726 | 0.004 | 0.065 | 0.008 | 1.89% |

<details><summary>Sweep: amount_absolute on ULB (threshold in EUR)</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| €23.00 | 0.002 | 0.461 | 0.004 | 0.001 | 0.389 | 0.002 |
| €38.42 | 0.002 | 0.419 | 0.004 | 0.001 | 0.380 | 0.003 |
| €60.32 | 0.002 | 0.380 | 0.005 | 0.001 | 0.306 | 0.003 |
| €101.98 | 0.002 | 0.258 | 0.005 | 0.002 | 0.269 | 0.004 |
| €143.00 | 0.003 | 0.203 | 0.005 | 0.002 | 0.222 | 0.004 |
| €206.59 | 0.003 | 0.167 | 0.006 | 0.002 | 0.185 | 0.005 |
| €283.50 | 0.004 | 0.128 | 0.007 | 0.003 | 0.157 | 0.006 |
| €370.01 | 0.003 | 0.083 | 0.006 | 0.003 | 0.102 | 0.005 |
| €531.68 | 0.004 | 0.055 | 0.007 | 0.004 | 0.093 | 0.008 |
| €699.00 **(chosen)** | 0.004 | 0.042 | 0.007 | 0.004 | 0.065 | 0.008 |
| €1031.06 | 0.003 | 0.016 | 0.005 | 0.004 | 0.028 | 0.007 |
| €1480.00 | 0.002 | 0.005 | 0.003 | 0.003 | 0.009 | 0.004 |
| €2249.00 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| €3075.00 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |

</details>

Fraud in this dataset is not concentrated among large amounts, so a "large amount" rule is a
weak detector here, and the table above shows how weak. The PCA features carry most of the
signal, and a rules engine over raw fields cannot use them.

## Sparkov simulated transactions (Kaggle kartik2112/fraud-detection)

Simulated with the Sparkov generator: 999 distinct US cards (counted in this run), with card
number, Unix time, amount, and cardholder and merchant latitude/longitude. Tuned on
`fraudTrain.csv` (earlier dates), reported on `fraudTest.csv` (later dates). Both files are
replayed as one time-ordered stream, so a test transaction sees the card's earlier train
transactions as history, as production would. Labels are never used to compute signals.
History window: 30 days. `amount_relative` needs at least 5 earlier transactions.
**Geography:** the data is US-only, so there is no country mismatch to test. Geo is measured as
the cardholder-to-merchant distance (`geo_distance`).

| Split | Transactions | Fraud | Base rate |
|---|---|---|---|
| tune (fraudTrain.csv) | 1296675 | 7506 | 0.579% |
| test (fraudTest.csv) | 555719 | 2145 | 0.386% |

### Per rule

| Rule | Chosen threshold | Tune F1 | Test TP | Test FP | Test FN | Test TN | Test precision | Test recall | Test F1 | Test flag rate |
|---|---|---|---|---|---|---|---|---|---|---|
| amount_absolute | $546.00 | 0.352 | 1025 | 3960 | 1120 | 549614 | 0.206 | 0.478 | 0.288 | 0.90% |
| amount_relative | 15× median | 0.270 | 691 | 3231 | 1454 | 550343 | 0.176 | 0.322 | 0.228 | 0.71% |
| geo_distance | 50 km | 0.012 | 1710 | 440286 | 435 | 113288 | 0.004 | 0.797 | 0.008 | 79.54% |
| velocity_count (1h0m0s) | 3 | 0.073 | 108 | 2385 | 2037 | 551189 | 0.043 | 0.050 | 0.047 | 0.45% |
| velocity_amount (1h0m0s) | $651.35 | 0.421 | 1199 | 3836 | 946 | 549738 | 0.238 | 0.559 | 0.334 | 0.91% |
| velocity_count (24h0m0s) | 4 | 0.014 | 1310 | 304722 | 835 | 248852 | 0.004 | 0.611 | 0.009 | 55.07% |
| velocity_amount (24h0m0s) | $2738.88 | 0.422 | 883 | 1899 | 1262 | 551675 | 0.317 | 0.412 | 0.358 | 0.50% |

### Combined

Rules are combined by **greedy forward selection on the tuning split**. Start with no rules; in
each round, enable the rule (at its tuned threshold) that most improves the combined tuning F1,
as computed by `fraud.Config.Decide` with a 24h0m0s velocity window (the window whose velocity
rules scored best on tuning). Stop when nothing improves it. The test split is only read to
report each step.

| Step | Rule enabled | Tune F1 | Test TP | Test FP | Test FN | Test TN | Test precision | Test recall | Test F1 | Test flag rate |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | velocity_amount (24h0m0s) > $2738.88 | 0.422 | 883 | 1899 | 1262 | 551675 | 0.317 | 0.412 | 0.358 | 0.50% |

Left disabled (adding any of them lowered the tuning F1): `amount_absolute`, `amount_relative`, `geo_distance`, `velocity_count (24h0m0s)`.

**Final combined detector on the held-out test split: precision 0.317, recall 0.412, F1 0.358, flagging 0.50% of transactions.**

### Threshold sweeps

The chosen row is the best F1 on the tuning split. The test columns show how each choice
generalises.

<details><summary>Sweep: amount_absolute</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| $47.52 | 0.009 | 0.792 | 0.018 | 0.006 | 0.790 | 0.012 |
| $60.94 | 0.011 | 0.781 | 0.022 | 0.008 | 0.778 | 0.015 |
| $75.03 | 0.015 | 0.781 | 0.030 | 0.010 | 0.778 | 0.020 |
| $94.68 | 0.023 | 0.781 | 0.044 | 0.015 | 0.777 | 0.030 |
| $110.84 | 0.030 | 0.776 | 0.058 | 0.020 | 0.772 | 0.039 |
| $136.67 | 0.044 | 0.763 | 0.083 | 0.030 | 0.757 | 0.057 |
| $165.30 | 0.063 | 0.761 | 0.116 | 0.043 | 0.752 | 0.081 |
| $196.31 | 0.088 | 0.760 | 0.158 | 0.061 | 0.752 | 0.112 |
| $248.07 | 0.144 | 0.749 | 0.242 | 0.101 | 0.738 | 0.178 |
| $337.98 | 0.157 | 0.541 | 0.243 | 0.111 | 0.538 | 0.184 |
| $546.00 **(chosen)** | 0.278 | 0.480 | 0.352 | 0.206 | 0.478 | 0.288 |
| $844.23 | 0.369 | 0.319 | 0.342 | 0.284 | 0.321 | 0.302 |
| $1125.70 | 0.080 | 0.028 | 0.041 | 0.047 | 0.025 | 0.032 |
| $1499.47 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |

</details>

<details><summary>Sweep: amount_relative</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| 1.5× median | 0.012 | 0.714 | 0.024 | 0.008 | 0.724 | 0.016 |
| 2× median | 0.017 | 0.708 | 0.033 | 0.012 | 0.719 | 0.023 |
| 3× median | 0.030 | 0.689 | 0.058 | 0.021 | 0.702 | 0.041 |
| 4× median | 0.051 | 0.649 | 0.095 | 0.037 | 0.670 | 0.070 |
| 5× median | 0.077 | 0.615 | 0.136 | 0.057 | 0.635 | 0.104 |
| 6× median | 0.100 | 0.583 | 0.171 | 0.073 | 0.587 | 0.130 |
| 8× median | 0.150 | 0.510 | 0.231 | 0.112 | 0.519 | 0.184 |
| 10× median | 0.184 | 0.452 | 0.262 | 0.137 | 0.456 | 0.210 |
| 15× median **(chosen)** | 0.234 | 0.321 | 0.270 | 0.176 | 0.322 | 0.228 |
| 20× median | 0.227 | 0.196 | 0.211 | 0.164 | 0.187 | 0.175 |

</details>

<details><summary>Sweep: geo_distance</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| 25 km | 0.006 | 0.953 | 0.012 | 0.004 | 0.950 | 0.008 |
| 50 km **(chosen)** | 0.006 | 0.802 | 0.012 | 0.004 | 0.797 | 0.008 |
| 75 km | 0.006 | 0.537 | 0.011 | 0.004 | 0.542 | 0.008 |
| 100 km | 0.006 | 0.233 | 0.011 | 0.004 | 0.231 | 0.008 |
| 110 km | 0.006 | 0.125 | 0.011 | 0.004 | 0.130 | 0.008 |
| 120 km | 0.006 | 0.051 | 0.010 | 0.004 | 0.054 | 0.007 |
| 130 km | 0.005 | 0.014 | 0.008 | 0.004 | 0.017 | 0.007 |
| 140 km | 0.006 | 0.001 | 0.002 | 0.007 | 0.002 | 0.003 |
| 150 km | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |

</details>

<details><summary>Sweep: velocity_count (1h0m0s)</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| 1 | 0.015 | 0.434 | 0.030 | 0.009 | 0.432 | 0.017 |
| 2 | 0.043 | 0.165 | 0.068 | 0.023 | 0.170 | 0.040 |
| 3 **(chosen)** | 0.108 | 0.055 | 0.073 | 0.043 | 0.050 | 0.047 |
| 4 | 0.213 | 0.017 | 0.031 | 0.074 | 0.013 | 0.022 |
| 5 | 0.275 | 0.004 | 0.007 | 0.129 | 0.004 | 0.008 |
| 6 | 0.188 | 0.000 | 0.001 | 0.300 | 0.001 | 0.003 |
| 8 | 0.000 | 0.000 | 0.000 | 1.000 | 0.000 | 0.001 |
| 10 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| 12 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| 15 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| 20 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| 25 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| 30 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |

</details>

<details><summary>Sweep: velocity_amount (1h0m0s)</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| $54.29 | 0.010 | 0.846 | 0.019 | 0.006 | 0.841 | 0.013 |
| $68.80 | 0.012 | 0.843 | 0.024 | 0.008 | 0.839 | 0.016 |
| $84.59 | 0.016 | 0.842 | 0.032 | 0.010 | 0.838 | 0.021 |
| $111.69 | 0.024 | 0.840 | 0.047 | 0.015 | 0.833 | 0.030 |
| $132.91 | 0.032 | 0.833 | 0.062 | 0.020 | 0.827 | 0.040 |
| $166.17 | 0.048 | 0.829 | 0.091 | 0.030 | 0.819 | 0.058 |
| $199.92 | 0.068 | 0.828 | 0.126 | 0.044 | 0.816 | 0.083 |
| $232.26 | 0.095 | 0.825 | 0.171 | 0.061 | 0.811 | 0.114 |
| $317.11 | 0.135 | 0.702 | 0.227 | 0.088 | 0.688 | 0.156 |
| $435.09 | 0.179 | 0.620 | 0.278 | 0.121 | 0.607 | 0.202 |
| $651.35 **(chosen)** | 0.332 | 0.574 | 0.421 | 0.238 | 0.559 | 0.334 |
| $1006.34 | 0.404 | 0.349 | 0.374 | 0.304 | 0.352 | 0.326 |
| $1725.32 | 0.528 | 0.182 | 0.271 | 0.394 | 0.182 | 0.249 |
| $2313.54 | 0.459 | 0.079 | 0.135 | 0.324 | 0.078 | 0.126 |

</details>

<details><summary>Sweep: velocity_count (24h0m0s)</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| 1 | 0.006 | 0.961 | 0.012 | 0.004 | 0.964 | 0.008 |
| 2 | 0.007 | 0.874 | 0.013 | 0.004 | 0.890 | 0.008 |
| 3 | 0.007 | 0.743 | 0.014 | 0.004 | 0.772 | 0.009 |
| 4 **(chosen)** | 0.007 | 0.586 | 0.014 | 0.004 | 0.611 | 0.009 |
| 5 | 0.007 | 0.423 | 0.014 | 0.004 | 0.449 | 0.008 |
| 6 | 0.007 | 0.281 | 0.013 | 0.004 | 0.308 | 0.007 |
| 8 | 0.005 | 0.092 | 0.009 | 0.003 | 0.121 | 0.005 |
| 10 | 0.002 | 0.022 | 0.004 | 0.002 | 0.041 | 0.003 |
| 12 | 0.001 | 0.005 | 0.002 | 0.001 | 0.017 | 0.002 |
| 15 | 0.001 | 0.001 | 0.001 | 0.001 | 0.007 | 0.002 |
| 20 | 0.001 | 0.000 | 0.000 | 0.004 | 0.003 | 0.003 |
| 25 | 0.000 | 0.000 | 0.000 | 0.015 | 0.003 | 0.005 |
| 30 | 0.000 | 0.000 | 0.000 | 0.088 | 0.003 | 0.005 |

</details>

<details><summary>Sweep: velocity_amount (24h0m0s)</summary>

| Threshold | Tune P | Tune R | Tune F1 | Test P | Test R | Test F1 |
|---|---|---|---|---|---|---|
| $235.48 | 0.011 | 0.968 | 0.022 | 0.007 | 0.962 | 0.013 |
| $294.45 | 0.014 | 0.956 | 0.027 | 0.008 | 0.952 | 0.016 |
| $371.34 | 0.018 | 0.923 | 0.035 | 0.010 | 0.921 | 0.019 |
| $486.33 | 0.026 | 0.900 | 0.051 | 0.014 | 0.900 | 0.027 |
| $574.73 | 0.034 | 0.887 | 0.066 | 0.018 | 0.886 | 0.035 |
| $710.49 | 0.050 | 0.855 | 0.094 | 0.026 | 0.859 | 0.050 |
| $840.80 | 0.069 | 0.829 | 0.127 | 0.035 | 0.835 | 0.067 |
| $975.83 | 0.093 | 0.799 | 0.166 | 0.047 | 0.806 | 0.089 |
| $1208.99 | 0.144 | 0.744 | 0.241 | 0.074 | 0.754 | 0.135 |
| $1434.39 | 0.200 | 0.689 | 0.309 | 0.107 | 0.710 | 0.185 |
| $1945.16 | 0.331 | 0.572 | 0.420 | 0.196 | 0.600 | 0.296 |
| $2738.88 **(chosen)** | 0.455 | 0.393 | 0.422 | 0.317 | 0.412 | 0.358 |
| $4094.82 | 0.467 | 0.161 | 0.240 | 0.334 | 0.156 | 0.213 |
| $5144.83 | 0.381 | 0.066 | 0.112 | 0.285 | 0.070 | 0.112 |

</details>

---

Generated by `go run ./cmd/fraudeval` in 1m6s (Go go1.27.4, darwin/arm64, 8 CPUs) on 2026-11-15.
