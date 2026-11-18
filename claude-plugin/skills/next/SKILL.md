---
name: next
description: Pull the next ticket
---

# Next

Run this check, then the command it names:

```
command -v lit >/dev/null || { echo "lit is not installed: install the release, then run /lit:next again" >&2; exit 1; }
lit next
```

`lit next` picks the ticket and prints what to do with it. Follow what it prints.
