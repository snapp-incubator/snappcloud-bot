# Weekly best-practice report: single-instance brokers/caches and drain-blocking PDBs

## Goal

Every Saturday at 00:00 (`schedules.timezone`, Asia/Tehran), the bot:

1. scans every cluster for
   - **single-instance RabbitMQ and Redis**, which go down whenever their node
     is drained, and
   - **PodDisruptionBudgets that block drains** (`maxUnavailable: 0`,
     `minAvailable` ≥ the replica count, overlapping PDBs), and
   - **PDBs that are dangerous**: they let a drain take down more members of a
     quorum system than it can lose (e.g. `minAvailable: 1` on a 3-member
     cluster, which should be `2`), or that are missing entirely on a quorum
     system.

   PDBs that simply have no effect (matching no pods, and so on) are not
   reported;
2. groups the findings by **team**: namespace label `snappcloud.io/team` →
   the `Team` CR → `spec.teamAdmins`;
3. posts a **preview to the cloud-admin channel** and waits;
4. once a cloud-admin **approves**, **emails** the findings as a PDF (SMTP,
   `mail.snapp.cab`): one email per configured group of teams, to the union
   of its teams' admins; one email for every team in no group, to a fixed
   list (`others`); the CTO and the cloud director in cc on all of them.

Nothing goes to a team without a human approving it first.

## Design decisions

### 1. Rules are deterministic code, not the LLM

The findings are facts: a replica count, a PDB's status fields. Having a model
"investigate" them would cost about 40 tool calls per cluster, could invent a
finding, and would word things differently every week, which breaks
comparing one week with the next. The report is plain Go: collect, evaluate
the rules, render. The agent loop is not involved, so neither are the LLM
budget, the review pass or the `unattended()` framing.

### 2. Data comes from openshift-mcp through new, purpose-built tools

The bot has no cluster credentials, and that stays true. `list_resource`
returns only name/namespace/age, caps at 200 items and carries no spec, so it
cannot serve this report. openshift-mcp gets two new read-only tools:

| Tool | Returns |
|---|---|
| `disruption_audit` (cluster-wide, no args) | every PDB: `ns, name, minAvailable, maxUnavailable, unhealthyPodEvictionPolicy, expectedPods, currentHealthy, desiredHealthy, disruptionsAllowed, owners[{kind,name,replicas,images[],operatorKind}], overlapsWith[]`; every data-service instance (brokers, caches, and quorum systems by operator kind or `quorumImages`, so the image list is passed as an argument): `ns, name, engine, kind, replicas, quorum (bool), source (operator\|image), pdbs[]`; `namespaces{ns: {team, env}}`; `notInstalled[]` (CRDs absent on this cluster) |
| `list_teams` | `[{team, admins[], projects[{name, envLabel}]}]` from `team.snappcloud.io/v1alpha1 Team` |

The server does the joins: pod→PDB overlap and PDB→owning workload. Doing
them there means the bot never pulls every pod in the cluster over MCP, and
the response stays compact, a few hundred KB even on teh-1. Pagination
uses Kubernetes `continue` inside the server, so the tool's output is never
silently truncated.

How broker/cache instances are found:
- **Operator CRs**: `rabbitmq.com/v1beta1 RabbitmqCluster` (`spec.replicas`,
  which **defaults to 1**); `redis.redis.opstreelabs.in/v1beta1` `Redis`
  (standalone, so always one instance) and `RedisReplication`
  (`spec.clusterSize`). `RedisCluster` and `RedisSentinel` are not
  single-instance by construction and are skipped.
- **Plain workloads** (Bitnami charts, hand-written StatefulSets): a
  StatefulSet or Deployment whose container image is `*redis*`/`*rabbitmq*`
  (not `*exporter*`), with replicas == 1 and no operator owner. These are
  marked `source: image` so the message can say how they were detected.

RBAC added to `helm/apps/openshift-mcp/templates/crb.yaml` (get/list only):
`rabbitmq.com/rabbitmqclusters`, `redis.redis.opstreelabs.in/{redis,redisreplications,redisclusters,redissentinels}`,
`team.snappcloud.io/teams`. PDBs, workloads and namespaces are already
readable. A CRD that is missing on a cluster (the snappgroup cluster has no
Redis operator) shows up in `notInstalled` and is not treated as an error.

Both tools go into `toolRules` as `clusterAdminOnly`, so through chat only
cluster-admins can use them. That is also useful on its own: an admin can ask
"which PDBs on teh-2 block drains?" in chat.

### 3. Trust model: the bot reads on its own authority, then gates delivery

This is the first feature where the bot reads data **without a user's
scope**: alerts and schedules always run as their owner. This should be
stated plainly in the README. It is acceptable because the bot never releases
that data except through two gates:

- **Preview**: posted only to the configured cloud-admin channel. Approval
  commands work only for callers with `scope.HasClusterWide()`, the same
  test `diagnostics` uses.
- **Delivery**: the preview names every email, its teams and its recipients,
  so approving it approves who gets what. Recipients are **not** checked
  against mcp-authz: an email goes to a group's admins together, to the
  `others` lists and to the cc, none of whom is a single cluster user. (An
  earlier version emailed each admin only the findings in namespaces mcp-authz
  said they could access; grouping replaced it.) A wrong
  `snappcloud.io/team` label puts a namespace in the wrong team's email, so
  the preview is where to catch it.

The direct call path is `brain.CallSystemTool(ctx, cluster, tool, args)`. It
uses the cluster's mux directly and bypasses the enforcer, so it is a separate
method with one caller (the report). It is not something `Answer` can reach,
and the model cannot reach it.

### 4. Approval happens through chat commands, not buttons

Mattermost interactive buttons need the bot to expose an HTTP callback that
the Mattermost server can reach, and they need Mattermost's allow-list for
untrusted internal connections. The bot has neither today. Commands
fit the existing `runCommand` design (whole-message match, normalised).
Buttons can be added later on top of the same store (see Later).

### 5. Re-check at send time

Approval may come hours after the 00:00 scan. When a cloud-admin approves, the
bot **collects again** and sends only findings that are **in both** scans:
- findings fixed since the preview are dropped, so nobody is told about
  something they already fixed;
- findings that are new since the preview are **not** sent, because nobody
  approved them. They will appear in next week's preview.

The thread shows `sent 41 · 3 resolved since preview · 2 new (held for next week)`.

## Rules

Each rule has a stable ID. The IDs are used for ignores, metrics and
week-over-week tracking.

PDB rules use the **status** the disruption controller computes
(`expectedPods`, `desiredHealthy`), not the spec. That way percentages, their
rounding (`minAvailable` rounds up, so `50%` of 1 pod means 1) and
integer-vs-string forms are already resolved, the same way the eviction API
resolves them.

| ID | Condition | Severity | Reported in | Why it matters |
|---|---|---|---|---|
| `pdb-max-unavailable-zero` | `maxUnavailable` is `0` or `0%` | high | all namespaces | No pod can ever be evicted; every drain of its node hangs |
| `pdb-min-available-all` | `minAvailable` set, `expectedPods > 0`, `desiredHealthy >= expectedPods` (e.g. `minAvailable: 1` on 1 replica, `100%`, `3` on 3 replicas) | high | all namespaces | Same as above: zero disruptions allowed **even when all pods are healthy** |
| `pdb-overlap` | a pod is selected by more than one PDB | high | all namespaces | Eviction refuses the pod outright ("more than one PodDisruptionBudget"), so the drain is blocked |
| `pdb-blocked-unhealthy` | `disruptionsAllowed == 0` only because `currentHealthy < desiredHealthy`, and `unhealthyPodEvictionPolicy != AlwaysAllow` | medium | all namespaces | A crashlooping pod pins its node; recommend `unhealthyPodEvictionPolicy: AlwaysAllow` |
| `pdb-below-quorum` | the PDB covers a **quorum system** of N members and, with every pod healthy, allows more disruptions than the system can lose: `expectedPods − desiredHealthy > (N−1)/2` (integer division) | high | production only | A drain, or two drains running in parallel, can evict a majority at once, and the cluster loses quorum. N=3 tolerates 1 (`minAvailable: 1` is wrong, `2` is right); N=5 tolerates 2 |
| `quorum-no-pdb` | a quorum system with N ≥ 2 members whose pods no PDB covers | high | production only | Nothing stops a drain from evicting two members at once (two members on one node, or two drains running in parallel), and the cluster loses quorum. Fix: a PDB with `maxUnavailable: 1` |
| `rabbitmq-single-instance` | RabbitMQ with replicas == 1 | high | production only | Broker down for the whole reschedule + volume reattach on every drain |
| `redis-single-instance` | Redis standalone, or replication/workload with replicas == 1 | high | production only | Same; and data loss if persistence is off |

**Outside production, only drain blockers are reported**: the four `pdb-*`
rules that stop a node from draining (`pdb-max-unavailable-zero`,
`pdb-min-available-all`, `pdb-overlap`, `pdb-blocked-unhealthy`). Those block
cluster maintenance wherever they are. Downtime and quorum risks
(`*-single-instance`, `pdb-below-quorum`, `quorum-no-pdb`) only matter for
production, so a single-instance Redis in staging is not reported, but a PDB
that blocks its drain is. A namespace's environment is its `envLabel` from
the Team CR; a namespace with no env label is treated as production.

**Not reported**: PDBs with no effect. A PDB that matches no pods, has an
empty selector, or allows more disruptions than a stateless workload would
want is a weaker guarantee, not a hazard to drains or to data. A PDB that
matches nothing is reported only if it also overlaps with another PDB.

**What counts as a quorum system** for `pdb-below-quorum` and
`quorum-no-pdb`:
- operator-managed: `RabbitmqCluster`, `RedisCluster` (leader pods),
  `RedisSentinel`;
- by image (config `quorumImages`, default `rabbitmq`, `etcd`, `zookeeper`,
  `kafka`, `consul`, `nats`, `mongo`, `redis-sentinel`, `elasticsearch`).
  Only StatefulSets count, because Deployments are not member sets.

N is the owner's desired replica count, not `expectedPods`, so a member that
is down at scan time does not change the rule's verdict. With N = 2, no
setting is safe: the PDB either blocks drains or allows losing quorum. The
finding says so and recommends 3 members instead of a different PDB value.
The fix in the email is computed: `minAvailable: ⌊N/2⌋+1`, or
`maxUnavailable: 1`.

How the rules combine on one workload: a single-instance broker **with** a
blocking PDB is reported as one finding with both reasons, not two. The text
is "drains of its node are blocked until a cloud admin forces it, which then
takes it down anyway". Outside production, the same workload produces only
the PDB finding.

Out of scope for v1, and easy to add as more rules later: workloads with ≥2
replicas and no PDB, single-replica Deployments in production, missing
topology spread / anti-affinity. Each of these would be noisy across the
whole fleet, so add them only once the first two categories are working well.

### Excluding things

- Config `excludeNamespaceSelectors`: label selectors in `kubectl -l` syntax
  (`key`, `!key`, `key=value`, `key!=value`, `key in (a,b)`,
  `key notin (a,b)`). A namespace matching any of them is left out, for
  platform namespaces, which are the cloud team's own concern. There is no
  name matching; `kubernetes.io/metadata.name=<name>` covers a single
  namespace. disruption_audit returns every namespace's labels for this. A
  namespace whose labels are unknown is never excluded, so a negative selector
  cannot hide it by accident.
- Object annotation `snappcloud.io/best-practice-ignore: "<rule-id>[,<rule-id>]"`
  on the PDB, CR or workload, for teams to mark something as intentional. The
  admin preview **lists the ignored findings with counts** so ignores stay
  visible. **Drain blockers cannot be ignored** (`pdb-max-unavailable-zero`,
  `pdb-min-available-all`, `pdb-overlap`, `pdb-blocked-unhealthy`): a node
  that cannot be drained holds up maintenance for everyone on it. An ignore
  on one is overruled, and the finding says so; `bestPractice.disabledRules`
  is the cloud team's only switch for them.
- Namespaces with no team label: shown in the preview's **"no owner"**
  section and never sent to anyone. This is also a useful report on its own.

## Flow

```
Sat 00:00 ─► collect (all clusters, in parallel, bounded) ─► evaluate rules
                │ cluster failed? → recorded, report marked PARTIAL
                ▼
          Report{id, status: pending, findings, perTeam, recipients}
                ▼  persisted to bestPractice.path (PVC)
          preview → cloud-admin channel (root post + thread)
                ▼
  "report approve <id>"  (cluster-wide caller, in that channel)
                ▼
          re-collect → intersect → route into emails (groups, others)
                ▼
          email + PDF, cc on all  ─► thread: sent / failed / refused, by whom
```

### Preview (cloud-admin channel)

Root post, short:

> **Best-practice report `r7k2m` — Sat 18 Oct, 00:00 Asia/Tehran** — 63
> findings for 21 teams (31 high) · 4 clusters scanned, **teh-2 failed:
> openshift-mcp 503** (partial)
> `report approve r7k2m` · `report reject r7k2m` · expires Mon 00:00

The thread contains:
- an emails table: email · teams · high/medium · to (a group's admins, or
  `others`), with the cc above it; a team with no admins in its Team CR is
  named there (its group's email goes to the cc alone if no team has any);
- a per-team table: team · clusters · high/medium. An address the mail
  server refuses is listed in the delivery summary, so it can be fixed in
  the Team CR or the config;
- a "no owner" table and the ignored-by-annotation counts;
- week-over-week changes: `+9 new · −14 fixed · 40 open ≥3 weeks`.

### Commands (only in the admin channel or a DM, caller must be cluster-wide)

| Command | Effect |
|---|---|
| `report approve <id>` | re-check, then send to every team in the report |
| `report reject <id>` | close it without sending; the thread records who rejected it |
| `report exclude <id> <team>[,<team>]` | remove teams before approving (e.g. a team under migration) |
| `report preview <id> <team>` | show the email that carries that team's findings (to, cc, subject, body, attachment) and the team's findings as the PDF lists them |
| `report run` | build a preview now (testing, or after a fix); same path as the timer |
| `report status` | the last report, its state, the next run |

`approve` is idempotent: once a report is `sending` or `sent`, a second
approve reports who approved it and when. Only one report is pending at a
time. A new Saturday report **supersedes** an unapproved previous one, which
is marked `expired` because its data is a week old. `expireAfter` (default
48h) expires a pending report early.

### Report email

One email per **mailing**, from `bestPractice.recipients`:

```yaml
recipients:
  groups: [[snapp-box, snapp-box-ai], [snapppay]]
  others: [tech-ems@snapp.cab, tech-specialists@snapp.cab, tech-directors@snapp.cab]
  cc: [qasim.zaidi@snapp.cab, ali.borhani@snapp.cab]
```

- each group with findings: one email to the union of its teams' admins;
- every team in no group: one email to `others` (so snapppay's teams must be
  in a group, or they land with Snapp's tech leads);
- `cc` on every email. A mailing with nobody in To goes to the cc alone; one
  with nobody at all is not sent, and the summary says so.

The body is short: the date, what the findings risk, a team · high · medium
table, and the footer. The findings themselves, each with its detail, fix,
docs link and rule, are in the attached PDF (`snappcloud-best-practice-
<date>.pdf`, A4, rendered with `codeberg.org/go-pdf/fpdf`'s core fonts, so
text outside Windows-1252 becomes dots). The body is `multipart/alternative`
(plain text and HTML, built by `html/template` so names from cluster objects
are escaped) inside `multipart/mixed` with the PDF. Subject: `SnappCloud
best-practice check: 5 findings for snapp-box, snapp-box-ai (2 high)`, or
"for 37 teams" beyond three. `Auto-Submitted: auto-generated` keeps
out-of-office replies away; `mail.replyTo`, when set, is where questions go.

The admin channel stays the control room: preview, approval, and the
delivery summary are still Mattermost posts.

Each finding in the PDF:

```
HIGH   teh-1 / dispatching-prod
PDB api-pdb blocks node drains
`minAvailable: 1` on a workload with 1 replica: its node cannot be drained.
Fix: `maxUnavailable: 1`, or run ≥2 replicas.
Docs
rule pdb-min-available-all (cannot be ignored)
```

The rule text and fixes are templates in Go, one per rule ID, with a
`docsURL` per rule from config (user-docs pages). Nothing in the message is
written by a model.

## Code layout

**openshift-mcp** (separate PR, merged first):
- `internal/mcp/tools.go`, `handlers.go`: `disruption_audit`, `list_teams`.
- `internal/k8s`: paged list helpers; dynamic client for the CRDs, with
  "not found → notInstalled".
- chart: RBAC rules above.

**snappcloud-bot**:
- `internal/bestpractice/` (new, no Mattermost or brain imports, so it is
  testable on its own)
  - `inventory.go`: tool result types; `Collect(ctx, caller, clusters)` runs
    the clusters in parallel, collecting per-cluster errors instead of
    aborting.
  - `rules.go`: `Evaluate(inv, cfg) []Finding`, pure functions, one per rule
    ID, table-tested against fixture JSON.
  - `report.go`: `Report`, `Finding{ID: cluster/ns/kind/name/rule}`,
    `Intersect`, week-over-week diff.
  - `render.go`: preview, delivery summary; `routing.go`: groups → mailings;
    `email.go`: the email body (text and HTML); `pdf.go`: the PDF.
  - `store.go`: pending/last reports + finding first-seen dates, using the
    same JSON-on-PVC, tmp-then-rename pattern as `schedule.Store`.
  - `runner.go`: the weekly timer. It computes the next slot with
    `schedule`'s `nextWeekly` every time (not `+7×24h`), and **catches up**
    after downtime: if the bot was down at 00:00 and comes back within
    `catchUp` (default 12h), it runs once; if it comes back later, it skips
    the week and logs that. The last fired slot is persisted, so a restart
    does not run the report twice.
- `internal/brain/system.go`: `CallSystemTool`, and `ClustersWithTool(name)`,
  which finds the server that carries `disruption_audit` with no extra config.
- `internal/mail`: an SMTP sender (one connection per message, To and Cc,
  attachments, STARTTLS when offered or TLS on 465, PLAIN or LOGIN auth,
  never a password in clear). An address that is invalid or gets a 5xx to
  `RCPT TO` is skipped and reported, and the rest still get the message;
  only when every address fails is it `ErrRejected`.
- `internal/bot/report.go`: the commands, the approval gate, delivery
  (route → render body and PDF → email, paced), and thread updates. A
  cluster the approval cannot re-scan is named, with the reason, in the
  delivery summary.
- `internal/bot/commands.go`: `help` lists `report …` only for cluster-wide
  callers with the feature on.
- `internal/config/config.go` + `config.example.yaml`: a `bestPractice`
  section.
- `internal/metrics`: `report_runs_total{outcome}`,
  `report_findings{rule,severity}`, `report_deliveries_total{outcome}`
  (`sent|rejected|unrouted|send_error`),
  `report_collect_failures_total{cluster}`.
- README: a "Best-practice report" section, including the trust-model
  paragraph from §3.

### Config

```yaml
bestPractice:
  enabled: false
  path: /data/best-practice.json
  schedule: "every saturday at 00:00"   # parsed by internal/schedule, in schedules.timezone
  catchUp: 12h
  expireAfter: 48h
  adminChannel: <channel id>            # preview + approvals live here
  timeout: 5m                           # one collection, all clusters
  excludeNamespaceSelectors: ["snappcloud.io/team=snappcloud"]
  excludeTeams: []
  disabledRules: []                     # e.g. [pdb-blocked-unhealthy]
  quorumImages: [rabbitmq, etcd, zookeeper, kafka, consul, nats, mongo, redis-sentinel, elasticsearch]
  docs:
    pdb: https://docs.snappcloud.io/docs/...
    quorum: https://docs.snappcloud.io/docs/...
    rabbitmq: https://docs.snappcloud.io/docs/...
    redis: https://docs.snappcloud.io/docs/...
  recipients:
    groups: [[snapp-box, snapp-box-ai], [snapppay]]
    others: [tech-ems@snapp.cab, tech-specialists@snapp.cab, tech-directors@snapp.cab]
    cc: [qasim.zaidi@snapp.cab, ali.borhani@snapp.cab]
```

## Testing

- `rules_test.go`: one table case per edge: `minAvailable: 1`/1 replica,
  `50%`/1 pod (rounds to blocked), `100%`, `maxUnavailable: "0%"`,
  `maxUnavailable: 1`/1 replica (fine), a PDB matching no pods (not
  reported), overlap, annotation ignore, broker with blocking PDB (one
  combined finding), and per environment: staging keeps the four drain-blocking
`pdb-*` rules and drops the single-instance and quorum rules (a staging
single-instance broker with a blocking PDB yields only the PDB finding), and
a namespace with no env label counts as production. For quorum:
  N=3 `minAvailable: 1` (reported) / `2` (fine) / `maxUnavailable: 1` (fine) /
  `maxUnavailable: 2` (reported), N=5 `minAvailable: 2` (reported) / `3`
  (fine), N=3 with one member down at scan time (verdict unchanged), N=2
  (3-member recommendation), a stateless Deployment with `minAvailable: 1`
  on 3 replicas (not reported), a 3-member StatefulSet with no PDB
  (`quorum-no-pdb`), and a 1-member one with no PDB (single-instance rule
  only, not `quorum-no-pdb`).
- `report_test.go`: intersect (resolved/new), diff, supersede/expire.
- `runner_test.go`: Saturday slot in Asia/Tehran, catch-up inside and outside
  the window, no double fire after restart (fake clock, as `schedule` tests do).
- `bot/report_test.go`: non-admin approve refused; approve twice; a group's
  email reaches its admins and the rest go to `others`, both with the cc,
  each holding only its own teams; nobody to send to is reported.
- openshift-mcp: fake dynamic client fixtures for each operator kind and a
  missing CRD.
- Dry run before enabling: `report run` on staging clusters, then compare the
  preview with `oc get pdb -A` by hand once.

## Rollout

1. openshift-mcp release with the tools and RBAC → sync to all regions.
2. Bot release with `bestPractice.enabled: false`, then turn it on with
   `report run` only (no timer) by setting `schedule: ""`. Have cloud admins
   read two or three previews and tune `excludeNamespaceSelectors` and the rule
   texts.
3. Write the user-docs pages the emails link to, and announce to team admins
   before the first email arrives, so the first email is expected.
4. Enable the Saturday schedule.

## Later

- Mattermost buttons (Approve / Reject) through an action endpoint on the
  API port, using the same store and the same approval gate.
- More rules (no PDB on ≥2 replicas, no anti-affinity on single-zone
  StatefulSets, PDB on a Deployment with HPA min 1).
- Escalation: a finding open for ≥4 weeks is shown separately in the admin
  preview.
- Expose the latest report through MCP (`/mcp`) so other agents can query it.

## Open questions

1. **Delivery target**: decided: one email per group of teams with a PDF,
   the rest to `others`, cc the CTO and the cloud director.
2. **Approvers**: is "cluster-wide on any cluster" the right test, or should
   it be a named list or group (`approvers: [...]`)?
3. **Language**: English like the rest of the bot, or Persian for the team
   email?
4. **Plain-workload detection by image**: include it in v1, or only report
   operator-managed instances at first, to keep the first few reports
   free of false positives? The same question applies to `quorumImages`.

## Mattermost setup

1. A **private channel** for the previews and approvals (e.g.
   `cloud-best-practice`), with the bot account and the cloud admins as
   members. The bot must be a member to post there and to see the
   `report …` commands. Its **channel ID** goes in
   `bestPractice.adminChannel` (channel menu → View Info, or
   `GET /api/v4/teams/name/<team>/channels/name/<channel>`).
No slash command, webhook, or new permission is needed. Approvals are
@-mentions, which go through the existing WebSocket.

## Mail setup

1. A mailbox (or relay permission) on `mail.snapp.cab` for the sender in
   `mail.from`. Its username goes in `mail.username`, its password in the
   Secret as `MAIL_PASSWORD` (`mail.passwordEnv`).
2. The pod can reach `mail.snapp.cab:587` (or 465); egress NetworkPolicy may
   need a rule.
3. If the server's certificate is signed by an internal CA, the image must
   trust it (mount it and set `SSL_CERT_FILE`, or add it to the image).
4. Optionally a `mail.replyTo` the cloud team reads, so questions reach
   people.
5. Exchange may cap recipients or message size: the largest email is the
   `others` one (many teams, one PDF, three lists and the cc).
