Finishing work (lit)

Mark a ticket done when all work is completed: `lit done <issue-id>` closes the ticket (success path) and prints follow-up guidance for capturing what the next agent needs.
Close a ticket without marking done: `lit close <issue-id> --resolution <duplicate|superseded|obsolete|wontfix>` (resolution is REQUIRED — it records why the work was not finished; from any non-closed state). duplicate/superseded redirect to a canonical ticket; obsolete = the need is gone; wontfix = a standing decision not to do it. Reopening clears the resolution. Filter closed work by it: `lit ls --query "resolution:wontfix"`.
Create a follow-up ticket: `lit followup --on <closed-id> --title "..."` — it lands beside the closed ticket, in the same epic. A follow-up of a top-level epic has no epic to land in, so file it as an epic (`--type epic`) or with `lit new --parent <epic-id>`.

Want to change that follow-up guidance, or make it fire only for certain tickets? `lit workflows edit done` scaffolds an editable override — `lit workflows` shows what is active now, and `lit workflows --help` documents the definition format.

**Always** commit your work when you're done.
