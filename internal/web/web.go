// Package web serves a read-mostly overview of a Herodotos world: the
// canonical timeline, regional conditions, and attempt diagnostics. The
// step action runs the simulation engine in this process, serialized by
// a mutex; the database itself is additionally protected by the
// single-writer lock.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/sim"
	"github.com/dnym/herodotos/internal/store"
)

//go:embed templates/*.tmpl static/*
var assets embed.FS

// Server exposes one world database over HTTP.
type Server struct {
	DB     *store.DB
	Engine *sim.Engine // nil when no inference is configured

	mu   sync.Mutex // one step at a time
	tmpl *template.Template
}

// ListenAndServe starts the overview server and shuts down on ctx end.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}).ParseFS(assets, "templates/*.tmpl")
	if err != nil {
		return fmt.Errorf("web templates: %w", err)
	}
	s.tmpl = tmpl

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutCtx)
	}()
	err = httpSrv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.pageOverview)
	mux.HandleFunc("GET /events", s.pageEvents)
	mux.HandleFunc("GET /attempts", s.pageAttempts)
	mux.HandleFunc("GET /attempt", s.pageAttempt)
	mux.HandleFunc("POST /step", s.actionStep)
	// embed paths keep the static/ prefix, so the URI maps directly.
	mux.Handle("GET /static/", http.FileServerFS(assets))
	return logRequests(mux)
}

// --- rendering ---

type pageData struct {
	Title    string
	Flash    string
	FlashErr bool
	Inner    any
}

func (s *Server) render(w http.ResponseWriter, page, title string, r *http.Request, inner any) {
	flash, flashErr := notice(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, page+".tmpl", pageData{
		Title: title, Flash: flash, FlashErr: flashErr, Inner: inner,
	}); err != nil {
		fmt.Println("web: render:", err) // headers may already be sent
	}
}

// --- pages ---

type overviewData struct {
	Snap    *store.Snapshot
	Date    string
	Pending int
	HasStep bool
}

func (s *Server) pageOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snap, err := s.DB.LoadSnapshot(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	date := fmt.Sprintf("clock step %d", snap.Meta.ClockStep)
	if t, err := snap.Meta.StepDate(snap.Meta.ClockStep); err == nil {
		date = fmt.Sprintf("clock step %d — %s", snap.Meta.ClockStep, t.Format("2 Jan 2006"))
	}
	od := overviewData{Snap: snap, HasStep: s.Engine != nil, Date: date}
	if reports, err := s.DB.ListReports(ctx); err == nil {
		for _, rp := range reports {
			if rp.Report.DeliveredStep == 0 {
				od.Pending++
			}
		}
	}
	s.render(w, "overview", snap.Meta.WorldName, r, od)
}

type eventsData struct {
	Events []eventRow
	Region string
	Offset int
	Limit  int
	More   bool
}

func (s *Server) pageEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	region := r.URL.Query().Get("region")
	if !domain.ValidID(region) {
		region = ""
	}
	evs, err := s.DB.ListEvents(ctx, store.EventQuery{RegionID: region, Limit: limit, Offset: offset})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]eventRow, 0, len(evs))
	for _, ev := range evs {
		rows = append(rows, eventRow{
			Event:    ev,
			Regions:  strings.Join(ev.RegionIDs, ", "),
			Entities: strings.Join(ev.EntityIDs, ", "),
			Causes:   strings.Join(ev.CauseIDs, ", "),
		})
	}
	s.render(w, "events", "Timeline", r, eventsData{
		Events: rows, Region: region, Offset: offset, Limit: limit,
		More: len(evs) == limit,
	})
}

type eventRow struct {
	Event              domain.Event
	Regions, Entities string
	Causes             string
}

type attemptsData struct {
	Attempts []domain.Attempt
	HasStep  bool
}

func (s *Server) pageAttempts(w http.ResponseWriter, r *http.Request) {
	attempts, err := s.DB.ListAttempts(r.Context(), 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "attempts", "Attempts", r, attemptsData{Attempts: attempts, HasStep: s.Engine != nil})
}

func (s *Server) pageAttempt(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !domain.ValidID(id) {
		http.Redirect(w, r, "/attempts", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	proposals, err := s.DB.ListProposals(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	calls, err := s.DB.ListModelCalls(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "attempt", "Attempt "+id, r, struct {
		ID        string
		Proposals []store.ProposalRow
		Calls     []store.ModelCallRow
	}{id, proposals, calls})
}

// --- step action ---

func (s *Server) actionStep(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		redirectFlash(w, r, "/attempts",
			"no inference configured: set HERODOTOS_INFERENCE_BASE_URL and HERODOTOS_INFERENCE_MODEL", true)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.Engine.RunStep(r.Context())
	if err != nil {
		redirectFlash(w, r, "/attempts", "step not committed: "+err.Error(), true)
		return
	}
	redirectFlash(w, r, "/", fmt.Sprintf(
		"step %d committed: %d event(s), %d model call(s)", res.Step, len(res.EventIDs), res.ModelCalls), false)
}

// --- helpers ---

func redirectFlash(w http.ResponseWriter, r *http.Request, path, msg string, isErr bool) {
	q := url.Values{}
	if isErr {
		msg = "! " + msg // notice() reads the "!" prefix as the error flag
	}
	q.Set("flash", msg)
	http.Redirect(w, r, path+"?"+q.Encode(), http.StatusSeeOther)
}

func notice(r *http.Request) (string, bool) {
	msg := r.URL.Query().Get("flash")
	if msg == "" {
		return "", false
	}
	if strings.HasPrefix(msg, "! ") {
		return msg[2:], true
	}
	return msg, false
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			fmt.Printf("web: %s %s (%s)\n", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
		}
	})
}
