# Gateway benchmarks

Run on 2026-11-23 with `gwbench providers qwen-via-openai qwen-via-anthropic llama gemma fallback-to-local`.

## Provider latency

15 streaming requests per route, 1 concurrent, same prompt, max_tokens 200.

| Route | OK | First token p50 | First token p95 | Total p50 | Total p95 |
|---|---|---|---|---|---|
| qwen-via-openai | 15/15 | 132.2 ms | 355.9 ms | 11.17 s | 16.61 s |
| qwen-via-anthropic | 15/15 | 129.9 ms | 156.2 ms | 11.52 s | 14.82 s |
| llama | 15/15 | 98.2 ms | 7.19 s | 10.53 s | 17.56 s |
| gemma | 15/15 | 103.9 ms | 2.93 s | 4.46 s | 6.28 s |
| fallback-to-local | 15/15 | 127.7 ms | 8.44 s | 12.52 s | 17.77 s |
