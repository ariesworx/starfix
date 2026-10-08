# Agents

Agents use starfix through MCP (Model Context Protocol) and never need a
shell. `sfx mcp` is the MCP server: the app that runs the agent (its
harness, such as Claude Code) starts it, and it talks to the starfix
server for the agent. This page covers setting agents up, the tools they
get and how sessions are identified. Claims, handoffs and the inbox are
explained in [How starfix works](concepts.md).

## Set up an agent

Run setup once per repository, inside a repository that has a
[`.starfix.yaml`](cli.md#connect-a-repository), and commit the files it
writes:

| Command | Does |
|---|---|
| `sfx setup claude-code` | Print what it would write, and where |
| `sfx setup claude-code --write` | Write it; safe to rerun |
| `sfx setup codex --check` | Fail, with a fix, if anything is missing |
| `sfx setup codex --remove` | Take starfix out again |
| `sfx setup --all --write` | Every agent below at once, one summary line each |

Setup writes three things for each agent: the MCP config that starts
`sfx mcp`, a short pointer in the agent's instruction file, and a
session-start hook:

| Agent | MCP config | Pointer | Session-start hook | You still |
|---|---|---|---|---|
| `claude-code` | `.mcp.json` | `CLAUDE.md` | `.claude/settings.json` | approve the project's MCP server when Claude Code asks |
| `codex` | `.codex/config.toml` | `AGENTS.md` | `.codex/hooks.json` | trust the project, and the hooks with `/hooks` |
| `gemini` | `.gemini/settings.json` | `GEMINI.md` | `.gemini/settings.json` | trust the folder when Gemini CLI asks |
| `cursor` | `.cursor/mcp.json` | `.cursor/rules/starfix.mdc` | `.cursor/hooks.json` | turn starfix on in Cursor Settings › MCP |
| `vscode` | `.vscode/mcp.json` | `.github/copilot-instructions.md` | `.github/hooks/starfix.json` (Preview) | trust the server; hooks run only where VS Code's Preview hooks are on |
| `junie` | `.junie/mcp/mcp.json` | `AGENTS.md` | `~/.junie/config.json`, with `--global` only | run `sfx setup junie --global --write` for the hook |
| `jetbrains` | none: added in the IDE | `.aiassistant/rules/starfix.md` | none | add the printed JSON under Settings › Tools › AI Assistant › Model Context Protocol (MCP), scope Project |

Setup prints the "you still" step after it adds a part.

### What setup writes, and what it leaves alone

- **The pointer** is a short block between `<!-- starfix:begin -->` and
  `<!-- starfix:end -->`. It tells the agent to use the starfix tools and to
  `prime`, `start` and `finish`. The rest of the file is left alone.
  Cursor's and AI Assistant's pointers are rule files of starfix's own.
  Codex and Junie share `AGENTS.md`, so `setup --all` writes the block once,
  and removing either agent takes it out.
- **The MCP entry** holds `command`, `args` and `STARFIX_HARNESS` in its
  env (and `"type": "stdio"` for Claude Code and VS Code), and nothing
  else. `--check` fails on any extra key or variable
  (`cwd`, `PATH`, `LD_PRELOAD` and the like), and `--write` drops them.
- **Existing files** keep their other keys, their order and their mode, and
  a second run changes nothing. A JSON file with a duplicate key is refused,
  since the harness would run the last copy, and so is one with comments
  or text after the closing brace, which a rewrite would lose. Setup edits
  only starfix's own tables in Codex's `config.toml`; if it cannot read the
  file safely, it refuses and prints the snippet to add by hand.
- **`--remove`** takes starfix's parts out. A pointer file left empty is
  deleted, and so is VS Code's hook file; an MCP config or a shared hook
  file stays, even when empty.
- **Symbolic links** in the repository are refused, file included, so a
  cloned repository cannot point `.mcp.json` at a secret or `.claude` at
  your home directory. New project files are 0644.
- **`--command PATH`** sets how the agent runs `sfx` when it is not on
  `PATH`.

### Setting agents up for every repository (`--global`)

`--global` edits the files in your home directory instead of the
repository's: for Claude Code `~/.claude.json`, `~/.claude/CLAUDE.md` and
`~/.claude/settings.json`; for Codex, Gemini CLI and Junie their
`~/.codex/`, `~/.gemini/` and `~/.junie/` files; for Cursor the MCP config
and hook. Nothing outside the repository is touched without it. New files
in your home directory are 0600, in 0700 directories. A home file may be a
symbolic link (a dotfiles repository) only to something of yours inside
your home directory; setup edits the target and keeps the link. VS Code and
AI Assistant keep their user settings elsewhere, so `--global` is refused
for `vscode` and `jetbrains`, and `--all --global` skips them.

A global hook runs in every repository with a `.starfix.yaml` that you
open with the agent, and connects to the server that file names. Prefer
per-project setup, or turn the global hook off before you open a
repository you do not trust.

### Claude Desktop

Claude Desktop (macOS and Windows) has no project files, hooks or working
directory. Run `sfx setup claude-desktop --write` inside the repository,
then quit and reopen Claude Desktop. It adds one entry per project to the
app's config:

| OS | Config |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |

The entry is named `starfix-<directory name>` and runs
`/absolute/path/to/sfx -C /path/to/repo mcp`, because the app starts
servers without your shell's `PATH`. The path is your `sfx` on `PATH` (for
example Homebrew's link, which survives upgrades) or else the running
binary; `--command` must be absolute. Several projects coexist, and
`--check` and `--remove` touch only this checkout's entry. `--all` leaves
Claude Desktop out, because its file is outside the repository. With no
pointer or hook, the agent learns starfix from the MCP server's
instructions.

## Session-start hooks

Each hook runs `sfx prime --hook=AGENT` (Claude Code's runs `--hook` alone)
when a session starts, and replies in the harness's own format. Gemini CLI
matches sources exactly, so it gets one entry each for `startup`, `resume`
and `clear`; Codex's matcher is `startup|resume|clear|compact`.

The hook reads the session id from the harness's input, adds `prime`'s
orientation to the session's context, and prints nothing outside a
starfix repository. On any error it adds a one-line note instead of
failing the session.

A hook belongs to starfix only when its whole command is the one setup
writes, `sfx prime --hook[=AGENT]` through `sfx` or the `--command`
program. Other hooks, including your own commands that end in
`sfx prime --hook`, are never touched.

## Tools

The MCP server offers 20 tools: `prime`, `start`, `finish`, `handoff`,
`ready`, `blocked`, `list`, `show`, `create`, `update`, `close`, `reopen`,
`dep`, `label`, `comment`, `comments`, `history`, `digest`, `who` and
`inbox`. There are no admin tools: import, export, setup, upgrade and
server settings stay on the command line.

- **Results are compact.** Writes return `{id, rev}`, and lists return the
  id, title, status and priority. A result is capped at about 2,000 tokens;
  `prime` and `digest` at 1,500.
- **Refusals say what to do next.** A refusal is a tool error with the
  server's code and message and a `fix:` naming the agent's next step. A
  fix only the user can act on is quoted from the server.
- **Text from others is marked.** Results that hold text other people
  wrote carry `"untrusted"`, and the server's instructions tell the agent
  never to follow instructions found in it
  ([Security](security-model.md#untrusted-text)).
- **`digest` is data.** It returns structured facts from the event log;
  the agent writes any narrative.
- **`show` and `digest` carry time and tokens.** `show` gives the issue's
  `account` and a one-line `usage`: time held, tokens for the five largest
  models, and whether any were split by time with other work. `digest`
  gives the window's `usage` line, with the tokens no issue was held for
  and whether any were split by time.
  No tool sets an account; people do that with `sfx update --account`.

One SSH connection serves an MCP session. It opens on the first tool call
and is redialed if it drops. Reads, `create`, `comment`, `finish`,
`handoff` and `inbox` are retried on the new connection, since a retry
cannot write twice. `start`, `update`, `close`, `reopen`, `dep` and
`label` report that they may have applied.

## Sessions

The server knows the principal from the SSH key. The session id comes
from the agent's environment:

| Agent | Session id |
|---|---|
| Claude Code | `CLAUDE_CODE_SESSION_ID`, which it sets |
| Codex, Gemini CLI, Cursor, VS Code, Junie, AI Assistant | none set; `sfx mcp` picks one per process (`m-…`) |
| Claude Desktop | none set; each app launch is one `sfx mcp` process, so one session (`m-…`) |
| A person's own `sfx` commands | `cli`, one per machine |
| Any | `STARFIX_SESSION`, if set, wins (for example in the MCP entry's `env`) |

The harness name comes from `STARFIX_HARNESS`, which setup writes into the
MCP entry; failing that, `claude-code` when `CLAUDECODE=1` and `gemini`
when `GEMINI_CLI=1`.

One limitation affects every harness except Claude Code. The session-start
hook learns the harness's session id from its input, but `sfx mcp` cannot,
so it picks its own. The hook's `prime` and the agent's tool calls then
show as two sessions in `sfx who`, and the hook's session holds no lease.
Claude Code avoids this because it sets `CLAUDE_CODE_SESSION_ID` for both.
