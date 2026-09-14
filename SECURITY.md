# Security policy

## Reporting a vulnerability

Please report security issues privately to the SnappCloud team rather than
opening a public issue. Use GitHub's [private vulnerability
reporting](https://github.com/snapp-incubator/snappcloud-bot/security/advisories/new)
on this repository.

Include what you found, how to reproduce it, and what an attacker could reach
with it. We will confirm receipt and keep you updated while we work on a fix.

## What this project treats as a vulnerability

The bot's entire purpose is to answer questions about clusters **without**
widening anyone's access, so anything that breaks that is a security issue, not
a bug:

- A caller receiving data from a namespace or cluster they are not authorized
  for — including through an aggregate, a metrics query, or a tool result that
  should have been filtered.
- A way to reach a tool that the configuration does not permit, or to call a
  write operation through a read-only server.
- Credentials appearing in an answer, a log line, or an error message.
- Anything that lets the model's output influence authorization, rather than
  authorization constraining what the model can see.

Bypassing a *rate limit* or a *size budget* is a bug; bypassing a namespace
scope is a vulnerability.

## Supported versions

The latest release is supported. Fixes are published as a new tag.
