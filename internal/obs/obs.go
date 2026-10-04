// Package obs provides structured logging and metrics.
//
// One logger, reached through the context, with redaction wired into the
// handler rather than into call sites. A rule that says "do not log the
// address" is obeyed until the night somebody is debugging a stuck account at
// two in the morning; a handler that masks on the way out is obeyed always.
//
// Metrics are passed explicitly instead of living in package state so tests can
// build an isolated registry and assert on a counter.
package obs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewLogger builds the process logger, writing to stdout. level is
// debug|info|warn|error and format is json|text; internal/config validates
// both before they arrive.
func NewLogger(level, format string) *slog.Logger {
	return NewLoggerTo(os.Stdout, level, format)
}

// NewLoggerTo is NewLogger writing to w, with the same redaction, so a test
// can read what the process would have written.
func NewLoggerTo(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level), ReplaceAttr: redactAttr}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type loggerKey struct{}

// WithLogger returns a context carrying lg.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, lg)
}

// LoggerFrom returns the context's logger, never nil. A handler that has to
// check for nil before logging is a handler that eventually does not log.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if lg, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && lg != nil {
		return lg
	}
	return slog.Default()
}

// Metrics holds every collector the process exports.
type Metrics struct {
	reg *prometheus.Registry

	// Sync engine.
	IMAPSessions    *prometheus.GaugeVec   // account, role
	IMAPCommands    *prometheus.CounterVec // account, command, outcome
	SyncPasses      *prometheus.CounterVec // account, folder, outcome
	SyncDuration    *prometheus.HistogramVec
	IdleEvents      *prometheus.CounterVec // account, kind
	Reconnects      *prometheus.CounterVec // account, reason
	DownloadBytes   *prometheus.CounterVec // account
	AccountState    *prometheus.GaugeVec   // account, state
	MessagesIndexed *prometheus.CounterVec // account, folder

	// Delivery.
	Events            *prometheus.CounterVec // type
	WebhookDeliveries *prometheus.CounterVec // outcome
	Sends             *prometheus.CounterVec // account, outcome

	// Transports.
	HTTPRequests *prometheus.CounterVec // route, method, status
	HTTPDuration *prometheus.HistogramVec
	AuthFailures *prometheus.CounterVec // reason
}

// NewMetrics registers every collector on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	f := promauto(reg)
	return &Metrics{
		reg:               reg,
		IMAPSessions:      f.gauge("imap_sessions", "Open IMAP sessions.", "account", "role"),
		IMAPCommands:      f.counter("imap_commands_total", "IMAP commands issued.", "account", "command", "outcome"),
		SyncPasses:        f.counter("sync_passes_total", "Folder synchronisation passes.", "account", "folder", "outcome"),
		SyncDuration:      f.histogram("sync_pass_seconds", "Duration of a folder synchronisation pass.", []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}, "account"),
		IdleEvents:        f.counter("idle_events_total", "Unilateral IMAP data seen on the IDLE connection.", "account", "kind"),
		Reconnects:        f.counter("reconnects_total", "IMAP reconnections, by classified reason.", "account", "reason"),
		DownloadBytes:     f.counter("download_bytes_total", "Bytes fetched from IMAP. Gmail caps this at 2.5 GB per day.", "account"),
		AccountState:      f.gauge("account_state", "1 for the account's current state, 0 otherwise.", "account", "state"),
		MessagesIndexed:   f.counter("messages_indexed_total", "Messages written to the local index.", "account", "folder"),
		Events:            f.counter("events_total", "Events appended to the journal.", "type"),
		WebhookDeliveries: f.counter("webhook_deliveries_total", "Webhook delivery attempts.", "outcome"),
		Sends:             f.counter("sends_total", "Message submissions, by outcome.", "account", "outcome"),
		HTTPRequests:      f.counter("http_requests_total", "HTTP requests served.", "route", "method", "status"),
		HTTPDuration:      f.histogram("http_request_seconds", "HTTP request duration.", []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}, "route"),
		AuthFailures:      f.counter("auth_failures_total", "Rejected credentials, by reason.", "reason"),
	}
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the registry so tests can gather it.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

const namespace = "mailserver"

type factory struct{ reg *prometheus.Registry }

func promauto(reg *prometheus.Registry) factory { return factory{reg} }

func (f factory) counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	f.reg.MustRegister(c)
	return c
}

func (f factory) gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	f.reg.MustRegister(g)
	return g
}

func (f factory) histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets}, labels)
	f.reg.MustRegister(h)
	return h
}
