// Package metrics defines the bot's Prometheus instrumentation. Labels are kept
// deliberately low-cardinality (bounded outcome/region/cluster enums, never a
// user email, tool name, namespace, or free text) so the series count stays
// flat as usage grows.
package metrics

import (
	"net/http"

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

	// ToolCalls counts MCP tool calls by cluster and outcome.
	ToolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "tool_calls_total",
		Help: "MCP tool calls by cluster and outcome (ok, error, denied, filtered).",
	}, []string{"cluster", "outcome"})

	// LLMRequests counts reasoning-model calls by outcome.
	LLMRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "llm_requests_total",
		Help: "LLM requests by outcome (ok, error).",
	}, []string{"outcome"})

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
		Messages, MessageDuration, TurnIterations, ToolCalls,
		LLMRequests, LLMDuration, AuthzRequests, AuthzDuration,
		ActiveConversations, Panics, InFlight,
	)
	return r
}()

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
		"rate_limited", "too_long", "empty_answer", "refreshed",
	} {
		Messages.WithLabelValues(o)
	}
	for _, o := range []string{"ok", "error"} {
		LLMRequests.WithLabelValues(o)
		for _, r := range regions {
			AuthzRequests.WithLabelValues(r, o)
		}
	}
	for _, c := range clusters {
		for _, o := range []string{"ok", "error", "denied", "filtered"} {
			ToolCalls.WithLabelValues(c, o)
		}
	}
}
