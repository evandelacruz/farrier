# Branch: c/exciting-tesla-b7o4kt

Keeps requirement IDs out of anything an operator reads at runtime.

Requirement IDs (`UP-006`, `KEY-003`, …) are build-time vocabulary: they
belong in code comments, `docs/`, commit messages, and PR bodies, where
agents and reviewers need them. They had leaked into event details, error
messages, and CLI flag help, where they tell the reader nothing.

Each message is rewritten rather than truncated — where the ID was carrying
the justification, the sentence now carries it. A `go/parser` AST guard over
`internal/` and `cmd/` fails the build if an ID reappears in a string literal.

Not a new requirement, so no `docs/status.json` entry; the rule is recorded
in `CLAUDE.md`.

This file exists for conductor tracking and can be deleted once the PR merges.
