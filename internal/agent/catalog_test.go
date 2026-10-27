package agent

import (
	"context"
	"strings"
	"testing"
)

// The servers one production cluster runs, in the order they advertise.
var prodServers = map[string][]string{
	"cilium": strings.Fields(`bgp_peers bgp_route_policies bgp_routes bpf_auth_list bpf_bandwidth_list bpf_config_list
		bpf_ct_list bpf_ct_top bpf_egress_list bpf_endpoint_list bpf_frag_list bpf_fs_show bpf_ipcache_list bpf_ipmasq_list
		bpf_lb_list bpf_maglev_list bpf_metrics_list bpf_nat_list bpf_nat_retries_list bpf_nat_top bpf_nodeid_list
		bpf_policy_list bpf_sha_list bpf_tunnel_list cgroups_list cilium_health_status cilium_logs cilium_status config_get
		debuginfo encrypt_status endpoint_get endpoint_health endpoint_list endpoint_log endpointslice_list
		envoy_admin_clusters envoy_admin_listeners find_cilium_pod_for_workload fqdn_cache_list fqdn_names get_cilium_pods
		identity_get identity_list ip_list kubectl_get kubeproxy_status lb_services_list list_unmanaged_pods lrp_list
		map_events map_get map_list metrics_list monitor monitor_drops node_list policy_get policy_selectors prefilter_list
		service_backends service_list statedb troubleshoot`),
	"envoy": strings.Fields(`envoy_certs envoy_clusters envoy_clusters_health envoy_config_dump envoy_endpoints
		envoy_listeners envoy_memory envoy_ready envoy_routes envoy_runtime envoy_server_info envoy_stats get_contour_dag
		get_httpproxy get_httpproxy_routes get_httpproxy_status get_httpproxy_tree list_envoy_ingress_classes list_envoy_pods
		list_extension_services list_httpproxies list_invalid_httpproxies list_tls_cert_delegations
		search_httpproxy_by_backend search_httpproxy_by_fqdn`),
	"openshift": strings.Fields(`disruption_audit get_node get_pod get_policy_report get_quota get_resource get_service
		get_workload list_certificates list_events list_nodes list_pods list_policies list_pvcs list_resource list_routes
		list_services list_teams list_workloads pod_logs top_pods`),
	"hubble": strings.Fields(`flow_summary get_dns_flows get_dropped_flows get_flows get_flows_between_pods
		get_flows_by_pod get_flows_by_service get_http_flows get_namespaces get_nodes server_status`),
	"argocd": strings.Fields(`argocd_can_i argocd_get_application argocd_get_project argocd_list_applications
		argocd_list_namespaces argocd_list_projects`),
	"grafana": seq("query_", 15),
}

var descriptions = map[string]string{
	"monitor_drops":     "Stream packet drop events from the Cilium agent on a node. Shows why packets are dropped.",
	"bpf_ct_list":       "Dump the connection tracking table of a node.",
	"cilium_status":     "Agent health and status of a node.",
	"get_dropped_flows": "Flows Hubble saw as dropped, with the drop reason.",
}

// recMCP is a cluster that serves prodServers and records what was called.
type recMCP struct {
	called []string
	args   []map[string]any
}

func (m *recMCP) ListTools(context.Context) ([]Tool, error) {
	var ts []Tool
	for _, srv := range []string{"cilium", "hubble", "envoy", "openshift", "argocd", "grafana"} {
		for _, n := range prodServers[srv] {
			ts = append(ts, Tool{Name: n, Server: srv, Description: descriptions[n],
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"namespace": map[string]any{"type": "string"}, "node": map[string]any{"type": "string"}}}})
		}
	}
	return ts, nil
}

func (m *recMCP) CallTool(_ context.Context, name string, args map[string]any) (string, error) {
	m.called = append(m.called, name)
	m.args = append(m.args, args)
	return "{}", nil
}

func prodTotal() int {
	n := 0
	for _, ts := range prodServers {
		n += len(ts)
	}
	return n
}

// At production size the list is over the cap, and every tool that does not
// fit is named to the model and callable through tool_call.
func TestEveryToolReachableOverTheCap(t *testing.T) {
	if prodTotal() <= 128 {
		t.Fatalf("fixture must exceed the cap, has %d", prodTotal())
	}
	srv := &recMCP{}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: toolCallName, Args: map[string]any{"name": "monitor_drops",
			"arguments": map[string]any{"node": "okd4-bgp-0"}}}}},
		{Text: "done"},
	}}
	_, err := quietAgent(llm, nil, 128).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-2", ClusterAdmin: true, Preferred: true, Allowed: []string{"team-a"}, MCP: srv},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.seen[0]
	if len(req.Tools) > 128 {
		t.Fatalf("offered %d tools, cap is 128", len(req.Tools))
	}
	for _, list := range prodServers {
		for _, n := range list {
			if toolByName(req.Tools, n) == nil && !strings.Contains(req.System, "- "+n+":") {
				t.Errorf("%s is neither offered nor in the directory", n)
			}
		}
	}
	if len(srv.called) != 1 || srv.called[0] != "monitor_drops" || srv.args[0]["node"] != "okd4-bgp-0" {
		t.Fatalf("tool_call did not run the tool: %v %v", srv.called, srv.args)
	}
	if !equalNames(llm.seen[0].Tools, llm.seen[1].Tools) {
		t.Fatal("the tool array changed between rounds")
	}
}

// tool_call is no way around access rules: a namespace the caller lacks and a
// cluster-admin tool for a non-admin are refused as they are for a direct call.
func TestToolCallKeepsEnforcement(t *testing.T) {
	srv := &recMCP{}
	rules := map[string]ToolRule{"cilium_status": {ClusterAdminOnly: true}}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{
			{ID: "1", Name: toolCallName, Args: map[string]any{"name": "troubleshoot", "arguments": map[string]any{"namespace": "team-b"}}},
			{ID: "2", Name: toolCallName, Args: map[string]any{"name": "cilium_status", "arguments": map[string]any{}}},
		}},
		{Text: "done"},
	}}
	_, err := quietAgent(llm, rules, 128).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-2", Allowed: []string{"team-a"}, MCP: srv},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.called) != 0 {
		t.Fatalf("refused calls reached the server: %v", srv.called)
	}
	res := lastResults(llm)
	if !strings.Contains(res[0].Content, "team-b") || !strings.Contains(res[1].Content, "cluster-admin") {
		t.Fatalf("results: %+v", res)
	}
	if strings.Contains(llm.seen[0].System, "- cilium_status:") {
		t.Fatal("a tool the caller may not use is listed as deferred")
	}
}

// Configured priority decides what stays in the list when it is trimmed.
func TestPriorityKeepsToolDirect(t *testing.T) {
	rules := map[string]ToolRule{"troubleshoot": {Priority: 10}, "monitor_drops": {Priority: 10}}
	llm := &fakeLLM{turns: []Response{{Text: "ok"}}}
	_, err := quietAgent(llm, rules, 128).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-2", ClusterAdmin: true, Allowed: []string{"team-a"}, MCP: &recMCP{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"troubleshoot", "monitor_drops"} {
		if toolByName(llm.seen[0].Tools, n) == nil {
			t.Errorf("%s has priority but was deferred", n)
		}
	}
}

func TestSearchFindsByWhatTheToolDoes(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: toolSearchName, Args: map[string]any{"queries": []any{"dropped packets", "bgp_routes"}}}}},
		{Text: "ok"},
	}}
	_, err := quietAgent(llm, nil, 20).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-2", ClusterAdmin: true, Allowed: []string{"team-a"}, MCP: &recMCP{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := lastResult(llm)
	first := strings.Index(out, `"name":"`)
	if first < 0 || !strings.HasPrefix(out[first+8:], "monitor_drops") && !strings.HasPrefix(out[first+8:], "get_dropped_flows") {
		t.Fatalf("drop tools not ranked first: %s", out)
	}
	if !strings.Contains(out, `"query":"bgp_routes","matches":[{"name":"bgp_routes"`) {
		t.Fatalf("exact name not matched first: %s", out)
	}
}

// Under the cap nothing is deferred and no bridge is offered.
func TestNoBridgeUnderTheCap(t *testing.T) {
	llm := &fakeLLM{turns: []Response{{Text: "ok"}}}
	_, err := quietAgent(llm, nil, 1000).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-2", ClusterAdmin: true, Allowed: []string{"team-a"}, MCP: &recMCP{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if toolByName(llm.seen[0].Tools, toolSearchName) != nil || len(llm.seen[0].Tools) != prodTotal() {
		t.Fatalf("offered %d tools with bridge=%v", len(llm.seen[0].Tools), toolByName(llm.seen[0].Tools, toolSearchName) != nil)
	}
}

func equalNames(a, b []Tool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}
