// Package store is starfixd's issue store on a Dolt sql-server.
//
// It owns the schema (embedded, versioned migrations in schema_migrations)
// and exposes a small typed API: issues, dependencies, labels, comments,
// ready and blocked queries, and the event log. Ready and blocked are
// computed at read time with recursive CTEs; nothing derived is stored.
//
// # Concurrency
//
// Writes go through one connection, so within a process they are
// serialized and the event sequence is gapless without contention. Reads
// use a separate pool and see committed transactions.
//
// Every mutation runs in one SQL transaction that also appends an event
// (principal, session, machine, op, before and after). Dolt detects
// conflicts per cell, not per row, so a rev check alone is not a
// compare-and-swap: two writers that read the same rev both write rev+1
// and both succeed. Every UPDATE therefore also sets write_id to a value
// unique to that write, which turns the race into a same-cell conflict;
// Dolt then fails the later commit with error 1213. Two transactions also
// conflict on the events primary key, since both claim the next seq. A
// write that loses is rerun from the start, up to Options.MaxAttempts, and
// rereads the row, so a stale expected rev surfaces as ErrConflict. This
// covers a second process on the same database, such as an admin tool.
//
// SELECT ... FOR UPDATE does not lock in Dolt and is not used.
//
// Transactions update Dolt's working set. A committer goroutine turns
// pending writes into a Dolt commit at most once per CommitInterval
// (default 1s), with --skip-empty; Flush and Close commit at once. The
// commit message names the last event sequence it covers.
package store
