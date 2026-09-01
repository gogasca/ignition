// Package adminz is the control-plane admin surface: /healthz, /statusz, /rpcz
// and /metrics, served on a private listener (IGNITION_ADMIN_ADDR, default
// :9090) that is never exposed through the Ingress.
package adminz

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Section is a titled key/value table rendered on /statusz.
type Section struct {
	Title string
	Rows  [][2]string
}

// Options configure a Server.
type Options struct {
	Name     string // e.g. "ignition-api"
	Registry *prometheus.Registry
	Recorder *Recorder
	HealthFn func(context.Context) error // nil => always healthy
	Status   func() []Section            // nil => no extra sections
}

// Server renders the admin endpoints.
type Server struct {
	opts     Options
	started  time.Time
	revision string
}

// New builds an admin Server.
func New(opts Options) *Server {
	rev := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				rev = s.Value
			}
		}
	}
	return &Server{opts: opts, started: time.Now(), revision: rev}
}

// Handler returns the admin mux. It performs no authentication — bind it only to
// the private admin address.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /statusz", s.statusz)
	mux.HandleFunc("GET /rpcz", s.rpcz)
	mux.HandleFunc("GET /{$}", s.index)
	if s.opts.Registry != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.opts.Registry, promhttp.HandlerOpts{}))
	}
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.opts.HealthFn != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.opts.HealthFn(ctx); err != nil {
			http.Error(w, "unhealthy: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8><title>` + s.opts.Name + `</title>
<h1>` + s.opts.Name + `</h1><ul>
<li><a href="statusz">/statusz</a> — status + request stats</li>
<li><a href="rpcz">/rpcz</a> — recent requests</li>
<li><a href="metrics">/metrics</a> — Prometheus</li>
<li><a href="healthz">/healthz</a></li></ul>`))
}

type statuszData struct {
	Name     string
	Process  Section
	Health   string
	Sections []Section
	Routes   []RouteStat
}

func (s *Server) statusz(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	health := "ok"
	if s.opts.HealthFn != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.opts.HealthFn(ctx); err != nil {
			health = "UNHEALTHY: " + err.Error()
		}
	}
	d := statuszData{
		Name:   s.opts.Name,
		Health: health,
		Process: Section{Title: "process", Rows: [][2]string{
			{"uptime", time.Since(s.started).Round(time.Second).String()},
			{"started", s.started.UTC().Format(time.RFC3339)},
			{"go", runtime.Version()},
			{"revision", s.revision},
			{"goroutines", itoa(runtime.NumGoroutine())},
			{"heap", humanBytes(ms.HeapAlloc)},
			{"gc runs", itoa(int(ms.NumGC))},
		}},
	}
	if s.opts.Status != nil {
		d.Sections = s.opts.Status()
	}
	if s.opts.Recorder != nil {
		d.Routes = s.opts.Recorder.Aggregate()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statuszTmpl.Execute(w, d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) rpcz(w http.ResponseWriter, _ *http.Request) {
	var events []Event
	if s.opts.Recorder != nil {
		events = s.opts.Recorder.Snapshot()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := rpczTmpl.Execute(w, struct {
		Name   string
		Events []Event
	}{s.opts.Name, events}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Serve runs h on addr until ctx is cancelled, then shuts it down gracefully.
// Mirrors internal/sandboxinit.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

var funcs = template.FuncMap{"dur": func(d time.Duration) string { return d.Round(time.Microsecond).String() }}

var statuszTmpl = template.Must(template.New("statusz").Funcs(funcs).Parse(`<!doctype html><meta charset=utf-8>
<title>{{.Name}} statusz</title>
<style>body{font:13px system-ui,monospace;margin:1.5rem;max-width:70rem}
h2{margin:1.4rem 0 .3rem;font-size:1rem}table{border-collapse:collapse;width:100%}
td,th{border:1px solid #ccc;padding:2px 8px;text-align:left}th{background:#f4f4f4}
.bad{color:#b00}</style>
<h1>{{.Name}}</h1>
<p>health: {{if eq .Health "ok"}}<b>ok</b>{{else}}<b class=bad>{{.Health}}</b>{{end}}
 &nbsp;·&nbsp; <a href=rpcz>/rpcz</a> &nbsp;·&nbsp; <a href=metrics>/metrics</a></p>
{{define "sec"}}<h2>{{.Title}}</h2><table>{{range .Rows}}<tr><th>{{index . 0}}</th><td>{{index . 1}}</td></tr>{{end}}</table>{{end}}
{{template "sec" .Process}}
{{range .Sections}}{{template "sec" .}}{{end}}
{{if .Routes}}<h2>requests (retained window)</h2><table>
<tr><th>route</th><th>count</th><th>errors</th><th>p50</th><th>p90</th><th>p99</th><th>max</th></tr>
{{range .Routes}}<tr><td>{{.Route}}</td><td>{{.Count}}</td>
<td{{if .Errors}} class=bad{{end}}>{{.Errors}}</td>
<td>{{dur .P50}}</td><td>{{dur .P90}}</td><td>{{dur .P99}}</td><td>{{dur .Max}}</td></tr>{{end}}
</table>{{end}}`))

var rpczTmpl = template.Must(template.New("rpcz").Funcs(funcs).Parse(`<!doctype html><meta charset=utf-8>
<title>{{.Name}} rpcz</title>
<style>body{font:12px ui-monospace,monospace;margin:1.5rem}table{border-collapse:collapse}
td,th{border:1px solid #ccc;padding:1px 6px}th{background:#f4f4f4}.bad{color:#b00}</style>
<h1>{{.Name}} — recent ({{len .Events}})</h1>
<table><tr><th>time (UTC)</th><th>kind</th><th>method</th><th>route</th><th>status</th><th>code</th><th>dur</th><th>request-id</th><th>err</th></tr>
{{range .Events}}<tr{{if or (ge .Status 400) .Err}} class=bad{{end}}>
<td>{{.Time.UTC.Format "15:04:05.000"}}</td><td>{{.Kind}}</td><td>{{.Method}}</td><td>{{.Route}}</td>
<td>{{if .Status}}{{.Status}}{{end}}</td><td>{{.Code}}</td><td>{{dur .Dur}}</td><td>{{.RequestID}}</td><td>{{.Err}}</td></tr>
{{end}}</table>`))
