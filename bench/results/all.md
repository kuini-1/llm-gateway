# Gateway benchmarks

Run on 2026-11-23 with `gwbench all`.

## Gateway overhead

2000 chat requests per row, 32 concurrent, against a mock provider with a fixed 50 ms response time. Each gateway request authenticates the key, checks the rate limit, reserves and settles spend in Redis and queues a log row for Postgres.

| Path | OK | p50 | p95 | p99 | First token p50 | Throughput |
|---|---|---|---|---|---|---|
| direct to provider | 2000/2000 | 52.5 ms | 55.5 ms | 59.6 ms | – | 601 req/s |
| through the gateway | 2000/2000 | 60.3 ms | 66.4 ms | 69.6 ms | – | 523 req/s |
| direct, streaming | 2000/2000 | 52.8 ms | 58.1 ms | 64.0 ms | 51.5 ms | 593 req/s |
| gateway, streaming | 2000/2000 | 57.6 ms | 71.8 ms | 81.0 ms | 55.0 ms | 535 req/s |

Added latency: **7.8 ms at p50, 10.9 ms at p95** (non-streaming).

## Provider outage

600 requests per row, 16 concurrent. The primary mock answers in 150 ms ± 50 ms, the secondary in 200 ms ± 50 ms. "Down" means every primary request returns 503. The circuit breaker opens after 5 consecutive failures and probes again after 30 s. Latency is over all requests, failed ones included.

| Scenario | Succeeded | Served by the fallback | p50 | p95 | Calls that reached the dead primary |
|---|---|---|---|---|---|
| healthy | 600/600 (100.0%) | 0 | 181.8 ms | 204.0 ms | – |
| primary down, fallback route | 600/600 (100.0%) | 600 | 230.9 ms | 255.7 ms | 16 of 600 |
| primary down, single-target route (circuit already open) | 0/600 (0.0%) | 0 | 5.3 ms | 9.9 ms | 0 of 600 |

Once the breaker opens, the fallback route sends requests straight to the secondary instead of paying for a failed call first, and the single-target route fails fast with 503 `circuit_open` instead of waiting on a provider that is down.

## Embedding cache

A synthetic RAG workload through the `bench-embed` route: index 400 chunks of about 110 words, re-index three times with 10% of chunks edited each time, then serve 2,000 single-text search queries drawn from 300 distinct queries with a Zipf (s = 1.1) skew, so a few queries are very common. Cost uses $0.02 per million tokens, the price configured for this route.

| Phase | Inputs | Cache hits | Paid | Saved | p50 all hits | p50 all misses |
|---|---|---|---|---|---|---|
| initial index (400 chunks) | 400 | 0 (0%) | $0.001549 | $0.000000 | – | 57.1 ms |
| re-index 1 (10% of chunks edited) | 400 | 360 (90%) | $0.000157 | $0.001394 | – | – |
| re-index 2 (10% of chunks edited) | 400 | 362 (90%) | $0.000149 | $0.001404 | 4.2 ms | – |
| re-index 3 (10% of chunks edited) | 400 | 361 (90%) | $0.000152 | $0.001402 | – | – |
| 2,000 search queries (300 distinct, Zipf) | 2000 | 1763 (88%) | $0.000129 | $0.000945 | 1.6 ms | 48.7 ms |
| **total** | 3600 | 2846 (79%) | $0.002137 | $0.005145 | | |

The cache cut embedding spend by **71%** on this workload ($0.002137 paid instead of $0.007282).
