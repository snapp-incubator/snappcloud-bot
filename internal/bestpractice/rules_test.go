package bestpractice

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func num(n int) *IntOrPercent { return &IntOrPercent{Value: n} }
func pct(n int) *IntOrPercent { return &IntOrPercent{Value: n, IsPercent: true} }

// pdbOf builds a PDB whose status is what the disruption controller would
// compute for expected healthy pods.
func pdbOf(ns, name string, minA, maxU *IntOrPercent, expected int) PDB {
	desired := 0
	switch {
	case maxU != nil:
		desired = max(expected-maxU.scaled(expected), 0)
	case minA != nil:
		desired = minA.scaled(expected)
	}
	return PDB{Namespace: ns, Name: name, MinAvailable: minA, MaxUnavailable: maxU, ExpectedPods: expected,
		CurrentHealthy: expected, DesiredHealthy: desired, DisruptionsAllowed: max(expected-desired, 0),
		Owners: []Owner{{Kind: "Deployment", Name: name, Replicas: &expected}}}
}

func inv(env string, pdbs []PDB, instances []Instance) Inventory {
	return Inventory{Clusters: []ClusterInventory{{
		Cluster: "teh-1",
		Audit: Audit{PDBs: pdbs, Instances: instances, Namespaces: map[string]NSOwner{
			"app": {Team: "pay", Env: env, Labels: map[string]string{
				"kubernetes.io/metadata.name": "app", "snappcloud.io/team": "pay", "tier": "platform"}},
		}},
		Teams: []Team{{Team: "pay", Admins: []string{"A@snapp.cab"}}},
	}}}
}

func rules(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		r := f.Rule + ":" + f.Name
		if f.Other != "" {
			r += "+" + f.Other
		}
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func TestDrainBlockers(t *testing.T) {
	for _, tc := range []struct {
		name string
		pdb  PDB
		want []string
	}{
		{"minAvailable 1 on 1 replica", pdbOf("app", "p", num(1), nil, 1), []string{"pdb-min-available-all:p"}},
		{"minAvailable 50% of 1 pod rounds up", pdbOf("app", "p", pct(50), nil, 1), []string{"pdb-min-available-all:p"}},
		{"minAvailable 100%", pdbOf("app", "p", pct(100), nil, 3), []string{"pdb-min-available-all:p"}},
		{"minAvailable 3 on 3", pdbOf("app", "p", num(3), nil, 3), []string{"pdb-min-available-all:p"}},
		{"maxUnavailable 0%", pdbOf("app", "p", nil, pct(0), 2), []string{"pdb-max-unavailable-zero:p"}},
		{"maxUnavailable 0", pdbOf("app", "p", nil, num(0), 3), []string{"pdb-max-unavailable-zero:p"}},
		{"maxUnavailable 1 on 1 replica is fine", pdbOf("app", "p", nil, num(1), 1), nil},
		{"minAvailable 1 on 2 is fine", pdbOf("app", "p", num(1), nil, 2), nil},
		{"matching no pods has no effect", pdbOf("app", "p", nil, num(0), 0), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, env := range []string{"production", "staging"} {
				got := rules(Evaluate(inv(env, []PDB{tc.pdb}, nil), Options{}).Findings)
				if !reflect.DeepEqual(got, append([]string{}, tc.want...)) {
					t.Errorf("%s: got %v, want %v", env, got, tc.want)
				}
			}
		})
	}
}

func TestOverlapReportedOncePerPair(t *testing.T) {
	a := pdbOf("app", "a", nil, num(1), 2)
	b := pdbOf("app", "b", nil, num(1), 2)
	a.OverlapsWith, b.OverlapsWith = []string{"b"}, []string{"a"}
	got := rules(Evaluate(inv("staging", []PDB{b, a}, nil), Options{}).Findings)
	if want := []string{"pdb-overlap:a+b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBlockedByUnhealthyPods(t *testing.T) {
	p := pdbOf("app", "p", num(2), nil, 3)
	p.CurrentHealthy, p.DisruptionsAllowed = 1, 0
	got := Evaluate(inv("staging", []PDB{p}, nil), Options{}).Findings
	if len(got) != 1 || got[0].Rule != RuleBlockedUnhealthy || got[0].Severity != Medium {
		t.Fatalf("got %+v, want one medium %s", got, RuleBlockedUnhealthy)
	}
	p.UnhealthyPodEvictionPolicy = "AlwaysAllow"
	if got := Evaluate(inv("staging", []PDB{p}, nil), Options{}).Findings; len(got) != 0 {
		t.Errorf("AlwaysAllow already evicts unhealthy pods, got %v", rules(got))
	}
}

func TestIgnoreAnnotation(t *testing.T) {
	quorum := Instance{Namespace: "app", Name: "zk", Engine: "zookeeper", Kind: "StatefulSet", Replicas: 3,
		Quorum: true, Source: "image"}
	for _, ig := range []string{"quorum-no-pdb", "pdb-overlap, quorum-no-pdb", "*"} {
		quorum.Ignore = ig
		res := Evaluate(inv("production", nil, []Instance{quorum}), Options{})
		if len(res.Findings) != 0 || res.Ignored[RuleQuorumNoPDB] != 1 {
			t.Errorf("annotation %q: findings %v ignored %v", ig, rules(res.Findings), res.Ignored)
		}
	}
}

// A PDB that blocks drains holds up maintenance for everyone on the node: no
// annotation, on the PDB or its workload, silences it.
func TestDrainBlockersCannotBeIgnored(t *testing.T) {
	p := pdbOf("app", "p", num(1), nil, 1)
	p.Ignore = "pdb-overlap, pdb-min-available-all"
	res := Evaluate(inv("production", []PDB{p}, nil), Options{})
	if got := rules(res.Findings); !reflect.DeepEqual(got, []string{"pdb-min-available-all:p"}) || len(res.Ignored) != 0 {
		t.Fatalf("PDB annotation: findings %v ignored %v", got, res.Ignored)
	}
	if !strings.Contains(res.Findings[0].Detail, "annotation does not apply") {
		t.Errorf("the finding must tell the team their annotation was overruled: %s", res.Findings[0].Detail)
	}

	p = pdbOf("app", "p", num(1), nil, 1)
	p.Owners[0].Ignore = "*"
	if got := rules(Evaluate(inv("production", []PDB{p}, nil), Options{}).Findings); len(got) != 1 {
		t.Errorf("owner annotation \"*\" must not silence a drain blocker, got %v", got)
	}
	for _, r := range Rules {
		want := r == RuleMaxUnavailableZero || r == RuleMinAvailableAll || r == RuleOverlap || r == RuleBlockedUnhealthy
		if DrainBlocker(r) != want {
			t.Errorf("DrainBlocker(%s) = %v", r, !want)
		}
	}
}

func TestSingleInstanceMergesItsBlockingPDB(t *testing.T) {
	events := pdbOf("app", "events-pdb", num(1), nil, 1)
	rmq := Instance{Namespace: "app", Name: "events", Engine: "rabbitmq", Kind: "RabbitmqCluster", Replicas: 1,
		Quorum: true, Source: "operator", PDBs: []string{"events-pdb"}}

	prod := Evaluate(inv("production", []PDB{events}, []Instance{rmq}), Options{}).Findings
	if got := rules(prod); !reflect.DeepEqual(got, []string{"rabbitmq-single-instance:events"}) {
		t.Fatalf("production: got %v, want one combined finding", got)
	}
	if !strings.Contains(prod[0].Detail, "events-pdb") {
		t.Errorf("combined finding must name the blocking PDB: %s", prod[0].Detail)
	}

	staging := Evaluate(inv("staging", []PDB{events}, []Instance{rmq}), Options{}).Findings
	if got := rules(staging); !reflect.DeepEqual(got, []string{"pdb-min-available-all:events-pdb"}) {
		t.Errorf("staging: got %v, want only the drain blocker", got)
	}
}

func TestNoEnvLabelIsProduction(t *testing.T) {
	redis := Instance{Namespace: "app", Name: "cache", Engine: "redis", Kind: "Deployment", Replicas: 1,
		Source: "image", PDBs: []string{}, Images: []string{"redis:7"}}
	got := Evaluate(inv("", nil, []Instance{redis}), Options{}).Findings
	if r := rules(got); !reflect.DeepEqual(r, []string{"redis-single-instance:cache"}) {
		t.Fatalf("got %v", r)
	}
	if !strings.Contains(got[0].Detail, "redis:7") {
		t.Errorf("image detection should say so: %s", got[0].Detail)
	}
}

func TestSentinelIsNotSingleInstance(t *testing.T) {
	s := Instance{Namespace: "app", Name: "sentinel", Engine: "redis-sentinel", Kind: "RedisSentinel", Replicas: 1,
		Quorum: true, Source: "operator", PDBs: []string{}}
	if got := Evaluate(inv("production", nil, []Instance{s}), Options{}).Findings; len(got) != 0 {
		t.Errorf("got %v", rules(got))
	}
}

func TestQuorum(t *testing.T) {
	quorum := func(n int, pdbs ...string) Instance {
		return Instance{Namespace: "app", Name: "kafka", Engine: "kafka", Kind: "StatefulSet", Replicas: n,
			Quorum: true, Source: "image", PDBs: pdbs}
	}
	for _, tc := range []struct {
		name string
		n    int
		pdb  *PDB
		want []string
	}{
		{"N=3 minAvailable 1", 3, ptr(pdbOf("app", "q", num(1), nil, 3)), []string{"pdb-below-quorum:q"}},
		{"N=3 minAvailable 2", 3, ptr(pdbOf("app", "q", num(2), nil, 3)), nil},
		{"N=3 maxUnavailable 1", 3, ptr(pdbOf("app", "q", nil, num(1), 3)), nil},
		{"N=3 maxUnavailable 2", 3, ptr(pdbOf("app", "q", nil, num(2), 3)), []string{"pdb-below-quorum:q"}},
		{"N=5 minAvailable 2", 5, ptr(pdbOf("app", "q", num(2), nil, 5)), []string{"pdb-below-quorum:q"}},
		{"N=5 minAvailable 3", 5, ptr(pdbOf("app", "q", num(3), nil, 5)), nil},
		{"N=5 maxUnavailable 50% rounds up to 3", 5, ptr(pdbOf("app", "q", nil, pct(50), 5)), []string{"pdb-below-quorum:q"}},
		// One member down at scan time: the status shrinks, the verdict must not.
		{"N=3 minAvailable 1, one member down", 3, ptr(pdbOf("app", "q", num(1), nil, 2)), []string{"pdb-below-quorum:q"}},
		{"N=2 maxUnavailable 1", 2, ptr(pdbOf("app", "q", nil, num(1), 2)), []string{"pdb-below-quorum:q"}},
		{"N=3 no PDB", 3, nil, []string{"quorum-no-pdb:kafka"}},
		{"N=2 no PDB", 2, nil, []string{"quorum-no-pdb:kafka"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pdbs []PDB
			in := quorum(tc.n)
			if tc.pdb != nil {
				pdbs = []PDB{*tc.pdb}
				in.PDBs = []string{tc.pdb.Name}
			}
			fs := Evaluate(inv("production", pdbs, []Instance{in}), Options{}).Findings
			if got := rules(fs); !reflect.DeepEqual(got, append([]string{}, tc.want...)) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if tc.n == 2 && len(fs) == 1 && !strings.Contains(fs[0].Fix, "3 members") {
				t.Errorf("2 members has no safe PDB; the fix must say 3 members: %s", fs[0].Fix)
			}
			// Outside production, quorum risks are not reported.
			if got := Evaluate(inv("staging", pdbs, []Instance{in}), Options{}).Findings; len(got) != 0 {
				t.Errorf("staging: got %v", rules(got))
			}
		})
	}
}

func TestOneMemberQuorumSystemIsOnlySingleInstance(t *testing.T) {
	rmq := Instance{Namespace: "app", Name: "events", Engine: "rabbitmq", Kind: "RabbitmqCluster", Replicas: 1,
		Quorum: true, Source: "operator", PDBs: []string{}}
	got := rules(Evaluate(inv("production", nil, []Instance{rmq}), Options{}).Findings)
	if !reflect.DeepEqual(got, []string{"rabbitmq-single-instance:events"}) {
		t.Errorf("got %v", got)
	}
}

func TestStatelessMinAvailableOneIsNotReported(t *testing.T) {
	if got := Evaluate(inv("production", []PDB{pdbOf("app", "web", num(1), nil, 3)}, nil), Options{}).Findings; len(got) != 0 {
		t.Errorf("a loose PDB on a stateless workload is not a hazard, got %v", rules(got))
	}
}

func selectors(t *testing.T, raw ...string) []Selector {
	t.Helper()
	var out []Selector
	for _, r := range raw {
		s, err := ParseSelector(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestExclusions(t *testing.T) {
	blocker := pdbOf("app", "p", num(1), nil, 1)
	for name, o := range map[string]Options{
		"label":             {ExcludeNamespaces: selectors(t, "tier=platform")},
		"any selector":      {ExcludeNamespaces: selectors(t, "nope", "tier in (db,platform)")},
		"namespace by name": {ExcludeNamespaces: selectors(t, "kubernetes.io/metadata.name=app")},
		"team":              {ExcludeTeams: []string{"pay"}},
		"disabled rule":     {DisabledRules: []string{RuleMinAvailableAll}},
	} {
		if got := Evaluate(inv("production", []PDB{blocker}, nil), o).Findings; len(got) != 0 {
			t.Errorf("%s: got %v", name, rules(got))
		}
	}
	for name, o := range map[string]Options{
		"other value":      {ExcludeNamespaces: selectors(t, "tier=apps")},
		"every req counts": {ExcludeNamespaces: selectors(t, "tier=platform,!snappcloud.io/team")},
	} {
		if got := Evaluate(inv("production", []PDB{blocker}, nil), o).Findings; len(got) != 1 {
			t.Errorf("%s: must not exclude, got %v", name, rules(got))
		}
	}
}

// A namespace whose labels are unknown is not excluded, even by a negative
// selector that an empty label set would satisfy.
func TestUnknownLabelsAreNotExcluded(t *testing.T) {
	in := inv("production", []PDB{pdbOf("app", "p", num(1), nil, 1)}, nil)
	in.Clusters[0].Audit.Namespaces["app"] = NSOwner{Team: "pay"}
	got := Evaluate(in, Options{ExcludeNamespaces: selectors(t, "!tier")}).Findings
	if len(got) != 1 {
		t.Errorf("got %v", rules(got))
	}
}

func TestSelector(t *testing.T) {
	labels := map[string]string{"env": "prod", "tier": "platform", "managed": ""}
	for sel, want := range map[string]bool{
		"env":                       true,
		"!env":                      false,
		"!missing":                  true,
		"env=prod":                  true,
		"env==prod":                 true,
		"env=staging":               false,
		"env!=staging":              true,
		"missing!=x":                true,
		"managed=":                  true,
		"tier in (db, platform)":    true,
		"tier notin (db,platform)":  false,
		"missing notin (a)":         true,
		"missing in (a)":            false,
		"env=prod,tier in (a,b)":    false,
		"env=prod, tier notin (db)": true,
	} {
		s, err := ParseSelector(sel)
		if err != nil {
			t.Errorf("%q: %v", sel, err)
			continue
		}
		if got := s.Matches(labels); got != want {
			t.Errorf("%q matches = %v, want %v", sel, got, want)
		}
	}
	for _, bad := range []string{"", "  ", "a b", "x in ()", "x in (a", "=v", "a=b=c", "a,", "x in (a b)"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

type fakeCaller map[string]string

func (f fakeCaller) CallSystemTool(_ context.Context, _, tool string, _ map[string]any) (string, error) {
	return f[tool], nil
}

func TestCollectRefusesAuditWithoutLabels(t *testing.T) {
	old := fakeCaller{
		ToolAudit: `{"pdbs":[],"instances":[],"namespaces":{"app":{"team":"pay"}}}`,
		ToolTeams: `{"teams":[]}`,
	}
	got := Collect(context.Background(), old, []string{"teh-1"}, nil)
	if len(got.Clusters) != 0 || !strings.Contains(got.Failed["teh-1"], "too old") {
		t.Errorf("got %+v", got)
	}
	current := fakeCaller{
		ToolAudit: `{"pdbs":[],"instances":[],"namespaces":{"app":{"team":"pay","labels":{"kubernetes.io/metadata.name":"app"}}}}`,
		ToolTeams: `{"teams":[{"team":"pay","admins":["a@snapp.cab"]}]}`,
	}
	got = Collect(context.Background(), current, []string{"teh-1"}, nil)
	if len(got.Clusters) != 1 || got.Clusters[0].Audit.Namespaces["app"].Labels["kubernetes.io/metadata.name"] != "app" {
		t.Errorf("got %+v", got)
	}
}

func TestFindingCarriesClusterAndTeam(t *testing.T) {
	f := Evaluate(inv("production", []PDB{pdbOf("app", "p", num(1), nil, 1)}, nil), Options{}).Findings[0]
	if f.Cluster != "teh-1" || f.Team != "pay" || f.Severity != High {
		t.Errorf("got %+v", f)
	}
	if f.Key() != "teh-1/app/PodDisruptionBudget/p/pdb-min-available-all" {
		t.Errorf("key = %s", f.Key())
	}
}

// The shape openshift-mcp's disruption_audit and list_teams actually return.
func TestDecodeAudit(t *testing.T) {
	raw := `{"pdbs":[{"ns":"pay-prod","name":"api-pdb","minAvailable":1,"expectedPods":1,"currentHealthy":1,
		"desiredHealthy":1,"disruptionsAllowed":0,"owners":[{"kind":"Deployment","name":"api","replicas":1,
		"images":["registry.local/pay/api:1"],"ignore":"pdb-overlap"}],"overlapsWith":["api-pdb-2"]},
		{"ns":"pay-prod","name":"api-pdb-2","maxUnavailable":"0%","expectedPods":1,"currentHealthy":1,
		"desiredHealthy":1,"disruptionsAllowed":0,"owners":[]}],
		"instances":[{"ns":"pay-prod","name":"events","engine":"rabbitmq","kind":"RabbitmqCluster",
		"workload":"events-server","replicas":1,"quorum":true,"source":"operator","pdbs":["events-pdb"]}],
		"namespaces":{"pay-prod":{"team":"pay","env":"production"},"plat":{}},
		"notInstalled":["redis.redis.redis.opstreelabs.in"],"quorumImages":["rabbitmq"]}`
	var a Audit
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if a.PDBs[0].MinAvailable == nil || *a.PDBs[0].MinAvailable != (IntOrPercent{Value: 1}) {
		t.Errorf("minAvailable = %+v", a.PDBs[0].MinAvailable)
	}
	if mu := a.PDBs[1].MaxUnavailable; mu == nil || mu.String() != "0%" {
		t.Errorf("maxUnavailable = %+v", mu)
	}
	if a.Instances[0].Replicas != 1 || !a.Instances[0].Quorum || a.Namespaces["pay-prod"].Env != "production" {
		t.Errorf("instances/namespaces = %+v %+v", a.Instances, a.Namespaces)
	}

	var teams struct {
		Teams []Team `json:"teams"`
	}
	if err := json.Unmarshal([]byte(`{"count":1,"teams":[{"team":"pay","admins":["a@snapp.cab"],
		"projects":[{"name":"pay-prod","envLabel":"production"}]}]}`), &teams); err != nil || teams.Teams[0].Admins[0] != "a@snapp.cab" {
		t.Errorf("teams = %+v, %v", teams, err)
	}
}

func TestTeamAdminsMergesClusters(t *testing.T) {
	got := Inventory{Clusters: []ClusterInventory{
		{Cluster: "a", Teams: []Team{{Team: "pay", Admins: []string{"B@snapp.cab", "a@snapp.cab"}}}},
		{Cluster: "b", Teams: []Team{{Team: "pay", Admins: []string{"a@snapp.cab", "c@snapp.cab"}}}},
	}}.TeamAdmins()
	if want := []string{"a@snapp.cab", "b@snapp.cab", "c@snapp.cab"}; !reflect.DeepEqual(got["pay"], want) {
		t.Errorf("got %v, want %v", got["pay"], want)
	}
}

func ptr[T any](v T) *T { return &v }
