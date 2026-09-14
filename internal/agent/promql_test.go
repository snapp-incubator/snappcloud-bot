package agent

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

var allowed = []string{"team-a", "team-a-staging"}

// Every selector must end up pinned, whatever shape the query takes. These are
// bypass attempts, not happy paths: each one is a way to read another tenant's
// series if the pinning misses a selector.
func TestPinNamespacesCoversEverySelector(t *testing.T) {
	for _, q := range []string{
		`up`,
		`rate(http_requests_total[5m])`,
		`sum(rate(http_requests_total[5m]))`, // aggregation hides labels
		`sum by (pod) (rate(http_requests_total[5m]))`,
		`{__name__=~".+"}`, // name-less selector
		`http_requests_total offset 1h`,
		`max_over_time(rate(http_requests_total[5m])[1h:5m])`, // subquery
		`http_requests_total / ignoring(code) other_metric`,   // both sides
		`absent(http_requests_total)`,
		`label_replace(up, "ns", "$1", "namespace", "(.*)")`, // label rewriting
		`topk(5, sum by (namespace) (rate(x[5m])))`,
		`count(up) by (namespace)`,
	} {
		out, err := PinNamespaces(q, allowed)
		if err != nil {
			t.Errorf("%s -> error: %v", q, err)
			continue
		}
		// Count selectors the crude way: every pin adds the matcher.
		if !strings.Contains(out, `namespace=~"team-a|team-a-staging"`) &&
			!strings.Contains(out, `namespace="team-a"`) {
			t.Errorf("%s -> not pinned: %s", q, out)
		}
		if strings.Contains(out, `namespace=~".*"`) || strings.Contains(out, `namespace!=`) {
			t.Errorf("%s -> escaped the pin: %s", q, out)
		}
	}
}

// A query naming someone else's namespace is refused outright rather than
// quietly rewritten to a different namespace.
func TestPinNamespacesRefusesAnotherTenant(t *testing.T) {
	for _, q := range []string{
		`up{namespace="team-b"}`,
		`sum(rate(http_requests_total{namespace="kube-system"}[5m]))`,
		`up{namespace="team-a"} / up{namespace="team-b"}`, // one good, one not
	} {
		if _, err := PinNamespaces(q, allowed); err == nil {
			t.Errorf("%s was allowed", q)
		} else if !strings.Contains(err.Error(), "not authorized") {
			t.Errorf("%s -> unclear error: %v", q, err)
		}
	}
}

// Anything that is not an exact, allowed namespace is replaced by the caller's
// own set: deciding whether one regex is a subset of another is not worth
// getting wrong.
func TestPinNamespacesReplacesLooseMatchers(t *testing.T) {
	for _, q := range []string{
		`up{namespace=~".*"}`,
		`up{namespace=~"team-.*"}`,
		`up{namespace!="team-b"}`,
		`up{namespace!~"kube-.*"}`,
	} {
		out, err := PinNamespaces(q, allowed)
		if err != nil {
			t.Fatalf("%s -> %v", q, err)
		}
		if !strings.Contains(out, `namespace=~"team-a|team-a-staging"`) {
			t.Errorf("%s -> %s (loose matcher survived)", q, out)
		}
	}
}

// An exact namespace the caller owns is honoured as written, so asking about
// one namespace does not silently return all of theirs.
func TestPinNamespacesKeepsAnOwnedNamespace(t *testing.T) {
	out, err := PinNamespaces(`sum(rate(http_requests_total{namespace="team-a"}[5m]))`, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `namespace="team-a"`) || strings.Contains(out, "team-a-staging") {
		t.Errorf("exact namespace not honoured: %s", out)
	}
}

// A name with a regex metacharacter must not widen the matcher.
func TestPinNamespacesEscapesNames(t *testing.T) {
	out, err := PinNamespaces(`up`, []string{"team.a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `namespace=~"team\\.a"`) {
		t.Errorf("name not escaped: %s", out)
	}
}

func TestPinNamespacesRejectsGarbage(t *testing.T) {
	if _, err := PinNamespaces(`sum(rate(`, allowed); err == nil {
		t.Error("invalid PromQL was accepted")
	}
	if _, err := PinNamespaces(`up`, nil); err == nil {
		t.Error("a caller with no namespaces was allowed to query")
	}
}

// End to end through the enforcer: the argument the tool actually receives must
// be the pinned one, and a tenant naming another namespace must be denied
// before the call leaves the bot.
func TestPinPromQLRewritesTheToolArgument(t *testing.T) {
	a := &Agent{
		enforcer: NewEnforcer(map[string]ToolRule{
			"query_prometheus": {PromQLArgs: []string{"expr"}},
		}),
		log: discardLogger(),
	}

	args := map[string]any{"expr": `sum(rate(http_requests_total[5m]))`, "datasourceUid": "abc"}
	if err := a.pinPromQL("query_prometheus", args, []string{"team-a"}); err != nil {
		t.Fatal(err)
	}
	got, _ := args["expr"].(string)
	if !strings.Contains(got, `namespace=~"team-a"`) {
		t.Errorf("argument not pinned before the call: %s", got)
	}
	if args["datasourceUid"] != "abc" {
		t.Error("other arguments must be left alone")
	}

	denied := map[string]any{"expr": `up{namespace="team-b"}`}
	if err := a.pinPromQL("query_prometheus", denied, []string{"team-a"}); err == nil {
		t.Error("a query for another namespace left the bot")
	}
}

// A tool with no PromQL rule is untouched, so this cannot affect anything else.
func TestPinPromQLIgnoresOtherTools(t *testing.T) {
	a := &Agent{enforcer: NewEnforcer(map[string]ToolRule{}), log: discardLogger()}
	args := map[string]any{"expr": `up{namespace="team-b"}`}
	if err := a.pinPromQL("list_pods", args, []string{"team-a"}); err != nil {
		t.Fatal(err)
	}
	if args["expr"] != `up{namespace="team-b"}` {
		t.Error("a non-PromQL tool's arguments were rewritten")
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
