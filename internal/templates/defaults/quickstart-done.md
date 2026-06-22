Finishing work (lit)

Mark a ticket done when all work is completed: `lit done <issue-id>` prints pre-guidance with the exact apply command to run next (success path; only from in_progress).
Close a ticket without marking done: `lit close <issue-id> --resolution <duplicate|superseded|obsolete|wontfix>` (resolution is REQUIRED — it records why the work was not finished; from any non-closed state). duplicate/superseded redirect to a canonical ticket; obsolete = the need is gone; wontfix = a standing decision not to do it. Reopening clears the resolution. Filter closed work by it: `lit ls --query "resolution:wontfix"`.
Create a follow-up ticket: `lit followup --on <closed-id> --title "..."` (ALWAYS capture work surfaced as a child ticket while context is fresh).

**Always** commit your work when you're done.
