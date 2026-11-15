# Benchmarks

Every number below is copied from k6's end-of-run summary. The raw summaries are written to
`loadtest/results/<scenario>-<rate>.json` by each run. Rerun with `make load`, or one scenario
with `loadtest/run.sh <scenario> <rates…>`.

## Environment

| | |
|---|---|
| Machine | Apple M2, 8 cores (4 performance + 4 efficiency), 8 GB RAM, macOS |
| Docker | Docker Engine 29.7.2 (Docker Desktop); the Linux VM has 8 CPUs and 3.8 GiB |
| Where things run | `serve` and Postgres in Compose containers; **k6 on the host, sharing the same CPUs** |
| Go | go1.27.4 linux/arm64 (the `golang:1.27-alpine` build image) |
| Postgres | 17.12 (`postgres:17-alpine`), stock settings: `max_connections=100`, `shared_buffers=163848kB`, `synchronous_commit=on`, `fsync=on`, `wal_level=replica`, `work_mem=4096kB` |
| App pool | pgx pool `pool_max_conns=8` (pgx's default here, `max(4, CPUs)`), unless stated |
| k6 | v2.4.0 |
| Simulated acquirer | `serve` defaults: 50 ms latency + up to 50 ms jitter, 5% declines, no timeouts or errors |
| Fraud rules | the embedded defaults (two review rules) |

Every run is 30 s at a constant arrival rate (k6's open model). k6 starts the target number of
iterations per second whatever the latency, so overload shows up as latency and dropped
iterations instead of silently lowering the load. Declines (402) count as success.
"Achieved" is completed iterations / 30 s. Latencies are per endpoint and exclude setup
traffic.

## Results

### 1. Full flow: create → authorize → capture

`make load FLOW_RATES="200 400 600 800"` (one iteration is 3 requests; 5% are declined at
authorize and skip capture):

| Target flows/s | Achieved | Endpoint | req/s | p50 ms | p95 ms | p99 ms | max ms | Errors | Dropped |
|---|---|---|---|---|---|---|---|---|---|
| 200 | 200.0 | create | 200.0 | 1.2 | 11.3 | 44.0 | 224.1 | 0.00% | 0 |
| | | authorize | 200.0 | 80.8 | 104.9 | 128.4 | 291.8 | 0.00% | |
| | | capture | 189.6 | 2.3 | 11.9 | 25.9 | 150.2 | 0.00% | |
| 400 | 400.0 | create | 400.0 | 1.8 | 66.1 | 125.9 | 224.5 | 0.00% | 0 |
| | | authorize | 400.0 | 86.2 | 166.7 | 252.4 | 400.9 | 0.00% | |
| | | capture | 380.5 | 3.4 | 57.6 | 107.5 | 219.9 | 0.00% | |
| **600** | **491.9** | create | 491.9 | 765.4 | 3570.6 | 3730.3 | 3837.1 | 0.00% | **3245** |
| | | authorize | 491.9 | 1476.0 | 5537.7 | 5710.6 | 5786.2 | 0.00% | |
| | | capture | 465.4 | 986.1 | 3036.3 | 3713.3 | 3838.2 | 0.00% | |
| 800 | 400.5 | create | 400.5 | 3082.7 | 4665.2 | 4899.0 | 4954.8 | 0.00% | 11985 |
| | | authorize | 400.5 | 5306.3 | 7454.5 | 7688.8 | 7762.3 | 0.00% | |
| | | capture | 380.4 | 3250.8 | 4622.6 | 4901.2 | 4960.5 | 0.00% | |

**Breaks between 400 and 600 flows/s** (about 1,180 to 1,450 requests/s). At 600 only 492/s
complete, and p99 goes from hundreds of milliseconds to seconds. At 800 throughput *falls* to
400/s. Past the limit, more concurrency makes everything slower instead of holding at the limit:
k6 runs up to 4,000 virtual users on the same CPUs as the server, and these runs do not separate
the server's share of that slowdown from k6's. There are no errors at any rate (no request
reached k6's 60 s timeout), because the server queues rather than fails. Authorize's
floor of about 80 ms is the simulated acquirer's latency. That wait happens outside any database
transaction, so it does not hold a connection.

### 2. Idempotent replays

`make load REPLAY_RATES="500 1000 2000 3000"`. Each request replays a stored capture:

| Target req/s | Achieved | p50 ms | p95 ms | p99 ms | max ms | Errors | Dropped |
|---|---|---|---|---|---|---|---|
| 500 | 500.0 | 0.9 | 2.7 | 23.1 | 82.0 | 0.00% | 0 |
| 1000 | 1000.0 | 0.7 | 2.0 | 29.3 | 106.3 | 0.00% | 0 |
| 2000 | 2000.0 | 0.4 | 5.4 | 204.6 | 346.6 | 0.00% | 0 |
| 3000 | 3000.0 | 0.4 | 8.0 | 79.2 | 247.4 | 0.00% | 0 |

No break up to 3,000/s. A replay is one indexed read of the stored response, with no lock
and no write. The run did not find the limit; with k6 on the same machine, k6 itself would
likely be the next limit.

**This scenario found a bug.** The first sweep, before the fix in commit `b963ae4`, gave:

| Target req/s | Achieved | p50 ms | p95 ms | p99 ms | Errors | Dropped |
|---|---|---|---|---|---|---|
| 250 | 250.0 | 1.6 | 7.5 | 271.3 | 0.05% | 0 |
| 1000 | 999.9 | 0.7 | 11.5 | 71.9 | 0.16% | 0 |
| 2000 | 1928.5 | 0.7 | 1118.9 | 2165.1 | 1.10% | 2147 |

The errors were `409 idempotency_key_in_use` on keys whose request had *finished*. Every request
took the key's advisory try-lock before reading the stored response, so concurrent replays of
the same key collided. A direct probe showed 80 of 200 concurrent replays rejected. The fix reads
finished responses without the lock (`idempotency.Peek`); a finished record never changes, so
this is safe. `TestConcurrencyReplaysOfFinishedKey` pins it.

### 3. One hot payment

`make load HOT_RATES="200 300 400 600"`. Each request is a 1-cent refund of the same payment:

| Target req/s | Achieved | p50 ms | p95 ms | p99 ms | max ms | Errors | Dropped |
|---|---|---|---|---|---|---|---|
| 200 | 200.0 | 2.5 | 20.4 | 205.2 | 359.8 | 0.00% | 0 |
| 300 | 300.0 | 2.0 | 3.8 | 14.5 | 71.4 | 0.00% | 0 |
| 400 | 400.0 | 1.9 | 332.2 | 418.5 | 489.1 | 0.00% | 0 |
| **600** | **583.7** | 9.2 | 2974.8 | 3313.3 | 3341.1 | 0.00% | **491** |

**Breaks between 400 and 600 refunds/s on one payment.** Every refund locks the same row
(`SELECT … FOR UPDATE`, [ADR 0003](adr/0003-locking.md)), and the Postgres samples taken during
this run show `Lock:tuple` and `Lock:transactionid` waits. The refunds run one at a time, so
throughput is about 1 / (time the lock is held): each transaction holds it across its ledger
insert, refund insert, payment update, outbox insert, stored response and commit. No refund
ever over-refunded: the payment ended consistent and the audit passed.

## Where it breaks, and why

For the full flow, Postgres was sampled every 2 s during the sweep (`pg_stat_activity` by state
and wait event) along with container CPU (`docker stats`):

- **With the default pool of 8, the pool is the bottleneck.** From 400 flows/s on, all 8
  connections were busy in every sample: 9 client sessions, which is the pool plus the sampler.
  The top wait events were `Client:ClientRead` (Postgres idle inside a transaction, waiting for
  the application's next statement) and `LWLock:WALWrite` / `IO:WalSync` (commit flushes). CPU
  was far from saturated: Postgres peaked at 382% and the server at 224%, out of 800%. Requests
  were waiting for a connection, not for CPU.
- **Confirmed by changing only the pool size.** Each row is a fresh stack, flow scenario, 30 s:

  | Pool | Target | Achieved | create p99 ms | authorize p99 ms | capture p99 ms | Dropped |
  |---|---|---|---|---|---|---|
  | 8 | 400 | 398.7 | 660.8 | 1102.6 | 620.4 | 39 |
  | 8 | 600 | 585.8 | 1266.3 | 1873.5 | 1218.2 | 427 |
  | 32 | 400 | 400.0 | 30.9 | 124.0 | 27.9 | 0 |
  | 32 | 600 | **600.0** | **88.9** | **198.4** | **72.7** | **0** |
  | 32 | 800 | 800.0 | 319.4 | 565.5 | 372.0 | 0 |
  | 32 | 1000 | 582.9 | 4204.9 | 6139.0 | 4186.1 | 12507 |

  With 32 connections the same machine sustains 800 flows/s (about 2,360 req/s) and breaks
  between 800 and 1,000.
- **At pool 32 the limit moves to Postgres commits and the machine itself.** All 32 connections
  were busy (median 28 non-idle sessions). The waits were `ClientRead` and heavy
  `LWLock:WALWrite`, and Postgres CPU reached 431% median, 486% max, while the server used about
  200% and k6 shared the same 8 cores on the host. Each flow commits about 5 times: create,
  authorize phases 1 and 3, capture, and the acquirer simulator's own write. So 800 flows/s is
  roughly 4,000 synchronous commits per second through one WAL. On one laptop these effects
  cannot be separated cleanly. Separating them would need k6 on another machine and a profile
  of Postgres.

**Variance.** The pool-8 run at 400/s in the experiment above (p95 574 ms) was much worse than
the same rate in the sweep (p95 66 ms). The experiment's run was the first on a fresh database,
and the system was right at its pool limit, where small differences queue up quickly. The first
step of each sweep is similarly noisy (for example, hot at 200/s has a worse p99 than at 300/s).
Treat single numbers near the limit as ±2×; the breaking points themselves reproduced in every
run.

## What would raise the limits

1. **A bigger pool**, sized to Postgres's cores rather than the app's, since the app spends most
   of its connection time in round trips. The measurements above show the effect directly.
2. **Fewer round trips per transaction:** batch the payment update, ledger insert, outbox insert
   and stored response into one `pgx.Batch` or one CTE. `ClientRead` was the largest wait.
3. **Fewer commits per flow:** authorize's two phases are necessary for crash safety, but create
   could be merged into the first authorize call. `synchronous_commit=off` would remove the
   flush wait but risks losing the last few acknowledged writes on a crash, which is wrong for a
   ledger.
4. **Hot payments are capped by design** (one row lock). Higher rates on one payment would need
   splitting the work, for example queuing refunds and applying them in batches.
5. **A separate load generator** and a production-sized Postgres, to measure the system rather
   than the laptop.
