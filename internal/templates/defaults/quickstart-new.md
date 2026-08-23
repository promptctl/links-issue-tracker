Creating tickets (lit)

Create tickets: `lit new --title "..." --topic <topic> --type task|bug|feature|chore|epic [--parent <parent-id>] [--top]`

<agent-instructions>`--description`: describe what to build and why; leave how to the implementer — write what survives a refactor of the code it concerns.</agent-instructions>
<agent-instructions>`--topic`: Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')</agent-instructions>
<agent-instructions>New tickets append to the bottom of their frame by default — the bottom of their epic's children, or of the top-level order for a ticket with no parent. Filing work records it; promoting it is a separate, deliberate act. Pass `--top` at creation for a ticket that genuinely belongs at the front of the agenda, or `lit rank <id> --top` later. A batch authored in order keeps that order with no flag.</agent-instructions>

Create a follow-up parented to a just-closed ticket: `lit followup --on <closed-id> --title "..."` — a good habit for capturing work you surface as a child ticket while the context is fresh.

Creating (or updating) several tickets at once: `lit import --path <file.yaml>` reads a
multi-document YAML file, one ticket per document — a document without an `id` creates,
one with an `id` updates that existing ticket. Cheaper than N separate `lit new`/`lit
update` invocations for a batch. See `lit import` in docs/cli-reference.md for the full
format (create/update field lists, `parent`/`depends_on` wiring, error behavior).
