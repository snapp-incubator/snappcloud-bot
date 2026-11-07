package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/bestpractice"
)

// Teams ask here to leave a best-practice finding out of the reports. The name
// on a record is the sender's, from Mattermost, and each change is also posted
// to the admin channel, so the record does not rest on one file. These are
// commands, answered before the agent, so a model cannot make or change one.

// ignoreRe matches a message that may name an object. Only a second word of
// exactly four slash-separated parts makes it a command: "ignore this error and
// look at the pods" or "ignore kube-system/foo errors" is a question.
var ignoreRe = regexp.MustCompile(`(?is)^(un)?ignore\s+(\S+/\S+)(?:\s+(.*))?$`)

// reasonRe finds the reason after the rule.
var reasonRe = regexp.MustCompile(`(?is)^reason\s*:\s*(.*)$`)

const ignoreUsage = "Use: `ignore <cluster>/<namespace>/<kind>/<name> <rule> reason: <why>`. " +
	"The report's PDF gives the exact command for each finding."

const unignoreUsage = "Use: `unignore <cluster>/<namespace>/<kind>/<name> <rule>`. `ignores` lists what is ignored."

// ignoreCommand handles `ignore …`, `unignore …` and `ignores`.
func (s *Service) ignoreCommand(ctx context.Context, identity, query string, lg *slog.Logger) (bool, string) {
	if s.report == nil {
		return false, ""
	}
	switch normalizeCommand(query) {
	case "ignores", "ignore list", "ignored":
		return true, s.ignoreList(ctx, identity)
	}
	m := ignoreRe.FindStringSubmatch(strings.TrimSpace(query))
	if m == nil {
		return false, ""
	}
	revoke := m[1] != ""
	usage := ignoreUsage
	if revoke {
		usage = unignoreUsage
	}
	target := strings.Split(strings.ToLower(m[2]), "/")
	if len(target) != 4 {
		return false, ""
	}
	if slices.Contains(target, "") {
		return true, "❔ Name the object as `<cluster>/<namespace>/<kind>/<name>`. " + usage
	}
	rule, rest, _ := strings.Cut(strings.TrimSpace(m[3]), " ")
	rule = strings.ToLower(rule)
	if rule == "" {
		return true, "❔ Which rule? " + usage
	}
	if !slices.Contains(bestpractice.Rules, rule) {
		return true, fmt.Sprintf("❔ There is no rule `%s`. These can be ignored: `%s`.", rule,
			strings.Join(bestpractice.IgnorableRules(), "`, `"))
	}
	if !revoke && bestpractice.DrainBlocker(rule) {
		return true, fmt.Sprintf("🚫 `%s` cannot be ignored: a PDB that blocks node drains holds up maintenance "+
			"for every workload on the node, so it must be fixed.", rule)
	}

	// Access comes before the lookup, so the answer says nothing about what
	// exists in a namespace the sender cannot see.
	cluster, namespace := target[0], target[1]
	scope, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		lg.Error("authorize failed", "user", identity, "err", err)
		return true, msgBackendError
	}
	if !canAccess(scope, cluster, namespace) {
		return true, fmt.Sprintf("🔒 You have no access to namespace `%s` on `%s`, so you cannot change what is ignored there.",
			namespace, cluster)
	}

	key := bestpractice.IgnoreKey(cluster, namespace, target[2], target[3], rule)
	if revoke {
		return true, s.ignoreRevoke(ctx, identity, key, lg)
	}
	return true, s.ignoreAdd(ctx, identity, key, rest, lg)
}

func (s *Service) ignoreAdd(ctx context.Context, identity, key, rest string, lg *slog.Logger) string {
	rf := s.report
	m := reasonRe.FindStringSubmatch(strings.TrimSpace(rest))
	if m == nil {
		return "❔ Why is this safe? A reason is required, because it stays on the record. " + ignoreUsage
	}
	reason, err := bestpractice.CleanReason(m[1])
	if err != nil {
		return "❔ " + capitalize(err.Error()) + ". " + ignoreUsage
	}
	f, ok := rf.Store.FindFinding(key)
	if !ok {
		return "❔ No recent report has that finding, so nothing was recorded. Check the object and the rule against " +
			"the report's PDF. `ignores` lists what is already ignored."
	}

	now := rf.now()
	i := bestpractice.Ignore{Cluster: f.Cluster, Namespace: f.Namespace, Kind: f.Kind, Name: f.Name, Rule: f.Rule,
		By: identity, Reason: reason}
	renewed, err := rf.Store.AddIgnore(i, now)
	if err != nil {
		lg.Error("persist ignore", "user", identity, "key", key, "err", err)
		return "🚫 I could not save this, so the finding is **not** ignored. Please try again."
	}
	action := "ignored"
	if renewed {
		action = "renewed"
	}
	lg.Info("best-practice finding "+action, "user", identity, "key", key, "reason", reason)
	s.ignoreAudit(ctx, lg, fmt.Sprintf("🔕 %s %s `%s` `%s`. Reason: `%s`", identity, action, i.Target(), i.Rule, reason))

	return fmt.Sprintf("🔕 Recorded: **%s** %s `%s` `%s` until %s.\n\n"+
		"**The risk you are accepting:** %s\n\n"+
		"Your name and reason are on the record, and your team's admins see them in every report. "+
		"`unignore %s %s` reports it again.",
		identity, action, i.Target(), i.Rule, bestpractice.DateOf(now.Add(bestpractice.IgnoreTTL)), f.Detail, i.Target(), i.Rule)
}

func (s *Service) ignoreRevoke(ctx context.Context, identity, key string, lg *slog.Logger) string {
	rf := s.report
	i, err := rf.Store.RevokeIgnore(key, identity, rf.now())
	switch {
	case errors.Is(err, bestpractice.ErrNoIgnore):
		return "❔ That finding is not ignored. `ignores` lists what is."
	case err != nil:
		lg.Error("persist ignore revoke", "user", identity, "key", key, "err", err)
		return "🚫 I could not save this, so the finding is **still** ignored. Please try again."
	}
	lg.Info("best-practice ignore removed", "user", identity, "key", key)
	s.ignoreAudit(ctx, lg, fmt.Sprintf("🔔 %s removed the ignore on `%s` `%s`, set by %s.", identity, i.Target(), i.Rule, i.By))
	return fmt.Sprintf("🔔 `%s` `%s` is no longer ignored: the next report includes it.", i.Target(), i.Rule)
}

// ignoreList shows the active ignores in the sender's namespaces.
func (s *Service) ignoreList(ctx context.Context, identity string) string {
	scope, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		return msgBackendError
	}
	var list []bestpractice.Ignore
	for _, i := range s.report.Store.ActiveIgnores(s.report.now()) {
		if canAccess(scope, i.Cluster, i.Namespace) {
			list = append(list, i)
		}
	}
	if len(list) == 0 {
		return "Nothing is ignored in the namespaces you can access."
	}
	return "**Ignored in the namespaces you can access**\n\n" + bestpractice.IgnoreTable(list)
}

// ignoreAudit copies a change to the admin channel. A failure does not undo the
// change, which is already saved; it is logged instead.
func (s *Service) ignoreAudit(ctx context.Context, lg *slog.Logger, msg string) {
	if err := s.post(ctx, s.report.AdminChannel, "", msg); err != nil {
		lg.Error("post ignore to the admin channel", "msg", msg, "err", err)
	}
}

// canAccess reports whether a scope covers a namespace. The names come from a
// typed command, so case does not count.
func canAccess(scope authzclient.Scope, cluster, namespace string) bool {
	for c, cs := range scope {
		if !strings.EqualFold(c, cluster) {
			continue
		}
		if cs.ClusterWide {
			return true
		}
		return slices.ContainsFunc(cs.Namespaces, func(n string) bool { return strings.EqualFold(n, namespace) })
	}
	return false
}
