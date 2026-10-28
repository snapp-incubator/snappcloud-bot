package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

// Tools that do not fit maxTools are deferred, not dropped: the model finds
// them with tool_search, reads a schema with tool_describe and runs one with
// tool_call, which goes through the same enforcement as a direct call. The
// tool array stays the same for the whole turn, so it stays cacheable.
const (
	toolSearchName   = "tool_search"
	toolDescribeName = "tool_describe"
	toolCallName     = "tool_call"
	bridgeTools      = 3

	searchDefaultLimit = 8
	searchMaxLimit     = 20
	// directoryRunes bounds the list of deferred tools in the instructions.
	// Past it, descriptions go and only names are listed.
	directoryRunes = 8000
)

// catalog is every tool the caller may call this turn, and which of them are
// deferred behind the bridge.
type catalog struct {
	routes   map[string]*route
	all      []*route // offerable, in listing order
	direct   map[string]bool
	deferred []*route
	index    *bm25
}

func newCatalog(routes map[string]*route, order []*route, direct []Tool) *catalog {
	c := &catalog{routes: routes, direct: make(map[string]bool, len(direct))}
	for _, t := range direct {
		c.direct[t.Name] = true
	}
	for _, r := range order {
		if len(r.clusters) == 0 {
			continue
		}
		c.all = append(c.all, r)
		if !c.direct[r.name] {
			c.deferred = append(c.deferred, r)
		}
	}
	if len(c.deferred) > 0 {
		docs := make([][]string, len(c.all))
		for i, r := range c.all {
			docs[i] = routeTerms(r)
		}
		c.index = newBM25(docs)
	}
	return c
}

func (c *catalog) bridge() []Tool {
	if len(c.deferred) == 0 {
		return nil
	}
	return []Tool{
		{
			Name: toolSearchName,
			Description: "Search the tools that are not in your tool list. Matches tool names, descriptions and " +
				"argument names; returns each match's name, what it does and its arguments.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"queries": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
						"description": "One or more searches, each a few words naming what you need, e.g. \"dropped packets\" or \"bgp peers\"."},
					"limit": map[string]any{"type": "integer", "description": fmt.Sprintf("Matches per query (default %d, max %d).", searchDefaultLimit, searchMaxLimit)},
				},
				"required": []any{"queries"},
			},
		},
		{
			Name:        toolDescribeName,
			Description: "Return the full argument schema of tools found with tool_search, before calling them with tool_call.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []any{"names"},
			},
		},
		{
			Name: toolCallName,
			Description: "Run a tool that is not in your tool list, by its name and arguments, exactly as you would " +
				"call it directly. For several, call this several times in the same step.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string"},
					"arguments": map[string]any{"type": "object"},
				},
				"required": []any{"name"},
			},
		},
	}
}

// directory is the deferred tools' names and first sentences, for the
// instructions, so the model knows what it can search for.
func (c *catalog) directory() string {
	if len(c.deferred) == 0 {
		return ""
	}
	lines := make([]string, len(c.deferred))
	size := 0
	for i, r := range c.deferred {
		lines[i] = "- " + r.name + ": " + firstSentence(r.tool.Description, 100)
		size += len([]rune(lines[i])) + 1
	}
	if size > directoryRunes {
		names := make([]string, len(c.deferred))
		for i, r := range c.deferred {
			names[i] = r.name
		}
		return strings.Join(names, ", ")
	}
	return strings.Join(lines, "\n")
}

func (c *catalog) search(args map[string]any) string {
	if c.index == nil {
		return "Every tool is already in your tool list."
	}
	limit := searchDefaultLimit
	if n, ok := args["limit"].(float64); ok && n > 0 {
		limit = int(n)
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}
	queries := valuesOf(args["queries"])
	if q, ok := args["query"].(string); ok {
		queries = append(queries, q)
	}
	if len(queries) == 0 {
		return "Pass queries: a few words naming what you need."
	}
	type hit struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Arguments   []string `json:"arguments,omitempty"`
		Required    []string `json:"required,omitempty"`
		Direct      bool     `json:"inToolList,omitempty"`
	}
	type group struct {
		Query   string `json:"query"`
		Matches []hit  `json:"matches"`
	}
	out := make([]group, 0, len(queries))
	for _, q := range queries {
		g := group{Query: q, Matches: []hit{}}
		for _, i := range c.index.top(q, c.all, limit) {
			r := c.all[i]
			args, req := schemaArgs(r.tool.InputSchema)
			g.Matches = append(g.Matches, hit{Name: r.name, Description: firstSentence(r.tool.Description, 300),
				Arguments: args, Required: req, Direct: c.direct[r.name]})
		}
		out = append(out, g)
	}
	b, _ := json.Marshal(map[string]any{"results": out,
		"next": "tool_describe for the full schema, then tool_call. A match with inToolList is in your list: call it directly."})
	return string(b)
}

func (c *catalog) describe(args map[string]any) string {
	type desc struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	var out []desc
	var missing []string
	for _, n := range valuesOf(args["names"]) {
		r := c.lookup(n)
		if r == nil {
			missing = append(missing, n)
			continue
		}
		out = append(out, desc{Name: r.name, Description: r.tool.Description, InputSchema: r.tool.InputSchema})
	}
	res := map[string]any{"tools": out}
	if len(missing) > 0 {
		res["notFound"] = missing
		res["hint"] = "use tool_search to find the right name"
	}
	b, _ := json.Marshal(res)
	return string(b)
}

// unwrap turns a tool_call into the call it carries.
func (c *catalog) unwrap(call ToolCall) (ToolCall, string) {
	name, _ := call.Args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return call, "tool_call needs name: the tool to run."
	}
	if name == toolCallName || name == toolSearchName || name == toolDescribeName {
		return call, "tool_call runs a tool, not " + name + ": call " + name + " directly."
	}
	inner, _ := call.Args["arguments"].(map[string]any)
	if inner == nil {
		inner = map[string]any{}
	}
	if r := c.lookup(name); r != nil {
		name = r.name
	}
	return ToolCall{ID: call.ID, Name: name, Args: inner}, ""
}

func (c *catalog) lookup(name string) *route {
	name = strings.TrimSpace(name)
	if r := c.routes[name]; r != nil && len(r.clusters) > 0 {
		return r
	}
	if r, _ := legacyRoute(c.routes, name); r != nil && len(r.clusters) > 0 {
		return r
	}
	return nil
}

func schemaArgs(s map[string]any) ([]string, []string) {
	props, _ := s["properties"].(map[string]any)
	args := make([]string, 0, len(props))
	for k := range props {
		args = append(args, k)
	}
	sort.Strings(args)
	return args, valuesOf(s["required"])
}

func firstSentence(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// routeTerms is what a tool is searched by: its name, description and
// argument names.
func routeTerms(r *route) []string {
	args, _ := schemaArgs(r.tool.InputSchema)
	text := r.name + " " + r.tool.Description + " " + strings.Join(args, " ")
	return terms(text)
}

func terms(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(f))
	for _, w := range f {
		if len(w) < 2 || stopWords[w] {
			continue
		}
		out = append(out, stem(w))
	}
	return out
}

// stem strips common English suffixes so "drops" matches "drop" and
// "peering" matches "peer".
func stem(w string) string {
	for _, suf := range []string{"ing", "ies", "es", "ed", "s"} {
		if len(w) > len(suf)+2 && strings.HasSuffix(w, suf) {
			if suf == "ies" {
				return w[:len(w)-3] + "y"
			}
			return w[:len(w)-len(suf)]
		}
	}
	return w
}

var stopWords = map[string]bool{"the": true, "and": true, "for": true, "of": true, "to": true, "in": true,
	"on": true, "or": true, "is": true, "an": true, "by": true, "with": true, "from": true, "this": true,
	"that": true, "it": true, "as": true, "be": true, "are": true, "its": true}

// bm25 ranks tools against a query (Okapi BM25, k1=1.2, b=0.75).
type bm25 struct {
	docs  []map[string]int
	lens  []int
	avg   float64
	df    map[string]int
	total int
}

func newBM25(docs [][]string) *bm25 {
	b := &bm25{df: map[string]int{}, total: len(docs)}
	sum := 0
	for _, d := range docs {
		tf := map[string]int{}
		for _, t := range d {
			tf[t]++
		}
		for t := range tf {
			b.df[t]++
		}
		b.docs = append(b.docs, tf)
		b.lens = append(b.lens, len(d))
		sum += len(d)
	}
	if len(docs) > 0 {
		b.avg = float64(sum) / float64(len(docs))
	}
	return b
}

// top returns the indexes of the best matches, an exact name first.
func (b *bm25) top(query string, all []*route, limit int) []int {
	q := strings.TrimSpace(query)
	exact := -1
	for i, r := range all {
		if r.name == q {
			exact = i
		}
	}
	qt := terms(q)
	type scored struct {
		i int
		s float64
	}
	var hits []scored
	for i, tf := range b.docs {
		if i == exact {
			continue
		}
		s := 0.0
		for _, t := range qt {
			n := tf[t]
			if n == 0 {
				continue
			}
			idf := math.Log(1 + (float64(b.total-b.df[t])+0.5)/(float64(b.df[t])+0.5))
			norm := float64(n) * 2.2 / (float64(n) + 1.2*(0.25+0.75*float64(b.lens[i])/b.avg))
			s += idf * norm
		}
		if s > 0 {
			hits = append(hits, scored{i, s})
		}
	}
	sort.SliceStable(hits, func(x, y int) bool { return hits[x].s > hits[y].s })
	var out []int
	if exact >= 0 {
		out = append(out, exact)
	}
	for _, h := range hits {
		if len(out) >= limit {
			break
		}
		out = append(out, h.i)
	}
	return out
}
