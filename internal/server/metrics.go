// Prometheus metrics, served on their own port so a public Ingress to /mcp never exposes them.
package server

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// No tenant label: under jwt its cardinality is unbounded, and it would name tenants to any scraper.
var (
	registry = prometheus.NewRegistry()

	toolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "keepsake_tool_calls_total",
		Help: "MCP tool calls by tool and outcome.",
	}, []string{"tool", "outcome"})

	toolDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "keepsake_tool_call_duration_seconds",
		Help:    "MCP tool call latency, including the wait for a connection slot.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tool"})
)

var outcomes = []string{"ok", "conflict", "tool_error", "unavailable", "error"}

func init() {
	registry.MustRegister(toolCalls, toolDuration, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	// Every series exists from the start, so rate() sees the first call.
	for name := range handlers {
		for _, o := range outcomes {
			toolCalls.WithLabelValues(name, o)
		}
		toolDuration.WithLabelValues(name)
	}
}

// metricsHandler serves the tool metrics and stat's pool gauges.
func metricsHandler(stat func() *pgxpool.Stat) http.Handler {
	pool := prometheus.NewRegistry()
	for name, f := range map[string]func(*pgxpool.Stat) int32{
		"acquired": (*pgxpool.Stat).AcquiredConns,
		"idle":     (*pgxpool.Stat).IdleConns,
		"total":    (*pgxpool.Stat).TotalConns,
		"max":      (*pgxpool.Stat).MaxConns,
	} {
		pool.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "keepsake_db_pool_" + name + "_connections",
			Help: "Postgres pool connections: " + name + ".",
		}, func() float64 { return float64(f(stat())) }))
	}
	return promhttp.HandlerFor(prometheus.Gatherers{registry, pool}, promhttp.HandlerOpts{})
}
