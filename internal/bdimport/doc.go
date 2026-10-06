// Package bdimport moves a bd (beads) backlog into the starfix store and
// back out again, as bd's JSONL export: one issue per line, with its
// labels, outgoing dependencies and comments embedded.
//
// # IDs are kept
//
// bd IDs are preserved unchanged (design §3: "bd IDs survive import
// unchanged"). Commit trailers, branch names, pull requests, documents and
// other trackers already cite them, and a mapping would break every one of
// those references or need a lookup table kept forever. starfix IDs are
// "<prefix>-<suffix>" with any prefix, so bd's own prefixes and dotted
// child IDs (bd-a3f.2) are valid as they are; the configured prefix only
// shapes IDs starfix generates. An ID that is not valid starfix syntax is
// reported and its issue skipped, never renamed.
//
// # What maps where
//
//   - description → body, acceptance_criteria → acceptance; design, notes,
//     assignee, owner, priority, created_by, created_at, updated_at,
//     closed_at, close_reason, due_at, defer_until, pinned, ephemeral (and
//     bd's legacy "wisp"), is_template → template, metadata: kept as is.
//   - status: bd's five core statuses map directly. "pinned" becomes open
//     with the pinned flag. "hooked" becomes in_progress, and a custom
//     status becomes closed if the issue has closed_at, else open; both
//     keep the original as a "bd-status:<name>" label.
//   - issue_type: bug, feature, task, epic and chore map directly. Any
//     other type (spike, story, decision, milestone, a custom type) becomes
//     task with a "bd-type:<name>" label, so nothing is lost and the work
//     stays visible.
//   - parent-child dependencies become the child's parent_id.
//   - relates-to becomes related (bd keeps both as near-synonyms).
//   - blocks, conditional-blocks, waits-for, related, discovered-from,
//     duplicates and supersedes map directly, with their author, time and
//     metadata.
//   - Comments keep their author, text and time. A bd comment ID becomes a
//     16-character ID derived from the issue and the bd ID, so re-importing
//     finds the same comment; an ID already in that form is kept, so a
//     starfix export re-imports unchanged.
//
// Everything else is reported, never dropped silently: other dependency
// types (replies-to, tracks, authored-by and the rest), external:
// dependencies, memory records, and any issue field the store has no column
// for (external_ref, spec_id, estimated_minutes, started_at and so on) each
// produce one warning naming the affected IDs. Derived fields bd computes
// on export (counts, parent, is_blocked) are ignored.
//
// # Order and safety
//
// Issues are written parents first, then every dependency, then comments,
// so a dependency is only added once both ends exist. A dangling reference,
// a parent or blocking cycle, or an invalid record is reported with the
// offending IDs and the rest of the file still imports. Importing the same
// file twice changes nothing the first run got right: identical rows are
// left alone, an issue is overwritten only when the file's updated_at is
// later than the stored one, labels merge, and dependencies and comments
// are never rewritten.
package bdimport
