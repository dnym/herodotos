// Package web serves a Herodotos world: the canonical timeline, regional
// conditions, attempt diagnostics, run control, and structured export.
// Steps execute in server-managed background runs, serialized by a mutex;
// closing a browser tab never terminates a run. The database itself is
// additionally protected by the single-writer lock.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/sim"
	"github.com/dnym/herodotos/internal/store"
)

//go:embed templates/*.tmpl all:static
var assets embed.FS

// Server exposes one world database over HTTP.
type Server struct {
	DB     *store.DB
	Engine *sim.Engine // nil when no inference configured

	stepMu sync.Mutex // one engine step at a time
	runMu  sync.Mutex // guards run
	run    *runState  // current or most recent background run

	// runCtx is the server lifetime context for background steps: a
	// request ending must never cancel a step, but shutting the server
	// down should.
	runCtx context.Context

	tmpl *template.Template
}

// runState is the observable state of one background run.
type runState struct {
	Active         bool
	Until          int // remaining steps in this run; -1 runs until stopped
	PauseRequested bool
	StepsDone      int
	LastResult     string
	LastErr        string
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

	runCtx, cancelRun := context.WithCancel(ctx)
	s.runCtx = runCtx
	defer cancelRun()

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
	mux.HandleFunc("GET /event", s.pageEvent)
	mux.HandleFunc("GET /attempts", s.pageAttempts)
	mux.HandleFunc("GET /attempt", s.pageAttempt)
	mux.HandleFunc("GET /run/panel", s.partialRunPanel)
	mux.HandleFunc("GET /export", s.pageExport)
	// Every mutation must be same-origin: the server never sets CORS
	// allow headers, and a carried Origin must match the Host. That is
	// the CSRF posture for a local, credential-free tool.
	mutation := func(h http.HandlerFunc) http.Handler { return sameOrigin(h) }
	mux.Handle("POST /step", mutation(s.actionStartRun)) // alias: one step
	mux.Handle("POST /run/start", mutation(s.actionStartRun))
	mux.Handle("POST /run/pause", mutation(s.actionPauseRun))
	mux.Handle("POST /run/resume", mutation(s.actionResumeWorld))
	mux.Handle("POST /directive/edit", mutation(s.actionDirectiveEdit))
	mux.Handle("POST /directive/add", mutation(s.actionDirectiveAdd))
	// embed paths keep the static/ prefix, so the URI maps directly.
	mux.Handle("GET /static/", http.FileServerFS(assets))
	return logRequests(mux)
}

// sameOrigin rejects cross-origin mutations (FR: mutation endpoints must
// not accept cross-origin requests, even locally).
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !originMatches(origin, r.Host) {
			http.Error(w, "cross-origin mutation refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originMatches(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, host)
}

// --- background runs ---

// beginRun starts a background run of the given number of steps
// (-1 = until stopped). One run at a time; step boundaries are the only
// pause points, which keeps every step atomic.
func (s *Server) beginRun(steps int) error {
	if s.runCtx != nil && s.runCtx.Err() != nil {
		return errors.New("server is shutting down")
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.run != nil && s.run.Active {
		return errors.New("a run is already in progress")
	}
	rs := &runState{Active: true, Until: steps}
	s.run = rs
	go s.runLoop(rs)
	return nil
}

func (s *Server) runLoop(rs *runState) {
	ctx := s.runCtx
	if ctx == nil {
		ctx = context.Background() // direct route wiring in tests
	}
	for {
		s.runMu.Lock()
		pauseRequested := rs.PauseRequested
		s.runMu.Unlock()
		if pauseRequested {
			s.finishRun(rs, fmt.Sprintf("run stopped at step boundary after %d step(s)", rs.StepsDone))
			return
		}
		if reason, paused, err := s.DB.WorldPause(ctx); err == nil && paused {
			s.finishRun(rs, "the world is paused: "+reason)
			return
		}

		s.stepMu.Lock()
		res, err := s.Engine.RunStep(ctx)
		s.stepMu.Unlock()

		s.runMu.Lock()
		rs.StepsDone++
		if err != nil {
			rs.Active = false
			rs.LastErr = err.Error()
			s.runMu.Unlock()
			return
		}
		rs.LastResult = fmt.Sprintf("step %d committed: %d event(s), %d model call(s)",
			res.Step, len(res.EventIDs), res.ModelCalls)
		rs.LastErr = ""
		if rs.Until > 0 {
			rs.Until--
			if rs.Until == 0 {
				rs.Active = false
				s.runMu.Unlock()
				return
			}
		}
		s.runMu.Unlock()
	}
}

func (s *Server) finishRun(rs *runState, result string) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	rs.Active = false
	rs.LastResult = result
}

// --- run panel ---

type panelData struct {
	HasEngine  bool
	WorldPause string // "" when not durably paused
	Active     bool
	PauseReq   bool
	StepsDone  int
	RunningID  string // attempt id of the step in flight, for the live viewer
	LastResult string
	LastErr    string
}

func (s *Server) panelData(ctx context.Context) panelData {
	pd := panelData{HasEngine: s.Engine != nil}
	if reason, paused, err := s.DB.WorldPause(ctx); err == nil && paused {
		pd.WorldPause = reason
	}
	if id, running, err := s.DB.RunningAttempt(ctx); err == nil && running {
		pd.RunningID = id
	}
	s.runMu.Lock()
	if s.run != nil {
		pd.Active = s.run.Active
		pd.PauseReq = s.run.PauseRequested
		pd.StepsDone = s.run.StepsDone
		pd.LastResult = s.run.LastResult
		pd.LastErr = s.run.LastErr
	}
	s.runMu.Unlock()
	return pd
}

func (s *Server) partialRunPanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "runpanel", s.panelData(r.Context())); err != nil {
		fmt.Println("web: render runpanel:", err)
	}
}

// --- actions ---

func (s *Server) actionStartRun(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		redirectFlash(w, r, "/attempts",
			"no inference configured: set HERODOTOS_INFERENCE_BASE_URL and HERODOTOS_INFERENCE_MODEL", true)
		return
	}
	if reason, paused, err := s.DB.WorldPause(r.Context()); err == nil && paused {
		redirectFlash(w, r, "/", "the world is paused: "+reason+" — resolve and resume first", true)
		return
	}
	steps := stepsFormValue(r.FormValue("steps"))
	if err := s.beginRun(steps); err != nil {
		redirectFlash(w, r, "/", err.Error(), true)
		return
	}
	switch {
	case steps < 0:
		redirectFlash(w, r, "/", "run started: stepping until stopped — closing this page does not stop it", false)
	case steps == 1:
		redirectFlash(w, r, "/", "run started: one step — closing this page does not stop it", false)
	default:
		redirectFlash(w, r, "/", fmt.Sprintf("run started: %d steps — closing this page does not stop it", steps), false)
	}
}

func (s *Server) actionPauseRun(w http.ResponseWriter, r *http.Request) {
	s.runMu.Lock()
	if s.run != nil && s.run.Active {
		s.run.PauseRequested = true
		s.runMu.Unlock()
		redirectFlash(w, r, "/", "pause requested: the run stops at the next step boundary", false)
		return
	}
	s.runMu.Unlock()
	redirectFlash(w, r, "/", "no run is in progress", true)
}

func (s *Server) actionResumeWorld(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.ResumeWorld(r.Context()); err != nil {
		redirectFlash(w, r, "/", "resume failed: "+err.Error(), true)
		return
	}
	redirectFlash(w, r, "/", "the world is resumed — stepping can continue", false)
}

// stepsFormValue maps the run form's steps field to a step count.
func stepsFormValue(v string) int {
	switch v {
	case "all":
		return -1
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 1
}

// --- directives ---

func (s *Server) actionDirectiveEdit(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	text := strings.TrimSpace(r.FormValue("text"))
	if !domain.ValidID(id) || text == "" || len(text) > 500 {
		redirectFlash(w, r, "/", "a directive needs a valid id and 1-500 characters of text", true)
		return
	}
	ok, err := s.DB.UpdateDirectiveText(r.Context(), domain.ID(id), text)
	if err != nil {
		redirectFlash(w, r, "/", "update failed: "+err.Error(), true)
		return
	}
	if !ok {
		redirectFlash(w, r, "/", "no such directive: "+id, true)
		return
	}
	redirectFlash(w, r, "/", "directive "+id+" updated", false)
}

func (s *Server) actionDirectiveAdd(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" || len(text) > 500 {
		redirectFlash(w, r, "/", "a directive needs 1-500 characters of text", true)
		return
	}
	hard := r.FormValue("hard") == "1"
	id, err := s.DB.CreateDirective(r.Context(), text, hard)
	if err != nil {
		redirectFlash(w, r, "/", "could not add directive: "+err.Error(), true)
		return
	}
	kind := "soft"
	if hard {
		kind = "hard"
	}
	redirectFlash(w, r, "/", "directive "+id+" added ("+kind+")", false)
}

// --- export ---

func (s *Server) pageExport(w http.ResponseWriter, r *http.Request) {
	b, err := s.DB.ExportBundle(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="herodotos-%s-step%d.json"`, b.Meta.WorldID, b.Meta.ClockStep))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		fmt.Println("web: export:", err)
	}
}

// --- rendering ---

type pageData struct {
	Title    string
	Section  string // highlights the current nav entry
	Flash    string
	FlashErr bool
	Inner    any
}

func (s *Server) render(w http.ResponseWriter, page, title string, r *http.Request, inner any) {
	flash, flashErr := notice(r)
	section := page
	if page == "attempt" {
		section = "attempts"
	}
	if page == "event" {
		section = "events"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, page+".tmpl", pageData{
		Title: title, Section: section, Flash: flash, FlashErr: flashErr, Inner: inner,
	}); err != nil {
		fmt.Println("web: render:", err) // headers may already be sent
	}
}

// stepDate formats the in-world date of a step, or "" when the calendar
// cannot cover it.
func stepDate(meta domain.Meta, step int) string {
	t, err := meta.StepDate(step)
	if err != nil {
		return ""
	}
	return t.Format("2 Jan 2006")
}

// --- pages ---

type overviewData struct {
	Snap    *store.Snapshot
	Date    string
	Pending int
	Panel   panelData
	Attrs   map[domain.ID][]string // region id -> "key: value" lines
}

// attrLines renders a map as deterministic "key: value" lines; Go map
// syntax in the UI is noise.
func attrLines(attrs map[string]any) []string {
	var lines []string
	for _, k := range sortedKeys(attrs) {
		lines = append(lines, fmt.Sprintf("%s: %v", k, attrs[k]))
	}
	return lines
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) pageOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snap, err := s.DB.LoadSnapshot(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	date := stepDate(snap.Meta, snap.Meta.ClockStep)
	if date == "" {
		date = "—"
	}
	od := overviewData{Snap: snap, Date: date, Attrs: map[domain.ID][]string{}, Panel: s.panelData(ctx)}
	for id, rg := range snap.Regions {
		od.Attrs[id] = attrLines(rg.Attrs)
	}
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
	Groups []stepGroup
	Region string
	Offset int
	Limit  int
	More   bool
}

// stepGroup bundles one step's events under its in-world date.
type stepGroup struct {
	Step   int
	Date   string
	Events []eventRow
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
	meta, err := s.DB.GetMeta(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var groups []stepGroup
	lastStep := -1
	for _, ev := range evs { // newest first
		if len(groups) == 0 || ev.Step != lastStep {
			groups = append(groups, stepGroup{Step: ev.Step, Date: stepDate(meta, ev.Step)})
			lastStep = ev.Step
		}
		g := &groups[len(groups)-1]
		g.Events = append(g.Events, eventRow{
			Event:    ev,
			Regions:  ev.RegionIDs,
			Entities: ev.EntityIDs,
			Causes:   ev.CauseIDs,
		})
	}
	s.render(w, "events", "Timeline", r, eventsData{
		Groups: groups, Region: region, Offset: offset, Limit: limit,
		More: len(evs) == limit,
	})
}

type eventRow struct {
	Event             domain.Event
	Regions, Entities []domain.ID
	Causes            []domain.ID
}

func (s *Server) pageEvent(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !domain.ValidID(id) {
		http.Redirect(w, r, "/events", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	ev, err := s.DB.GetEvent(ctx, domain.ID(id))
	if err != nil {
		http.Error(w, "no such event", http.StatusNotFound)
		return
	}
	meta, _ := s.DB.GetMeta(ctx)
	changes, err := s.DB.ListStateChanges(ctx, ev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	allReports, _ := s.DB.ListReports(ctx)
	var reports []store.ReportRow
	for _, rp := range allReports {
		if rp.Report.EventID == ev.ID {
			reports = append(reports, rp)
		}
	}
	s.render(w, "event", ev.Title, r, struct {
		Event       domain.Event
		Date        string
		Changes     []store.StateChangeRow
		Reports     []store.ReportRow
		RegionNames map[domain.ID]string
	}{
		Event: ev, Date: stepDate(meta, ev.Step),
		Changes: changes, Reports: reports,
		RegionNames: regionNames(ctx, s.DB),
	})
}

func regionNames(ctx context.Context, db *store.DB) map[domain.ID]string {
	out := map[domain.ID]string{}
	if regions, err := db.ListRegions(ctx); err == nil {
		for _, rg := range regions {
			out[rg.ID] = rg.Name
		}
	}
	return out
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
	attempt, err := s.DB.GetAttempt(ctx, domain.ID(id))
	if err != nil {
		http.Error(w, "no such attempt", http.StatusNotFound)
		return
	}
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
	// Responses are audit artifacts, not world prose: fold them away, but
	// keep them available — this page is also the live debugging view.
	for i := range calls {
		if len(calls[i].Response) > 6000 {
			calls[i].Response = calls[i].Response[:6000] + "\n…[truncated for display]"
		}
	}
	s.render(w, "attempt", "Attempt "+id, r, struct {
		Attempt   domain.Attempt
		Proposals []store.ProposalRow
		Calls     []store.ModelCallRow
	}{attempt, proposals, calls})
}

// --- helpers ---

// redirectFlash sends the browser to a page carrying a one-shot notice
// in the query string.
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
