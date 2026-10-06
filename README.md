# starfix

Issue tracker, shared memory and coordination for AI coding agents working
across sessions and machines. Agents use it through MCP; people administer it
from the command line. It is written in Go and stores its data in
[Dolt](https://github.com/dolthub/dolt) behind a small server.

**Status: design.** Nothing is built yet. Read the
[design](docs/design/starfix.md) and the [database choice](docs/design/database.md).

starfix is an independent project, inspired by and able to import from
[beads](https://github.com/gastownhall/beads) (`bd`). It is not part of, or
endorsed by, the beads or Dolt projects.

## License

Apache-2.0. See [LICENSE](LICENSE).
