---
name: Bug report
about: Something the bot did wrong
labels: bug
---

**What happened, and what you expected instead**

**How to reproduce**
The question or alert that triggered it, if you can share it. Redact namespace
and cluster names if they are sensitive — the shape of the query usually matters
more than its contents.

**Logs**
The bot logs a request id on every turn; including the lines for one `req=` is
far more useful than a single line. Redact before pasting.

**Version**
The image tag, and whether the answer came from Mattermost, the HTTP API, a
schedule, or an alert channel — those paths differ.
