package mcp

import (
	"context"
	"errors"
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
	// Cause classifies a refusal that the status code misattributes to the
	// server: "no-credential" when the bot sent no Authorization header at all
	// (a missing Secret key, not a sick server) and "credential-refused" when it
	// sent one and the server rejected it (an expired token). Empty otherwise.
	Cause string
}

// Status lists every server with its current tool count, or the error that
// stopped it. It performs a real listing — this is the state that matters, not
// a cached one.
func (m *Mux) Status(ctx context.Context) []ServerStatus {
	out := make([]ServerStatus, 0, len(m.servers))
	for _, s := range m.servers {
		st := ServerStatus{Name: s.name, URL: s.client.URL()}
		ts, err := s.client.ListTools(ctx)
		switch {
		case errors.Is(err, ErrNoCredential):
			st.Err, st.Cause = err.Error(), "no-credential"
		case errors.Is(err, ErrCredentialRefused):
			st.Err, st.Cause = err.Error(), "credential-refused"
		case err != nil:
			st.Err = err.Error()
		default:
			st.Tools = len(ts)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
