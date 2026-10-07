// Package store is starfixd's issue store: a typed API over one database
// on a Dolt sql-server, and the only code that writes to it.
//
// [Open] checks the database account, applies the embedded migrations
// (versioned in schema_migrations) and returns a [Store], which is safe
// for concurrent use. Every write names its [Actor]: the principal,
// session and machine that made it. The store holds issues ([Issue],
// keyed by [IssueID]) with their dependencies ([Dep]), labels and
// comments ([Comment]); claims, which lease an issue to one session
// ([Claim]); the agents registry ([Agent]); inbox items ([InboxItem]),
// pushed to each [Watch] as they commit; handoff notes ([Handoff]);
// acceptance checklists ([AcceptanceItem]); and the event log ([Event]).
// Ready, Blocked, Digest and acceptance items are computed at read time,
// with recursive CTEs where they follow the graph; nothing derived is
// stored.
//
// The design is docs/design/starfix.md: §3 for the data model, §7 for
// claims, the registry, inboxes, handoffs, idempotency and authorization,
// and §8 for how concurrent changes resolve. docs/design/database.md says
// why the database is Dolt.
//
// # Errors
//
// A refusal wraps one of the sentinel errors, such as [ErrNotFound],
// [ErrConflict] or [ErrInvalid]; test for them with [errors.Is]. An error
// that wraps none is a failure, such as a lost database connection or a
// canceled context. Refusals that carry detail are typed and wrap a
// sentinel: [*HeldError], [*IdemError] and [*StaleEpochError] wrap
// ErrConflict, [*ForbiddenError] wraps [ErrForbidden], and
// [*AcceptanceError] wraps ErrInvalid; read their fields with
// [errors.As]. [Open] refuses an unsafe database account with an
// [*UnsafeAccountError].
//
// # Authorization
//
// While a live claim holds an issue, only the holder's principal and the
// admins named in [Options.Admins] may update, close, reopen, finish,
// hand off or accept it. Anyone else is refused with a [*ForbiddenError]
// before anything is written. Comments, labels and dependencies stay open
// to everyone. An admin's change to an issue another principal holds is
// recorded as an [OpAdminOverride] event ahead of the change's own events.
//
// # Concurrency
//
// Writes run on one connection, so within a process they are serialized
// and the event sequence is gapless without contention. Reads use a
// separate pool and see committed transactions.
//
// Every write runs in one SQL transaction, and each change in it that is
// history appends an event in the same transaction (principal, session,
// machine, op, before and after). Bookkeeping that is not history records
// none: lease renewals, registry touches, inbox acks and [Store.Prune].
//
// Dolt detects conflicts per cell, not per row, so a rev check alone is
// not a compare-and-swap: two writers that read the same rev both write
// rev+1 and both succeed. Every UPDATE therefore also sets write_id to a
// value unique to that write, which turns the race into a same-cell
// conflict; Dolt then fails the later commit with error 1213. Two
// transactions also conflict on the events primary key, since both claim
// the next seq. A write that loses is rerun from the start, up to
// [Options.MaxAttempts], and rereads the row, so a stale expected rev
// surfaces as [ErrConflict]. This covers a second process on the same
// database, such as an admin tool.
//
// SELECT ... FOR UPDATE does not lock in Dolt and is not used.
//
// Transactions update Dolt's working set. A committer goroutine turns
// pending writes into a Dolt commit at most once per
// [Options.CommitInterval] (default 1s), with --skip-empty; [Store.Flush]
// and [Store.Close] commit at once. The commit message names the last
// event sequence it covers.
package store
