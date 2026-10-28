package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Some tools change something (a debug pod is created), so they run only after
// the person asking approved that exact use. The model calls the tool when it
// decides it needs it; without approval the call is recorded as a proposal and
// refused, the model asks, and an approving reply grants it for the next turn.

// Proposal is a refused call to an approval tool: what the user is asked to allow.
type Proposal struct {
	Tool      string
	Cluster   string
	Namespace string
	Reason    string
}

// Approval carries one turn's grants and collects its proposals.
type Approval struct {
	mu       sync.Mutex
	granted  map[string]bool
	proposed []Proposal
}

// NewApproval returns an Approval granting the given proposals.
func NewApproval(granted ...Proposal) *Approval {
	a := &Approval{granted: map[string]bool{}}
	for _, p := range granted {
		a.granted[approvalKey(p.Tool, p.Cluster, p.Namespace)] = true
	}
	return a
}

// Proposed returns the proposals made this turn, latest last.
func (a *Approval) Proposed() []Proposal {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Proposal(nil), a.proposed...)
}

// Allows reports whether the turn holds a grant for p.
func (a *Approval) Allows(p Proposal) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.granted[approvalKey(p.Tool, p.Cluster, p.Namespace)]
}

// Propose records p as asked for.
func (a *Approval) Propose(p Proposal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, q := range a.proposed {
		if q.Tool == p.Tool && q.Cluster == p.Cluster && q.Namespace == p.Namespace {
			return
		}
	}
	a.proposed = append(a.proposed, p)
}

func approvalKey(tool, cluster, ns string) string { return tool + "|" + cluster + "|" + ns }

type approvalKeyT struct{}

// WithApproval makes approval tools usable in a turn. A turn without one (the
// HTTP API, a schedule) cannot ask anybody, so its approval tools never run.
func WithApproval(ctx context.Context, a *Approval) context.Context {
	return context.WithValue(ctx, approvalKeyT{}, a)
}

// ApprovalFrom returns the turn's Approval, or nil.
func ApprovalFrom(ctx context.Context) *Approval {
	a, _ := ctx.Value(approvalKeyT{}).(*Approval)
	return a
}

// needsApproval refuses an approval tool's call unless this turn holds a
// grant for its tool, cluster and namespace. It returns the refusal or "".
func (a *Agent) needsApproval(ctx context.Context, b binding, args map[string]any) string {
	if !a.enforcer.RequiresApproval(b.real) {
		return ""
	}
	ns := strings.Join(a.enforcer.Namespaces(b.real, args), ",")
	p := Proposal{Tool: b.real, Cluster: b.ct.Cluster, Namespace: ns}
	p.Reason, _ = args["reason"].(string)
	ap := ApprovalFrom(ctx)
	if ap == nil {
		return fmt.Sprintf("%s was NOT called: it needs the user's approval, and nobody can approve it here. "+
			"Say what it would have checked and what the user can run instead.", b.real)
	}
	if ap.Allows(p) {
		return ""
	}
	ap.Propose(p)
	return fmt.Sprintf("%s was NOT called: it needs the user's approval first. Finish your answer with what you "+
		"found so far, then ask the user to approve it: name the cluster (%s), the namespace (%s) and exactly what "+
		"you would check with it, and tell them to reply \"yes\" to approve. Do not call it again in this answer.",
		b.real, b.ct.Cluster, ns)
}
