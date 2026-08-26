// Package metrics defines the bot's Prometheus instrumentation. Labels are kept
// deliberately low-cardinality (bounded outcome/region/cluster enums, never a
// user email, tool name, namespace, or free text) so the series count stays
// flat as usage grows.
package metrics

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "snappcloud_bot"

var (
	// Messages is the count of handled Mattermost messages by terminal outcome.
	Messages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "messages_total",
		Help: "Handled messages by outcome (answered, denied, unauthorized, backend_error, agent_error, panic, ignored).",
	}, []string{"outcome"})

	// APIRequests counts HTTP query-API requests by outcome (the programmatic
	// entry point; Mattermost traffic is counted by Messages).
	APIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "api_requests_total",
		Help: "HTTP query API requests by outcome.",
	}, []string{"outcome"})

	// MessageDuration is end-to-end handling latency (auth + agent + reply).
	MessageDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "message_duration_seconds",
		Help:    "End-to-end message handling latency.",
		Buckets: []float64{.5, 1, 2, 5, 10, 20, 30, 60, 120, 300},
	})

	// TurnIterations is the agent tool-calling loop depth per answered turn.
	TurnIterations = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "turn_iterations",
		Help:    "Agent loop iterations per turn.",
		Buckets: prometheus.LinearBuckets(1, 1, 12),
	})

	// ToolCalls counts MCP tool calls by cluster, tool, and outcome. The tool
	// label is bounded (the fixed tool set the MCP servers advertise, never user
	// input), so cardinality stays flat.
	ToolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "tool_calls_total",
		Help: "MCP tool calls by cluster, tool and outcome (ok, error, denied, filtered).",
	}, []string{"cluster", "tool", "outcome"})

	// ToolErrors counts failing tool calls by a CLASSIFIED reason, so a broken
	// MCP server is diagnosable from metrics alone (timeout vs not_found vs
	// bad_args vs server_error) without reading logs.
	ToolErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "tool_errors_total",
		Help: "Failed MCP tool calls by cluster, tool and classified reason.",
	}, []string{"cluster", "tool", "reason"})

	// ToolDuration is per-call latency, labeled by cluster only (per-tool
	// histograms would multiply series by the whole tool set).
	ToolDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "tool_call_duration_seconds",
		Help:    "MCP tool call latency by cluster.",
		Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"cluster"})

	// LLMRequests counts reasoning-model calls by outcome.
	LLMRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "llm_requests_total",
		Help: "LLM requests by outcome (ok, error).",
	}, []string{"outcome"})

	// LLMByModel splits LLM requests across the primary and backup models, so a
	// silent, prolonged failover is visible.
	LLMByModel = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "llm_model_requests_total",
		Help: "LLM requests by model role (primary, backup) and outcome.",
	}, []string{"role", "outcome"})

	// LLMFailover counts circuit-breaker transitions (open = switched to backup,
	// closed = primary recovered).
	LLMFailover = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "llm_failover_total",
		Help: "LLM failover circuit-breaker transitions (open, closed).",
	}, []string{"state"})

	// LLMDuration is per-LLM-request latency.
	LLMDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "llm_request_duration_seconds",
		Help:    "LLM request latency.",
		Buckets: []float64{.5, 1, 2, 5, 10, 20, 30, 60, 120},
	})

	// AuthzRequests counts per-region mcp-authz scope lookups by outcome.
	AuthzRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "authz_requests_total",
		Help: "Per-region mcp-authz scope lookups by outcome (ok, error).",
	}, []string{"region", "outcome"})

	// AuthzDuration is the aggregate scope-resolution latency (all regions).
	AuthzDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "authz_duration_seconds",
		Help:    "Aggregate per-user scope resolution latency (all regions).",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
	})

	// ActiveConversations is the number of live conversation transcripts held.
	ActiveConversations = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "active_conversations",
		Help: "Conversation transcripts currently retained in memory.",
	})

	// Panics counts recovered handler panics (each one = a message that crashed
	// mid-handling but did not take the process down).
	Panics = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "handler_panics_total",
		Help: "Recovered per-message handler panics.",
	})

	// InFlight is the number of messages being handled right now.
	InFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "messages_in_flight",
		Help: "Messages currently being handled.",
	})
)

// registry holds every bot collector plus Go/process runtime metrics.
var registry = func() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		Messages, APIRequests, MessageDuration, TurnIterations, ToolCalls, ToolErrors, ToolDuration,
		LLMRequests, LLMByModel, LLMFailover, LLMDuration, AuthzRequests, AuthzDuration,
		ActiveConversations, Panics, InFlight,
	)
	return r
}()

// ClassifyToolError maps an MCP tool error to a small, bounded reason label so
// failures are aggregatable. Unknown shapes fall back to "other".
func ClassifyToolError(err string) string {
	e := strings.ToLower(err)
	switch {
	case strings.Contains(e, "context deadline") || strings.Contains(e, "timeout") || strings.Contains(e, "timed out"):
		return "timeout"
	case strings.Contains(e, "connection refused") || strings.Contains(e, "no such host") ||
		strings.Contains(e, "no route to host") || strings.Contains(e, "eof") ||
		strings.Contains(e, "connection reset"):
		return "unreachable"
	case strings.Contains(e, "401") || strings.Contains(e, "403") || strings.Contains(e, "unauthorized") ||
		strings.Contains(e, "forbidden"):
		return "auth"
	case strings.Contains(e, "not found") || strings.Contains(e, "404"):
		return "not_found"
	case strings.Contains(e, "required") || strings.Contains(e, "invalid") ||
		strings.Contains(e, "unmarshal") || strings.Contains(e, "decode") ||
		strings.Contains(e, "parse"):
		return "bad_args"
	case strings.Contains(e, "500") || strings.Contains(e, "502") || strings.Contains(e, "503") ||
		strings.Contains(e, "internal"):
		return "server_error"
	default:
		return "other"
	}
}

// Handler serves the metrics registry (mount at /metrics).
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// Init zero-initializes the known label values so the series exist (at 0) from
// startup. Without it a counter only appears after its first event, and every
// dashboard panel reads "No data" on a freshly deployed, idle bot.
func Init(clusters, regions []string) {
	for _, o := range []string{
		"answered", "denied", "unauthorized", "backend_error", "agent_error",
		"rate_limited", "too_long", "empty_answer", "refreshed", "ignored",
	} {
		Messages.WithLabelValues(o)
	}
	for _, o := range []string{"answered", "unauthorized", "rate_limited", "too_long", "bad_request", "agent_error"} {
		APIRequests.WithLabelValues(o)
	}
	for _, o := range []string{"ok", "error"} {
		LLMRequests.WithLabelValues(o)
		LLMByModel.WithLabelValues("primary", o)
		LLMByModel.WithLabelValues("backup", o)
		for _, r := range regions {
			AuthzRequests.WithLabelValues(r, o)
		}
	}
	// Tool names are discovered from the MCP servers at runtime, so tool-labeled
	// series cannot be pre-created; only the latency histogram is seeded.
	for _, c := range clusters {
		ToolDuration.WithLabelValues(c)
	}
}
