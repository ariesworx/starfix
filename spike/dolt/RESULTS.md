# starfix stage 0: Dolt spike results

**Verdict: Dolt OK with conditions.** No double claims in any run, and write throughput (about 250 transactions/s on 4 vCPUs) is ten times what a team of agents needs. But Dolt's conflict detection is per cell, not per row, so `rev = rev + 1` alone is **not** a compare-and-swap: concurrent writes that touch different columns, or write the same value, both succeed silently. starfixd must (1) stamp every write with a value unique to that write, (2) serialize claims itself instead of relying on database contention, and (3) batch Dolt commits.

6 October 2026. Code: this directory (`go run ./spike/dolt -dsn 'root@tcp(127.0.0.1:PORT)/' <test> [flags]`).

## Environment

| | |
|---|---|
| Dolt | 2.4.2, `dolt sql-server` on 127.0.0.1, default settings (`max_connections: 200`), fresh data dir |
| Isolation | `REPEATABLE-READ` (Dolt default) |
| Machine | 4 vCPU Intel Xeon @ 2.10GHz, 16 GB RAM, Linux; client and server on the same machine, loopback |
| Client | Go 1.27.1, go-sql-driver/mysql v1.10.1, one `*sql.Conn` per goroutine |
| MySQL/MariaDB | Not installed; no comparison run |

## 1. Claim contention

2,000 open issues, one ready queue ordered by `(priority, id)`. Each attempt reads the queue head, then claims with `UPDATE issues SET assignee=?, status='in_progress', rev=rev+1, updated_at=NOW(6) WHERE id=? AND rev=? AND assignee IS NULL`, checking `RowsAffected`. `tx` also inserts the `claims` row in the same transaction; `auto` is the bare autocommit update. `pick` = choose randomly among the top N (1 = everyone fights for the head; 0 = queue partitioned so workers never collide). Runs stop when the queue drains or after 30-60 s.

| Workers | Mode | pick | Claims/s | Failed attempts | Attempts/claim | p50 ms | p99 ms | Double claims |
|---|---|---|---|---|---|---|---|---|
| 1 | tx | 1 | **182** | 0% | 1.00 | 5.8 | **8.5** | 0 |
| 4 | tx | 16 | **283** | 17% | 1.20 | 11.9 | 41 | 0 |
| 20 | tx | 1 | 26 | 95% | 19.1 | 356 | 4,621 | 0 |
| 20 | tx | 16 | 163 | 55% | 2.23 | 95 | 446 | 0 |
| 20 | auto | 1 | 33 | 95% | 19.1 | 286 | 3,567 | 0 |
| 20 | auto | 16 | 202 | 55% | 2.22 | 77 | 386 | 0 |
| 20 | forupdate | 1 | 26 | 95% | 19.0 | 72 | 4,369 | 0 |
| 20 | tx | 0 (no collisions) | 261 | 0 | 1.00 | 72 | 164 | 0 |
| 50 | tx | 1 | 10 | 98% | 49.0 | 1,873 | 22,427 | 0 |
| 50 | tx | 16 | 92 | 76% | 4.15 | 369 | 2,334 | 0 |
| 50 | auto | 16 | 113 | 76% | 4.18 | 311 | 1,868 | 0 |
| 50 | tx | 64 | 189 | 44% | 1.79 | 180 | 824 | 0 |
| 50 | tx | 0 (no collisions) | 252 | 0 | 1.00 | 190 | 304 | 0 |

Latency is first attempt to win, including retries. Almost all failures are `1213 serialization failure` at `COMMIT` (or on the autocommit statement); a minority are the CAS matching zero rows. Verification after every run: each issue won by at most one client, the database `assignee` matches the client-recorded winner for every row, and each `claims` row matches with `epoch = 1`.

**`SELECT ... FOR UPDATE` does not lock.** A second transaction's `FOR UPDATE` on a row another transaction holds returns in 0.6 ms; both update; the second committer gets `1213`. It is accepted syntax with optimistic semantics, no better than the CAS.

Reading: the database tops out at about 250-280 claim transactions/s however many connections push. Concurrency adds only conflicts and queueing. One serialized claimer gives 182/s at p99 8.5 ms; 50 contending claimers on the head give 10/s at p99 22 s.

## 2. Lost updates (two overlapping transactions, default isolation)

Both transactions take their snapshot before either writes; A commits first.

| Pattern | A | B | Final row | Outcome |
|---|---|---|---|---|
| Different columns (`title`, `priority`) | ok | ok | both changes | Merged; no error |
| Same column, different values | ok | **1213 at commit** | A's value | Safe |
| Same column, same value | ok | ok | value | Both told success |
| `priority = priority + 1` | ok | ok | 2 (expected 3) | **Lost update, silent** |
| CAS on `rev`, different columns | ok | ok | both changes, rev 2 (expected 3) | **CAS broken, silent**: both "won" rev 1 |
| CAS on `rev`, same claim value (same holder) | ok | ok | rev 2 | Both told they claimed |
| CAS on `rev` + unique stamp column | ok | **1213** | A only | Safe |
| Increment + unique stamp | ok | **1213** | 2 | Safe |
| Autocommit CAS, sequential | 1 row | 0 rows | A | Safe |

Concurrent autocommit statements on one hot row, 20 workers x 50:

| Statement | Reported 1 row | Applied | Errors |
|---|---|---|---|
| `SET priority = priority + 1` | 1,000 | **549** (final 550 from 1) | 0 |
| rev CAS, `title = worker name` | 923 | **582** | 4 |
| rev CAS, half set `title`, half set `priority` | 937 | **532** | 2 |
| rev CAS, `title` unique per write | 579 | 579 | 273 |
| rev CAS + unique stamp (`assignee=UUID()`), half/half | 584 | 584 | 278 |

Rule observed: Dolt merges concurrent transactions cell by cell. It raises `1213` only when both change **the same cell to different values**. `rev = rev + 1` from the same base produces the same value on both sides, so it never conflicts by itself. Writing a value no other writer can produce (a random token per write) turns this into first-committer-wins per row, and the CAS becomes exact.

## 3. Commit throughput

Each request is one SQL transaction: read `rev`, CAS update on a random issue of 2,000, an `events` insert, and a `comments` insert half the time. 15-20 s per run.

| Writers | Dolt commits | Requests/s | p50 ms | p99 ms | Dolt commits made | Errors (1213) |
|---|---|---|---|---|---|---|
| 1 | none (working set) | 233 | 4.3 | 7.0 | 0 | 0 |
| 1 | one per request | 159 | 6.2 | 9.0 | 2,391 | 0 |
| 1 | every 1 s | 236 | 4.2 | 7.2 | 15 | 0 |
| 8 | none | 266 | 28.7 | 57.0 | 0 | 25 |
| 8 | one per request | 106 | 72.0 | 126.8 | 2,133 | 8 |
| 8 | every 50 writes | 253 | 30.1 | 64.1 | 101 | 17 |
| 8 | every 1 s | 250 | 29.9 | 88.9 | 20 | 14 |
| 20 | none | 247 | 73.4 | 142.8 | 0 | 36 |
| 20 | one per request | 112 | 174.6 | 231.8 | 1,692 | 4 |
| 20 | every 1 s | 266 | 72.2 | 130.1 | 15 | 32 |

A background `DOLT_COMMIT('-Am', ..., '--skip-empty')` takes 27-31 ms p50, 35-70 ms p99. Batched commits cost nothing measurable; one per request costs a third (1 writer) to over half (8-20 writers) of throughput. The server was CPU-bound (dolt ~290% of 4 cores, 1.4 GB RSS).

## 4. Vectors (50,000 x 768 float32, unit-normalized)

| | Clustered (200 Gaussian clusters) | Uniform random |
|---|---|---|
| Load (200-row inserts, `STRING_TO_VECTOR` text) | 17.4 s (2,866 rows/s) | 16.1 s |
| `ALTER TABLE ... ADD VECTOR INDEX` | 29.1 s | 24.8 s |
| Go brute force, exact, single thread | p50 35.5 / p99 51.5 ms | p50 35.3 / p99 38.9 ms |
| Dolt SQL scan, no index | p50 7,465 ms (3 queries) | not run |
| Dolt vector index, top 10 | p50 11.8 / p99 15.7 ms | p50 11.2 / p99 17.4 ms |
| Dolt index recall@10 vs exact | **0.836** | 0.074 |

Syntax in 2.4.2: `v VECTOR(768) NOT NULL`, `VECTOR INDEX name (v)`, `ORDER BY VEC_DISTANCE(v, ?) LIMIT k`. Findings:

- `VEC_DISTANCE` is squared L2 (`VEC_DISTANCE_L2_SQUARED` in the plan); `VEC_DISTANCE_COSINE` exists but the index serves only L2, so store normalized vectors.
- The index is used only when the query vector is a bare string (literal or `?` placeholder). `VEC_DISTANCE(v, STRING_TO_VECTOR(?))` silently falls back to a full scan (7.5 s here).
- Binary parameters (little-endian float32 bytes) are rejected: `value of type string cannot be converted to 'vector' type`.
- Uniform random vectors have no meaningful nearest neighbors, so their 0.074 recall says little; the clustered figure is the relevant one.

Reading: the index is 3x faster than an unoptimized Go scan but misses 1 in 6 true neighbors. The design's "start with brute force in Go" stands; 35 ms single-threaded is acceptable at 50k and is easy to parallelize or quantize.

## What starfixd must do

1. **Stamp every write.** Each `UPDATE` sets a column to a value unique to that write (for example `write_id = <random 64-bit>`), alongside `rev = rev + 1`. Without it, CAS and read-modify-write lose updates silently. Add a test that fails if any write path omits it.
2. **Serialize claims in the server.** One claim goroutine (or a mutex per ready queue) choosing the issue and doing the CAS. It is faster than letting connections contend (182/s at p99 8.5 ms vs 10-26/s at multi-second p99) and leaves retries for the rare cross-path conflict.
3. **Small write pool.** 1-4 writer connections; throughput does not rise past that and latency does.
4. **Batch Dolt commits**, about once a second or per N writes, from one committer goroutine with `--skip-empty`. Not one per request.
5. **Retry `1213`** on every write path; it is the normal conflict signal, not an exceptional error.
6. **Do not use `FOR UPDATE`** as a lock.
7. **Vectors:** brute force in Go first; if the index is adopted later, pass a bare string, never `STRING_TO_VECTOR`.

## Caveats

- One machine, 4 vCPUs, loopback: no network latency, and client and server shared the CPU. A server with more cores may raise the ~250 txn/s ceiling; a tunnel adds round-trip time per statement, which favors fewer statements per request.
- Short runs (7-60 s) on small tables (2,000 issues); no growth of history over days, no garbage collection, no `dolt_gc` effects measured.
- Data dir on the container's filesystem; durability settings at defaults and not varied.
- Worst-case contention (everyone claims the queue head) is deliberately pessimistic; the realistic case with server-side claiming is the 1-worker row.
- No MySQL or Postgres baseline: none was installed.
- The Go brute force is single-threaded and unoptimized (no SIMD, no int8).

## Found later (stage 1)

- `SELECT MAX(pk)` and `COALESCE(MAX(pk), 0)` over an empty table's primary key return **no row at all** in Dolt 2.4.2; `COUNT(*)` is fine. Read the last key with `ORDER BY pk DESC LIMIT 1` instead.
