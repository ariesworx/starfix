# Moving from bd

starfix imports a [bd (beads)](https://github.com/gastownhall/beads)
backlog and keeps every bd ID, so commits, branches and pull requests that
cite them still resolve. It can also export back to bd's format at any
time.

## Move a backlog

1. Export from bd, in the repository that uses it:

   ```sh
   bd export -o bd.jsonl
   ```

   Export fresh rather than importing a committed `.beads/issues.jsonl`,
   which can lag behind bd's database.

2. Copy `bd.jsonl` to the server. As the `starfix` user, see what the
   import would do. A dry run writes nothing:

   ```console
   $ starfixd import-bd --dry-run bd.jsonl
   warning: type "spike" stored as task, labeled bd-type:spike (acme-7k2.2)
   fix: none needed; this is informational
   warning: memory records are not imported yet (line 12)
   fix: keep them in bd until starfix memory lands (design §13, stage 4)
   would import 10 issues (10 created), 8 deps (8 created), 3 comments (3 created); 0 errors, 17 warnings
   ```

3. Import it:

   ```sh
   starfixd import-bd bd.jsonl
   ```

4. Connect each repository ([`.starfix.yaml`](cli.md#connect-a-repository))
   and set its agents up ([agent guide](agents.md#set-up-an-agent)). Then
   take out bd's own agent instructions and hooks, so agents use one
   tracker.

Run the import as often as you like. It changes nothing the last run got
right, so you can import again while people still use bd, then switch over
when you are ready.

| Option | Does |
|---|---|
| `--dry-run` | Report what would change, and write nothing |
| `--principal NAME` | Record the import's events as NAME (default `import`) |
| `--json` | Print the report as one JSON document |
| `-` as the file | Read the export from stdin |

The import exits 1 if any record failed, and 0 when there are only
warnings.

## What carries over

| bd | starfix |
|---|---|
| IDs, including dotted child IDs such as `bd-a3f.2` | The same IDs. An ID starfix cannot store is reported and its issue skipped, never renamed |
| Title, description, acceptance criteria, design, notes, assignee, owner, priority, dates, close reason, metadata | The same fields (description becomes the body) |
| Types `bug`, `feature`, `task`, `epic`, `chore` | The same types |
| Other types (`spike`, `story`, `decision`, custom) | `task`, labeled `bd-type:<name>` |
| The five core statuses | The same statuses |
| `pinned` | `open`, with the pinned flag |
| `hooked`, and custom statuses | `in_progress` for `hooked`; a custom status becomes `closed` if the issue has a closed date, else `open`. Each is labeled `bd-status:<name>` |
| `parent-child` dependencies | The child's parent |
| `blocks`, `conditional-blocks`, `waits-for`, `related`, `discovered-from`, `duplicates`, `supersedes` | The same dependency types |
| `relates-to` | `related` |
| Labels and comments | The same, with authors and times |

`export-bd` turns the `bd-type:` and `bd-status:` labels back into bd's
types and statuses.

## What is reported instead

Nothing is dropped silently. Each of these produces a warning that names
the affected IDs:

- other dependency types (`replies-to`, `tracks` and the rest), and
  `external:` dependencies;
- memory records, until starfix memory lands (stage 4);
- fields starfix has no column for, such as `external_ref`,
  `estimated_minutes` and `started_at`;
- issues deleted in bd (tombstones), which are skipped;
- labels starfix cannot store, such as one with a space;
- control and bidirectional characters, which starfix refuses
  ([Security](security-model.md#untrusted-text)). They are removed, with a
  `text` warning naming the field, rather than failing the issue.

A dangling reference, a parent cycle or an invalid record is reported with
its IDs, and the rest of the file still imports.

## Rerunning safely

- An issue already stored is overwritten only when the file's `updated_at`
  is later. An issue edited in starfix after the export keeps the edit.
- An overwrite applies whoever holds the issue. One that closes an issue
  someone has started ends their claim and tells their session, as
  `close` does.
- Labels merge. Dependencies and comments are never rewritten.
- Identical rows are left alone.

## Going back

`starfixd export-bd -o back.jsonl` writes the store in bd's JSONL format,
for bd or for a later import. A handoff note exports as a plain comment,
because bd has no comment kinds. An issue's account is not exported,
because bd has no field for it. Importing the file again keeps each
issue's account and reports the issue unchanged.

The full mapping, field by field, is in
[internal/bdimport](../internal/bdimport/doc.go).
