// Package bestpractice builds the weekly best-practice report: single-instance
// broker and cache tools, and PodDisruptionBudgets that block node drains or let a
// drain break a quorum. The data comes from openshift-mcp's disruption_audit
// and list_teams; the rules that turn it into findings are plain code here, so
// the same cluster state always produces the same report.
package bestpractice

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Tool names served by openshift-mcp.
const (
	ToolAudit = "disruption_audit"
	ToolTeams = "list_teams"
)

// Caller runs one MCP tool on one cluster with the bot's own authority. The
// brain implements it; nothing a user or the model sends reaches it.
type Caller interface {
	CallSystemTool(ctx context.Context, cluster, tool string, args map[string]any) (string, error)
}

// IntOrPercent is a PDB's minAvailable or maxUnavailable exactly as written:
// an integer, or a string such as "50%" (or, rarely, "1").
type IntOrPercent struct {
	Value     int
	IsPercent bool
}

func (v *IntOrPercent) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*v = IntOrPercent{Value: n}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("intOrPercent: %s", b)
	}
	s = strings.TrimSpace(s)
	pct := strings.HasSuffix(s, "%")
	n, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
	if err != nil {
		return fmt.Errorf("intOrPercent: %q", s)
	}
	*v = IntOrPercent{Value: n, IsPercent: pct}
	return nil
}

func (v IntOrPercent) String() string {
	if v.IsPercent {
		return strconv.Itoa(v.Value) + "%"
	}
	return strconv.Itoa(v.Value)
}

// scaled resolves the value against n pods the way the disruption controller
// does: percentages round up.
func (v IntOrPercent) scaled(n int) int {
	if !v.IsPercent {
		return v.Value
	}
	return (v.Value*n + 99) / 100
}

// PDB is one PodDisruptionBudget as disruption_audit reports it.
type PDB struct {
	Namespace                  string        `json:"ns"`
	Name                       string        `json:"name"`
	MinAvailable               *IntOrPercent `json:"minAvailable,omitempty"`
	MaxUnavailable             *IntOrPercent `json:"maxUnavailable,omitempty"`
	UnhealthyPodEvictionPolicy string        `json:"unhealthyPodEvictionPolicy,omitempty"`
	ExpectedPods               int           `json:"expectedPods"`
	CurrentHealthy             int           `json:"currentHealthy"`
	DesiredHealthy             int           `json:"desiredHealthy"`
	DisruptionsAllowed         int           `json:"disruptionsAllowed"`
	Owners                     []Owner       `json:"owners"`
	OverlapsWith               []string      `json:"overlapsWith,omitempty"`
	Ignore                     string        `json:"ignore,omitempty"`
}

// Owner is a controller of the pods a PDB selects.
type Owner struct {
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	Replicas     *int     `json:"replicas,omitempty"`
	Images       []string `json:"images,omitempty"`
	OperatorKind string   `json:"operatorKind,omitempty"`
	Ignore       string   `json:"ignore,omitempty"`
}

// Instance is a broker, cache or quorum system: an operator CR, or a plain
// workload recognised by its image.
type Instance struct {
	Namespace string   `json:"ns"`
	Name      string   `json:"name"`
	Engine    string   `json:"engine"`
	Kind      string   `json:"kind"`
	Workload  string   `json:"workload,omitempty"`
	Replicas  int      `json:"replicas"`
	Quorum    bool     `json:"quorum"`
	Source    string   `json:"source"`
	PDBs      []string `json:"pdbs"`
	Images    []string `json:"images,omitempty"`
	Ignore    string   `json:"ignore,omitempty"`
}

// NSOwner is a namespace's team and environment, and all its labels.
type NSOwner struct {
	Team   string            `json:"team,omitempty"`
	Env    string            `json:"env,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Audit is one cluster's disruption_audit result.
type Audit struct {
	PDBs         []PDB              `json:"pdbs"`
	Instances    []Instance         `json:"instances"`
	Namespaces   map[string]NSOwner `json:"namespaces"`
	NotInstalled []string           `json:"notInstalled"`
}

// Team is one Team CR: who administers it.
type Team struct {
	Team   string   `json:"team"`
	Admins []string `json:"admins"`
}

// ClusterInventory is everything collected from one cluster.
type ClusterInventory struct {
	Cluster string
	Audit   Audit
	Teams   []Team
}

// Inventory is one collection across clusters. A cluster that could not be
// read is in Failed, never silently absent: its findings would otherwise read
// as "nothing wrong there".
type Inventory struct {
	Clusters []ClusterInventory
	Failed   map[string]string
}

// Scanned returns the clusters that were read, sorted.
func (inv Inventory) Scanned() []string {
	out := make([]string, 0, len(inv.Clusters))
	for _, c := range inv.Clusters {
		out = append(out, c.Cluster)
	}
	sort.Strings(out)
	return out
}

// TeamAdmins merges every cluster's Team CRs into team -> admins. A team exists
// once per cluster, and its admin lists can differ between them.
func (inv Inventory) TeamAdmins() map[string][]string {
	seen := map[string]map[string]bool{}
	for _, c := range inv.Clusters {
		for _, t := range c.Teams {
			if seen[t.Team] == nil {
				seen[t.Team] = map[string]bool{}
			}
			for _, a := range t.Admins {
				if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
					seen[t.Team][a] = true
				}
			}
		}
	}
	out := make(map[string][]string, len(seen))
	for team, admins := range seen {
		list := make([]string, 0, len(admins))
		for a := range admins {
			list = append(list, a)
		}
		sort.Strings(list)
		out[team] = list
	}
	return out
}

// collectConcurrency bounds how many clusters are audited at once: each audit
// lists every pod of a cluster on the openshift-mcp side.
const collectConcurrency = 4

// Collect audits every cluster. One cluster failing does not stop the others.
func Collect(ctx context.Context, c Caller, clusters, quorumImages []string) Inventory {
	inv := Inventory{Failed: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, collectConcurrency)
	for _, cluster := range clusters {
		wg.Add(1)
		go func(cluster string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ci, err := collectCluster(ctx, c, cluster, quorumImages)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				inv.Failed[cluster] = err.Error()
				return
			}
			inv.Clusters = append(inv.Clusters, ci)
		}(cluster)
	}
	wg.Wait()
	sort.Slice(inv.Clusters, func(i, j int) bool { return inv.Clusters[i].Cluster < inv.Clusters[j].Cluster })
	return inv
}

func collectCluster(ctx context.Context, c Caller, cluster string, quorumImages []string) (ClusterInventory, error) {
	args := map[string]any{}
	if len(quorumImages) > 0 {
		args["quorum_images"] = quorumImages
	}
	raw, err := c.CallSystemTool(ctx, cluster, ToolAudit, args)
	if err != nil {
		return ClusterInventory{}, fmt.Errorf("%s: %w", ToolAudit, err)
	}
	ci := ClusterInventory{Cluster: cluster}
	if err := json.Unmarshal([]byte(raw), &ci.Audit); err != nil {
		return ClusterInventory{}, fmt.Errorf("decode %s: %w", ToolAudit, err)
	}
	// Every namespace carries at least kubernetes.io/metadata.name, so a
	// result with no labels anywhere comes from an openshift-mcp too old to
	// report them, and label exclusions would silently not apply.
	if len(ci.Audit.Namespaces) > 0 && !anyLabels(ci.Audit.Namespaces) {
		return ClusterInventory{}, fmt.Errorf("%s reports no namespace labels: openshift-mcp is too old for label exclusions", ToolAudit)
	}
	// Without teams there are no recipients, and a report that silently has
	// none is worse than one that says the cluster failed.
	raw, err = c.CallSystemTool(ctx, cluster, ToolTeams, map[string]any{})
	if err != nil {
		return ClusterInventory{}, fmt.Errorf("%s: %w", ToolTeams, err)
	}
	var teams struct {
		Teams []Team `json:"teams"`
	}
	if err := json.Unmarshal([]byte(raw), &teams); err != nil {
		return ClusterInventory{}, fmt.Errorf("decode %s: %w", ToolTeams, err)
	}
	ci.Teams = teams.Teams
	return ci, nil
}

func anyLabels(ns map[string]NSOwner) bool {
	for _, o := range ns {
		if len(o.Labels) > 0 {
			return true
		}
	}
	return false
}
