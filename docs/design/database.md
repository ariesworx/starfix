# starfix: is Dolt the right database?

6 October 2026. This is based on a survey of bd 1.2.2's source, DoltHub's own benchmarks and posts, and running Dolt 2.4.2. Sources are at the end.

## Answer

Yes, keep Dolt, with two conditions:

1. **Put a starfix server in front of it.** Clients should not run SQL against Dolt through the tunnel.
2. **Run a one-day spike first**, measuring claim contention and commit throughput.

No alternative gives us versioned history, an audit trail and three-way merge for free. The things Dolt lacks (row locks, push notifications, a server clock you can trust from the client) belong in a starfix server anyway.

## What we need, and how each database does

| Need | Dolt 2.x | Postgres 17 | Doltgres 1.0 | SQLite / libSQL |
|---|---|---|---|---|
| Shared server for many machines | yes | yes | yes | no (single writer) |
| Per-cell history, `AS OF`, diff, blame (audit, conflict base versions) | **built in** | triggers / temporal tables, by hand | built in | no |
| Branch and merge (offline replay, what-if) | **built in** | no | built in | no |
| Speed | MySQL parity overall (mean ×0.99); writes faster, point selects ×1.35 | fastest | ~2.6× slower than Postgres (target) | fastest locally |
| Row locks, `SKIP LOCKED` work queue | no; repeatable read; same-cell conflict fails at commit | yes | partial | n/a |
| Push notifications (`LISTEN/NOTIFY`) | no | yes | no | no |
| Full-text search | yes (2023) | yes | partial | FTS5 |
| Vector search | yes: `VECTOR` type and vector indexes (2025) | pgvector, mature | not yet comparable | extensions |
| Go driver, pure Go | go-sql-driver/mysql | pgx | pgx | modernc.org/sqlite |
| Maturity / support | v2, commercial support, small community | very large | v1.0, August 2026 | very large |
| Already built for us | **server, provisioning, backups, tunnel, OpenTofu** | none | none | none |

## The gaps, and where they go

- **Lost updates.** Dolt runs at repeatable read. Two writers changing the same cell fail at commit, and two changing different cells both win silently. Fix: every write is a compare-and-set on a version column (`UPDATE … WHERE id=? AND version=?`), retried on a deadlock-style error. bd already does this with `row_lock`; ours is stricter.
- **No row locks or `SKIP LOCKED`.** A claim is a conditional update on version and lease, and the row count says who won. That is enough for a claim queue. The spike measures conflict rates with 20 or more concurrent claimers.
- **No push notifications.** The starfix server sends events to connected clients after its own writes. It is the only writer, so it knows every change.
- **One global commit lock.** DoltHub reports "hundreds of commit graph operations per second". A team of agents writes tens per second at most. Batching Dolt commits (one per request, or a short window) keeps headroom.
- **Clock.** Leases and reaping use the server's clock only. That is a server job, not a client one.

## Architecture this implies

```
agent ─MCP(stdio)─> starfix (client: MCP server, CLI, SQLite cache + op log)
                        │  compact RPC over SSH (in-process, pinned host key)
                        ▼
                  starfixd (server: auth per developer, leases, reaper,
                        │    events, gates, fencing, notifications)
                        ▼
                  dolt sql-server (loopback only)
```

- **Bandwidth:** the client sends small typed requests and gets deltas back. It never runs SQL or downloads whole tables.
- **Offline:** the client keeps a SQLite read cache and an operation log, and replays the log on reconnect. The server merges each change against its base version, which Dolt history provides (`AS OF`).
- **Identity:** the SSH key maps to a developer, so the actor comes from the authenticated key, not from what the client claims. Each session gets its own id under that developer.
- **Admin:** `starfix admin …` over the same channel, with server-side rights checked per developer.

## Vectors (later)

**Storage:** an `embeddings` table keyed by (kind, id, model), with a content hash so unchanged text is never re-embedded. Changing the model means re-embedding into new rows; nothing else migrates.

### Where vectors are stored and searched

| Option | Fit | Cost | Verdict |
|---|---|---|---|
| Brute-force cosine in Go over rows in Dolt | Exact results. 768-dimension float32 is 3 KB per item; 50k items is 150 MB and a scan takes tens of ms. int8 quantization cuts both by 4. | None: no new component | **Start here** |
| Dolt vector index (2025) | Same database, same backups and history; SQL `VEC_DISTANCE` | Young; recall and speed at our scale unmeasured | Switch on past ~50k items, if spike item 3 passes |
| Postgres + pgvector | Mature HNSW and IVFFlat indexes; the obvious choice if we fall back to Postgres anyway | A second database if Dolt stays | Only with the Postgres fallback |
| SQLite + sqlite-vec on the client | Offline similarity and duplicate checks | C extension: needs CGO or a WASM SQLite driver; a full copy of vectors on every machine | Not now; offline search falls back to full-text |
| Dedicated vector DB (Qdrant, Weaviate) | Built for millions of vectors, filtering, hybrid search | Another service to run, back up and secure, kept in sync with Dolt | No; our scale is thousands, not millions |

Vectors don't decide the database: every row above works behind starfixd, and the brute-force start needs nothing new.

### Embedding model: local or hosted

| | Local, on the starfix server (EmbeddingGemma, nomic-embed-text) | Hosted (OpenAI text-embedding-3, Google Gemini embeddings) |
|---|---|---|
| Data | Text never leaves our server | Every issue, comment and memory is sent to the provider, which may be unacceptable for confidential or client material |
| Quality | Good; the gap to hosted is small for short issue text | Best on benchmarks; larger dimensions |
| Cost | Server CPU and roughly 1 GB RAM; may need a bigger server | Fractions of a cent per thousand issues |
| Speed | Tens of ms per item on CPU; works when the internet doesn't | A network round trip; rate limits; outages |
| Operations | One more process to run and update (for example Ollama) | An API key in a secret store, billing, egress |
| Churn | We choose when to upgrade | Providers retire models, which forces a full re-embed on their schedule |
| Licensing | nomic-embed-text: Apache-2.0. EmbeddingGemma: Gemma terms (use restrictions, not OSI) | Commercial terms |


## Spike (one day, before any schema work)

1. 20–50 goroutines claiming from one ready queue through compare-and-set: conflict rate, p99 latency, double claims (must be 0).
2. A sustained write mix with a Dolt commit per request versus batched commits: throughput and p99.
3. Optional: the vector index at 50k rows, recall@10 and latency against brute force.

If 1 or 2 fails badly, the fallback is Postgres behind the same starfixd interface. The server boundary keeps that a contained change.

## Sources

- Dolt as fast as MySQL (2025-12-04): https://www.dolthub.com/blog/2025-12-04-dolt-is-as-fast-as-mysql/
- Dolt concurrency (2026-02-17): https://www.dolthub.com/blog/2026-02-17-dolt-concurrency
- Doltgres 1.0 (2026-07-30): https://dolthub.com/blog/2026-07-30-doltgres-1-0-one-week-out/
- Vector indexes (2025-01-16): https://www.dolthub.com/blog/2025-01-16-announcing-vector-indexes/
- Full-text indexes (2023-07-26): https://dolthub.com/blog/2023-07-26-announcing-fulltext-indexes
