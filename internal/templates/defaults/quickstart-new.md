Creating tickets (lit)

Create tickets: `lit new --title "..." --topic <topic> --type task|bug|feature|chore|epic --parent <epic-id> [--top]`

<agent-instructions>Every ticket lives inside an epic: `--parent` names the epic a task, bug, feature or chore belongs to and is required for those types. Only an epic may be top-level (`--type epic` with no `--parent`), and an epic may sit inside another epic. There is no loose ticket: file the epic first when the work has none, and `lit ls --type epic` lists the ones that exist.</agent-instructions>

<agent-instructions>`--description`: describe what to build and why; leave how to the implementer — write what survives a refactor of the code it concerns. A description longer than a line goes in a file passed as `--description-file <path>` (or `-` to read stdin), so no shell quoting touches it.</agent-instructions>
<agent-instructions>`--topic`: Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')</agent-instructions>
<agent-instructions>New tickets append to the bottom of their frame by default — the bottom of their epic's children, or of the top-level order for an epic with no parent. Filing work records it; promoting it is a separate, deliberate act. Pass `--top` at creation for a ticket that genuinely belongs ahead of its siblings — first among its epic's children, or first in the top-level order for a top-level epic; `lit rank <id> --top` does the same later. A batch authored in order keeps that order with no flag.</agent-instructions>

Creating (or updating) several tickets at once: `lit import --path <file.yaml>` reads a
multi-document YAML file, one ticket per document — a document without an `id` creates,
one with an `id` updates that existing ticket. Cheaper than N separate `lit new`/`lit
update` invocations for a batch. Run `lit import --help` for the full format
(create/update field lists, `parent`/`blocked_by` wiring, error behavior).
