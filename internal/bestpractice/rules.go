package bestpractice

import (
	"fmt"
	"sort"
	"strings"
)

// Rule IDs are stable: they are what teams put in the ignore annotation, what
// metrics are labelled with, and what week-over-week tracking keys on.
const (
	RuleMaxUnavailableZero = "pdb-max-unavailable-zero"
	RuleMinAvailableAll    = "pdb-min-available-all"
	RuleOverlap            = "pdb-overlap"
	RuleBlockedUnhealthy   = "pdb-blocked-unhealthy"
	RuleBelowQuorum        = "pdb-below-quorum"
	RuleQuorumNoPDB        = "quorum-no-pdb"
	RuleRabbitSingle       = "rabbitmq-single-instance"
	RuleRedisSingle        = "redis-single-instance"
)

// Rules lists every rule ID, for validation and metrics.
var Rules = []string{
	RuleMaxUnavailableZero, RuleMinAvailableAll, RuleOverlap, RuleBlockedUnhealthy,
	RuleBelowQuorum, RuleQuorumNoPDB, RuleRabbitSingle, RuleRedisSingle,
}

// Topic groups rules for documentation links.
func Topic(rule string) string {
	switch rule {
	case RuleBelowQuorum, RuleQuorumNoPDB:
		return "quorum"
	case RuleRabbitSingle:
		return "rabbitmq"
	case RuleRedisSingle:
		return "redis"
	}
	return "pdb"
}

// AnnotationIgnore is the annotation teams put on a PDB, CR or workload to
// mark a finding as intentional. openshift-mcp passes its value through. It
// does not apply to drain blockers.
const AnnotationIgnore = "snappcloud.io/best-practice-ignore"

// DrainBlocker reports whether a rule finds a PDB that stops node drains.
// Those cannot be ignored: a node that cannot be drained holds up maintenance
// for every workload on it, whatever its own team intends.
func DrainBlocker(rule string) bool {
	switch rule {
	case RuleMaxUnavailableZero, RuleMinAvailableAll, RuleOverlap, RuleBlockedUnhealthy:
		return true
	}
	return false
}

// Severity orders findings within a message.
type Severity string

const (
	High   Severity = "high"
	Medium Severity = "medium"
)

// Finding is one problem on one object.
type Finding struct {
	Rule      string   `json:"rule"`
	Severity  Severity `json:"severity"`
	Cluster   string   `json:"cluster"`
	Namespace string   `json:"ns"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	// Other is the second PDB of an overlapping pair.
	Other  string `json:"other,omitempty"`
	Team   string `json:"team,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// Key identifies a finding across scans.
func (f Finding) Key() string {
	k := f.Cluster + "/" + f.Namespace + "/" + f.Kind + "/" + f.Name + "/" + f.Rule
	if f.Other != "" {
		k += "/" + f.Other
	}
	return k
}

// Options tunes evaluation.
type Options struct {
	// ExcludeNamespaces leaves out every namespace matching any of these
	// label selectors.
	ExcludeNamespaces []Selector
	ExcludeTeams      []string
	DisabledRules     []string
}

// Result is an evaluation: the findings, and how many were ignored by
// annotation, per rule, so ignores stay visible to the cloud team.
type Result struct {
	Findings []Finding
	Ignored  map[string]int
}

// Evaluate applies every enabled rule to the inventory.
func Evaluate(inv Inventory, o Options) Result {
	e := evaluator{opts: o, disabled: toSet(o.DisabledRules), teams: toSet(o.ExcludeTeams),
		res: Result{Ignored: map[string]int{}}}
	for _, c := range inv.Clusters {
		e.cluster(c)
	}
	sortFindings(e.res.Findings)
	return e.res
}

type evaluator struct {
	opts     Options
	disabled map[string]bool
	teams    map[string]bool
	res      Result
}

// clusterEval holds one cluster's lookups while its rules run.
type clusterEval struct {
	*evaluator
	ci   ClusterInventory
	pdbs map[string]*PDB // ns/name
	// out is this cluster's findings, before the merge of single-instance
	// findings with the drain blockers on the same instance.
	out []Finding
}

func (e *evaluator) cluster(ci ClusterInventory) {
	ce := &clusterEval{evaluator: e, ci: ci, pdbs: map[string]*PDB{}}
	for i := range ci.Audit.PDBs {
		p := &ci.Audit.PDBs[i]
		ce.pdbs[p.Namespace+"/"+p.Name] = p
	}
	for i := range ci.Audit.PDBs {
		ce.pdbRules(&ci.Audit.PDBs[i])
	}
	for i := range ci.Audit.Instances {
		ce.instanceRules(&ci.Audit.Instances[i])
	}
	e.res.Findings = append(e.res.Findings, ce.out...)
}

// skipped reports whether a namespace is out of the report entirely. A
// namespace whose labels are unknown is never excluded by a selector: a
// negative one (`!key`, `notin`) would otherwise match it and hide a real
// finding.
func (ce *clusterEval) skipped(ns string) bool {
	owner := ce.ci.Audit.Namespaces[ns]
	if owner.Labels != nil {
		for _, sel := range ce.opts.ExcludeNamespaces {
			if sel.Matches(owner.Labels) {
				return true
			}
		}
	}
	return ce.teams[owner.Team]
}

// production reports whether a namespace is production. A namespace with no
// environment label is treated as production: guessing staging would hide a
// real risk, guessing production costs one extra line.
func (ce *clusterEval) production(ns string) bool {
	switch strings.ToLower(ce.ci.Audit.Namespaces[ns].Env) {
	case "", "production", "prod":
		return true
	}
	return false
}

// add records a finding unless its rule is disabled or the object ignores it.
// A drain blocker is recorded even when ignored, and says so: the team that
// annotated it believes it silenced.
func (ce *clusterEval) add(f Finding, ignores ...string) {
	if ce.disabled[f.Rule] || ce.skipped(f.Namespace) {
		return
	}
	for _, ig := range ignores {
		if !ignoresRule(ig, f.Rule) {
			continue
		}
		if DrainBlocker(f.Rule) {
			f.Detail += fmt.Sprintf(" The `%s` annotation does not apply: a PDB that blocks drains must be fixed.",
				AnnotationIgnore)
			break
		}
		ce.res.Ignored[f.Rule]++
		return
	}
	f.Cluster = ce.ci.Cluster
	f.Team = ce.ci.Audit.Namespaces[f.Namespace].Team
	ce.out = append(ce.out, f)
}

// ignoresRule reads an ignore annotation: comma-separated rule IDs, or "*".
func ignoresRule(annotation, rule string) bool {
	for _, r := range strings.Split(annotation, ",") {
		if r = strings.TrimSpace(r); r == rule || r == "*" {
			return true
		}
	}
	return false
}

// pdbIgnores are the annotations that can silence a finding on a PDB: its own,
// and its owning workloads'.
func pdbIgnores(p *PDB) []string {
	out := []string{p.Ignore}
	for _, o := range p.Owners {
		out = append(out, o.Ignore)
	}
	return out
}

// headroom is how many pods the PDB lets go when every pod is healthy.
func (p *PDB) headroom() int { return p.ExpectedPods - p.DesiredHealthy }

// blocksDrains reports whether the PDB allows no eviction even when every pod
// it covers is healthy.
func (p *PDB) blocksDrains() bool { return p.ExpectedPods > 0 && p.headroom() <= 0 }

func (ce *clusterEval) pdbRules(p *PDB) {
	ig := pdbIgnores(p)
	owners := ownersText(p.Owners)

	// The drain blockers. The status the disruption controller computed is
	// used, not the spec, so percentages and their rounding are resolved
	// exactly the way eviction resolves them.
	if p.blocksDrains() {
		f := Finding{Severity: High, Namespace: p.Namespace, Kind: "PodDisruptionBudget", Name: p.Name,
			Title: fmt.Sprintf("PDB `%s` allows no evictions", p.Name)}
		switch {
		case p.MaxUnavailable != nil:
			f.Rule = RuleMaxUnavailableZero
			f.Detail = fmt.Sprintf("`maxUnavailable: %s` on %s: no pod it covers can ever be evicted, so draining "+
				"a node that runs one of them hangs until a cloud admin forces it.", p.MaxUnavailable, owners)
			f.Fix = "Set `maxUnavailable: 1`."
		default:
			f.Rule = RuleMinAvailableAll
			spec := "minAvailable"
			if p.MinAvailable != nil {
				spec = "`minAvailable: " + p.MinAvailable.String() + "`"
			}
			f.Detail = fmt.Sprintf("%s on %s (%s): even with every pod healthy it allows no evictions, so draining "+
				"a node that runs one of them hangs until a cloud admin forces it.", spec, owners, pods(p.ExpectedPods))
			if p.ExpectedPods == 1 {
				f.Fix = "Run at least 2 replicas and keep `minAvailable: 1`, or use `maxUnavailable: 1` if a short outage is acceptable."
			} else {
				f.Fix = fmt.Sprintf("Allow at least one disruption: `maxUnavailable: 1`, or `minAvailable: %d`.", p.ExpectedPods-1)
			}
		}
		ce.add(f, ig...)
	}

	// A pod in two PDBs cannot be evicted at all. One finding per pair, on the
	// first name, so the pair is not reported twice.
	for _, other := range p.OverlapsWith {
		if p.Name >= other {
			continue
		}
		oig := ig
		if o, ok := ce.pdbs[p.Namespace+"/"+other]; ok {
			oig = append(append([]string{}, ig...), pdbIgnores(o)...)
		}
		ce.add(Finding{Rule: RuleOverlap, Severity: High, Namespace: p.Namespace, Kind: "PodDisruptionBudget",
			Name: p.Name, Other: other,
			Title: fmt.Sprintf("PDBs `%s` and `%s` select the same pods", p.Name, other),
			Detail: "Eviction refuses a pod covered by more than one PodDisruptionBudget, so a node running one of " +
				"these pods cannot be drained until a cloud admin forces it.",
			Fix: "Keep one PDB per workload: narrow the selectors so each pod matches exactly one, or delete the duplicate.",
		}, oig...)
	}

	// Healthy enough to allow a disruption, but not right now: the unhealthy
	// pods are not evictable either unless the policy says so.
	if p.headroom() > 0 && p.DisruptionsAllowed == 0 && p.CurrentHealthy < p.DesiredHealthy &&
		p.UnhealthyPodEvictionPolicy != "AlwaysAllow" {
		ce.add(Finding{Rule: RuleBlockedUnhealthy, Severity: Medium, Namespace: p.Namespace,
			Kind: "PodDisruptionBudget", Name: p.Name,
			Title: fmt.Sprintf("PDB `%s` is blocking drains because pods are unhealthy", p.Name),
			Detail: fmt.Sprintf("%d of %d pods of %s are healthy, below the %d it requires, so it allows no evictions: "+
				"not even of the unhealthy pods, which pin their nodes.", p.CurrentHealthy, p.ExpectedPods, owners, p.DesiredHealthy),
			Fix: "Fix the unhealthy pods, and set `unhealthyPodEvictionPolicy: AlwaysAllow` so a broken pod never blocks a drain.",
		}, ig...)
	}
}

func (ce *clusterEval) instanceRules(in *Instance) {
	if !ce.production(in.Namespace) {
		// Outside production only drain blockers matter, and those are PDB
		// findings already made above.
		return
	}
	what := instanceText(in)

	if in.Quorum && in.Replicas >= 2 {
		n := in.Replicas
		tol := (n - 1) / 2
		if len(in.PDBs) == 0 {
			f := Finding{Rule: RuleQuorumNoPDB, Severity: High, Namespace: in.Namespace, Kind: in.Kind, Name: in.Name,
				Title: fmt.Sprintf("%s has no PodDisruptionBudget", what)}
			if n == 2 {
				f.Detail = "2 members cannot lose one and keep quorum, and nothing stops a drain from evicting one."
				f.Fix = "Run 3 members and add a PDB with `maxUnavailable: 1`."
			} else {
				f.Detail = fmt.Sprintf("%d members can lose only %d, and nothing stops a drain from evicting more at once "+
					"(two members on one node, or two drains running in parallel): the cluster loses quorum.", n, tol)
				f.Fix = "Add a PDB with `maxUnavailable: 1` that selects its pods."
			}
			ce.add(f, in.Ignore)
		}
		for _, name := range in.PDBs {
			p, ok := ce.pdbs[in.Namespace+"/"+name]
			if !ok {
				continue
			}
			allowed := allowedWhenHealthy(p, n)
			if allowed <= tol {
				continue
			}
			f := Finding{Rule: RuleBelowQuorum, Severity: High, Namespace: in.Namespace, Kind: "PodDisruptionBudget",
				Name: p.Name, Title: fmt.Sprintf("PDB `%s` lets a drain break the quorum of %s", p.Name, what)}
			if n == 2 {
				f.Detail = fmt.Sprintf("%s lets %d of 2 members go, and 2 members cannot lose one and keep quorum. "+
					"No PDB value is safe at this size: it either blocks drains or allows losing quorum.", specText(p), allowed)
				f.Fix = "Run 3 members with `maxUnavailable: 1`."
			} else {
				f.Detail = fmt.Sprintf("%s lets %d of %d members go at once, but the cluster keeps quorum only while it "+
					"loses at most %d.", specText(p), allowed, n, tol)
				f.Fix = fmt.Sprintf("Set `minAvailable: %d`, or `maxUnavailable: 1`.", n/2+1)
			}
			ce.add(f, append(pdbIgnores(p), in.Ignore)...)
		}
	}

	if in.Replicas == 1 && (in.Engine == "rabbitmq" || in.Engine == "redis") {
		ce.singleInstance(in, what)
	}
}

// singleInstance reports a one-member broker or cache. A PDB that also blocks
// its drains is folded into the same finding: the two are one problem.
func (ce *clusterEval) singleInstance(in *Instance, what string) {
	f := Finding{Severity: High, Namespace: in.Namespace, Kind: in.Kind, Name: in.Name,
		Title: fmt.Sprintf("%s runs 1 replica", what)}
	if in.Engine == "rabbitmq" {
		f.Rule = RuleRabbitSingle
		f.Detail = "Every drain of its node restarts it elsewhere, and the broker is down until it is back, " +
			"including reattaching its volume."
		f.Fix = "Run 3 replicas (`spec.replicas: 3`; quorum queues need an odd count) with a PDB of `maxUnavailable: 1`."
	} else {
		f.Rule = RuleRedisSingle
		f.Detail = "Every drain of its node restarts it elsewhere: Redis is down until then, and its data is lost " +
			"if persistence is off."
		f.Fix = "Run it replicated with failover (a `RedisReplication` of 3 with a `RedisSentinel`), " +
			"with a PDB of `maxUnavailable: 1`."
		if in.ReplicaWorkload != "" {
			// A Bitnami chart in replication mode without Sentinel.
			if in.ReadReplicas > 0 {
				f.Title = fmt.Sprintf("%s has %s but no failover", what, readReplicas(in.ReadReplicas))
				f.Detail = fmt.Sprintf("Its replicas (StatefulSet `%s`) only serve reads: without Sentinel none of them "+
					"takes over from the master. Every drain of the master's node stops writes until it is back elsewhere.",
					in.ReplicaWorkload)
			}
			f.Fix = "Enable Sentinel in the chart values (`sentinel.enabled: true`, `replica.replicaCount: 3`), " +
				"connect clients through Sentinel, and add a PDB of `maxUnavailable: 1`."
		}
	}
	if in.Source == "image" {
		f.Detail += fmt.Sprintf(" (Recognised by its image, `%s`.)", strings.Join(in.Images, "`, `"))
	}
	if ce.disabled[f.Rule] || ignoresRule(in.Ignore, f.Rule) {
		ce.add(f, in.Ignore)
		return
	}

	var blockers []string
	kept := ce.out[:0]
	for _, x := range ce.out {
		if x.Namespace == in.Namespace && x.Kind == "PodDisruptionBudget" && contains(in.PDBs, x.Name) &&
			(x.Rule == RuleMaxUnavailableZero || x.Rule == RuleMinAvailableAll) {
			blockers = append(blockers, x.Name)
			continue
		}
		kept = append(kept, x)
	}
	ce.out = kept
	if len(blockers) > 0 {
		f.Detail += fmt.Sprintf(" Its PDB `%s` also blocks drains: its node cannot be drained until a cloud admin "+
			"forces it, which takes it down anyway.", strings.Join(blockers, "`, `"))
	}
	ce.add(f, in.Ignore)
}

// allowedWhenHealthy is how many of n healthy members the PDB lets go.
// Computed from the spec against the desired member count, so a member that
// happens to be down at scan time does not change the verdict.
func allowedWhenHealthy(p *PDB, n int) int {
	switch {
	case p.MaxUnavailable != nil:
		return min(p.MaxUnavailable.scaled(n), n)
	case p.MinAvailable != nil:
		return max(n-p.MinAvailable.scaled(n), 0)
	}
	return 0
}

func specText(p *PDB) string {
	switch {
	case p.MaxUnavailable != nil:
		return "`maxUnavailable: " + p.MaxUnavailable.String() + "`"
	case p.MinAvailable != nil:
		return "`minAvailable: " + p.MinAvailable.String() + "`"
	}
	return "it"
}

func instanceText(in *Instance) string {
	label := map[string]string{"rabbitmq": "RabbitMQ", "redis": "Redis", "redis-sentinel": "Redis Sentinel"}[in.Engine]
	if label == "" {
		label = in.Engine
	}
	return fmt.Sprintf("%s `%s`", label, in.Name)
}

func ownersText(owners []Owner) string {
	if len(owners) == 0 {
		return "its pods"
	}
	parts := make([]string, 0, len(owners))
	for _, o := range owners {
		s := fmt.Sprintf("%s `%s`", o.Kind, o.Name)
		if o.Replicas != nil {
			s += fmt.Sprintf(" (%s)", replicas(*o.Replicas))
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func replicas(n int) string {
	if n == 1 {
		return "1 replica"
	}
	return fmt.Sprintf("%d replicas", n)
}

func readReplicas(n int) string {
	if n == 1 {
		return "1 read replica"
	}
	return fmt.Sprintf("%d read replicas", n)
}

func pods(n int) string {
	if n == 1 {
		return "1 pod"
	}
	return fmt.Sprintf("%d pods", n)
}

var severityRank = map[Severity]int{High: 0, Medium: 1}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Team != b.Team {
			return a.Team < b.Team
		}
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		return a.Key() < b.Key()
	})
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[strings.TrimSpace(x)] = true
	}
	return m
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
