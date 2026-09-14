**What this changes, and why**

<!-- The diff says what. Explain the constraint you were working under, or the
failure that prompted this — that is what a future reader cannot recover. -->

**Does it touch authorization?**

<!-- Argument enforcement, result filtering, PromQL pinning, tool allow-lists,
cluster-admin gating. If yes: which control, and what test demonstrates it still
holds? A test that passes before your change is not testing your change. -->

**Checklist**

- [ ] `make test` (race detector) and `make lint` pass
- [ ] New behaviour has a test that fails without the change
- [ ] User-facing text says what the bot actually does now
