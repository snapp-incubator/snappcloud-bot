package mcp

import (
	"context"
	"sort"
)

// Nobody could see what the bot actually had. Whether a cluster's servers were
// answering, how many tools each advertised, which ones were refusing
// credentials — all of it was visible only in log lines and only to whoever
// thought to look. Every wrong answer about a "missing tool" had to be
// diagnosed by guessing from the answer's wording.

// ServerStatus is one MCP server as the bot sees it right now.
type ServerStatus struct {
	Name  string
	URL   string
	Tools int
	Err   string // empty when the server answered
}

// Status lists every server with its current tool count, or the error that
// stopped it. It performs a real listing — this is the state that matters, not
// a cached one.
func (m *Mux) Status(ctx context.Context) []ServerStatus {
	out := make([]ServerStatus, 0, len(m.servers))
	for _, s := range m.servers {
		st := ServerStatus{Name: s.name, URL: s.client.URL()}
		ts, err := s.client.ListTools(ctx)
		if err != nil {
			st.Err = err.Error()
		} else {
			st.Tools = len(ts)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
