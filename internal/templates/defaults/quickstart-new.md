Creating tickets (lit)

Create tickets: `lit new --title "..." --topic <topic> --type task|bug|feature|chore|epic [--parent <parent-id>] [--bottom]`

<agent-instructions>`--topic`: Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')</agent-instructions>
<agent-instructions>New tickets are ranked to the TOP of the order by default (fresh work surfaces first). Pass `--bottom` to append at the bottom instead — use it when authoring a batch in order so creation order is preserved.</agent-instructions>

Create a follow-up parented to a just-closed ticket: `lit followup --on <closed-id> --title "..."` (ALWAYS capture work surfaced as a child ticket while context is fresh)
