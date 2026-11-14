package web

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/inference"
	"github.com/dnym/herodotos/internal/sim"
	"github.com/dnym/herodotos/internal/store"
)

const quietKing = `{"prose":"q","events":[]}`
const quietPrin = `{"prose":"q","events":[]}`
const quietOrch = `{"notes":"quiet step","events":[]}`

func quietStep() []inference.Reply {
	return []inference.Reply{
		{Role: "logographer:king-a", Text: quietKing},
		{Role: "logographer:prin-b", Text: quietPrin},
		{Role: "orchestrator", Text: quietOrch},
	}
}

func newTestServer(t *testing.T, engine bool) *Server {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	w := &store.WorldSpec{Name: "Web World", StartDate: "1000-01-01", StepDays: 7}
	w.Regions = []struct {
		ID    domain.ID      `json:"id"`
		Name  string         `json:"name"`
		Attrs map[string]any `json:"attrs,omitempty"`
	}{
		{ID: "king-a", Name: "Kingdom A", Attrs: map[string]any{"unrest": "low"}},
		{ID: "prin-b", Name: "Principality B"},
	}
	w.Connections = []struct {
		A         domain.ID `json:"a"`
		B         domain.ID `json:"b"`
		TravelDay int       `json:"travel_days"`
	}{{A: "king-a", B: "prin-b", TravelDay: 4}}
	w.Entities = []struct {
		ID       domain.ID      `json:"id"`
		Kind     string         `json:"kind"`
		Name     string         `json:"name"`
		RegionID domain.ID      `json:"region_id,omitempty"`
		Attrs    map[string]any `json:"attrs,omitempty"`
	}{
		{ID: "q-elinor", Kind: "person", Name: "Queen Elinor", RegionID: "king-a",
			Attrs: map[string]any{"heir": "yes"}},
	}
	if err := db.InitWorld(ctx, w); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db}
	if engine {
		fake := &inference.Scripted{Replies: quietStep()}
		s.Engine = &sim.Engine{DB: db, Client: fake, ModelName: "fake"}
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}).ParseFS(assets, "templates/*.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	s.tmpl = tmpl
	return s
}

// slowClient delays each model call so tests can act while a step runs.
type slowClient struct {
	inner *inference.Scripted
	delay time.Duration
}

func (c *slowClient) Complete(ctx context.Context, req inference.Request) (inference.Response, error) {
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return inference.Response{}, ctx.Err()
	}
	return c.inner.Complete(ctx, req)
}

// --- http helpers ---

var noRedirect = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func get(t *testing.T, ts *httptest.Server, path string, want int) string {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s: status %d body %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func hxGet(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HX GET %s: status %d body %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func postForm(t *testing.T, ts *httptest.Server, path string, vals url.Values, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(vals.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func flashOf(t *testing.T, resp *http.Response) (string, bool) {
	t.Helper()
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("no Location header, status %d", resp.StatusCode)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	msg := u.Query().Get("flash")
	return strings.TrimPrefix(msg, "! "), strings.HasPrefix(msg, "! ")
}

// waitRunIdle waits for the background run to finish and returns its state.
func waitRunIdle(t *testing.T, s *Server) panelData {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		pd := s.panelData(context.Background())
		if !pd.Active {
			return pd
		}
		if time.Now().After(deadline) {
			t.Fatalf("run still active: %+v", pd)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitAttempts waits until at least n attempt rows exist, i.e. a step has
// truly begun; pausing after that can only take effect at the next boundary.
func waitAttempts(t *testing.T, s *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		attempts, err := s.DB.ListAttempts(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(attempts) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d attempts appeared", len(attempts))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- tests ---

func TestPagesRender(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body := get(t, ts, "/", http.StatusOK)
	for _, want := range []string{"Web World", "Kingdom A", "advance one step", "run until stopped", "unrest", "/export"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	get(t, ts, "/events", http.StatusOK)
	get(t, ts, "/events?region=king-a", http.StatusOK)
	get(t, ts, "/attempts", http.StatusOK)
	get(t, ts, "/attempt?id=att-000001", http.StatusNotFound) // no attempts yet
	get(t, ts, "/run/panel", http.StatusOK)
	get(t, ts, "/nope", http.StatusNotFound)
	get(t, ts, "/static/style.css", http.StatusOK)
	get(t, ts, "/static/htmx.min.js", http.StatusOK)
	get(t, ts, "/event?id=evt-000001", http.StatusNotFound) // nothing committed yet
}

func TestRunStartsInBackgroundAndSurvivesClient(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	// POST /step only starts the run; the request returns immediately and
	// the step continues regardless of what the client does next.
	resp := postForm(t, ts, "/step", url.Values{}, "http://"+ts.Listener.Addr().String())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start status = %d", resp.StatusCode)
	}
	msg, isErr := flashOf(t, resp)
	if isErr || !strings.Contains(msg, "run started") {
		t.Fatalf("flash = %q (err=%v)", msg, isErr)
	}

	pd := waitRunIdle(t, s)
	if !strings.Contains(pd.LastResult, "step 1 committed") {
		t.Fatalf("run result: %+v", pd)
	}
	meta, _ := s.DB.GetMeta(context.Background())
	if meta.ClockStep != 1 {
		t.Fatalf("clock did not advance: %+v", meta)
	}

	// A second run whose scripted replies are exhausted must fail without
	// moving the clock, and report the failure on the panel.
	postForm(t, ts, "/step", url.Values{}, "").Body.Close()
	pd = waitRunIdle(t, s)
	if pd.LastErr == "" {
		t.Fatal("second run should report an error")
	}
	meta, _ = s.DB.GetMeta(context.Background())
	if meta.ClockStep != 1 {
		t.Fatal("failed run moved the clock")
	}
	attempts, _ := s.DB.ListAttempts(context.Background(), 5)
	if len(attempts) != 2 || attempts[0].Status != "failed" {
		t.Fatalf("attempts: %+v", attempts)
	}
}

func TestRunPausesAtStepBoundary(t *testing.T) {
	s := newTestServer(t, true)
	s.Engine.Client = &slowClient{inner: &inference.Scripted{Replies: append(quietStep(), quietStep()...)}, delay: 150 * time.Millisecond}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	postForm(t, ts, "/run/start", url.Values{"steps": {"all"}}, "").Body.Close()
	waitAttempts(t, s, 1) // the first step is underway
	resp := postForm(t, ts, "/run/pause", url.Values{}, "")
	msg, isErr := flashOf(t, resp)
	if isErr || !strings.Contains(msg, "boundary") {
		t.Fatalf("pause flash = %q (err=%v)", msg, isErr)
	}

	pd := waitRunIdle(t, s)
	if !strings.Contains(pd.LastResult, "stopped at step boundary after 1 step") {
		t.Fatalf("run did not stop at boundary: %+v", pd)
	}
	meta, _ := s.DB.GetMeta(context.Background())
	if meta.ClockStep != 1 {
		t.Fatalf("run continued past boundary: clock %d", meta.ClockStep)
	}

	// Resume means starting a fresh run; the scripted replies allow it.
	postForm(t, ts, "/run/start", url.Values{"steps": {"1"}}, "").Body.Close()
	pd = waitRunIdle(t, s)
	if !strings.Contains(pd.LastResult, "step 2 committed") {
		t.Fatalf("resumed run: %+v", pd)
	}
}

func TestShutdownStopsAndJoinsRun(t *testing.T) {
	s := newTestServer(t, true)
	s.Engine.Client = &slowClient{inner: &inference.Scripted{Replies: quietStep()}, delay: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	s.runCtx = ctx
	if err := s.beginRun(-1); err != nil {
		t.Fatal(err)
	}
	waitAttempts(t, s, 1) // a step is underway
	cancel()
	done := make(chan struct{})
	go func() { s.runWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run goroutine still running after shutdown: the CLI could close the DB under it")
	}
}

func TestWorldPauseShowsInPanelAndResumes(t *testing.T) {
	s := newTestServer(t, true)
	pausing := append(quietStep()[:2], inference.Reply{Role: "orchestrator",
		Text: `{"notes":"impossible","events":[],"pause":{"reason":"hard directive cannot be met","directive_id":"dir-000001"}}`})
	s.Engine.Client = &inference.Scripted{Replies: pausing}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	postForm(t, ts, "/run/start", url.Values{"steps": {"5"}}, "").Body.Close()
	pd := waitRunIdle(t, s)
	if pd.WorldPause == "" || !strings.Contains(pd.WorldPause, "hard directive") {
		t.Fatalf("world pause not visible: %+v", pd)
	}
	body := get(t, ts, "/", http.StatusOK)
	if !strings.Contains(body, "resume world") || !strings.Contains(body, "world is paused") {
		t.Error("overview missing durable pause notice")
	}
	panel := get(t, ts, "/run/panel", http.StatusOK)
	if !strings.Contains(panel, "resume world") {
		t.Error("panel missing resume control")
	}

	// Starting a run while paused must be refused with guidance.
	resp := postForm(t, ts, "/run/start", url.Values{"steps": {"1"}}, "")
	msg, isErr := flashOf(t, resp)
	if !isErr || !strings.Contains(msg, "paused") {
		t.Fatalf("paused start flash = %q (err=%v)", msg, isErr)
	}

	postForm(t, ts, "/run/resume", url.Values{}, "").Body.Close()
	pd = s.panelData(context.Background())
	if pd.WorldPause != "" {
		t.Fatalf("still paused: %+v", pd)
	}
}

func TestWorldPauseVisibleEvenWithoutEngine(t *testing.T) {
	// Resume is a world-state fix and needs no inference: a paused world
	// must show its banner and resume control even with no engine set up.
	s := newTestServer(t, false)
	if err := s.DB.PauseWorld(context.Background(), "", "hard directive dir-000001 cannot be met"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	panel := get(t, ts, "/run/panel", http.StatusOK)
	if !strings.Contains(panel, "resume world") {
		t.Errorf("panel without engine lacks resume control: %s", panel)
	}
	postForm(t, ts, "/run/resume", url.Values{}, "").Body.Close()
	if _, paused, _ := s.DB.WorldPause(context.Background()); paused {
		t.Fatal("resume without engine failed")
	}
}

func TestCrossOriginMutationsRejected(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	for _, path := range []string{"/step", "/run/start", "/run/pause", "/run/resume", "/directive/add", "/directive/edit"} {
		resp := postForm(t, ts, path, url.Values{}, "http://evil.example")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: cross-origin status = %d, want 403", path, resp.StatusCode)
		}
	}
	// Same-origin and Origin-less requests are accepted.
	resp := postForm(t, ts, "/run/start", url.Values{"steps": {"1"}}, "http://"+ts.Listener.Addr().String())
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("same-origin start = %d", resp.StatusCode)
	}
	waitRunIdle(t, s)
	resp = postForm(t, ts, "/run/pause", url.Values{}, "")
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusOK {
		t.Errorf("origin-less pause = %d", resp.StatusCode)
	}
}

func TestExportEndpoint(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	postForm(t, ts, "/step", url.Values{}, "").Body.Close()
	waitRunIdle(t, s)

	resp, err := http.Get(ts.URL + "/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "herodotos-") {
		t.Errorf("content-disposition %q", cd)
	}
	var bundle store.ExportBundle
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Meta.ClockStep != 1 || len(bundle.Regions) != 2 || len(bundle.Events) != 0 {
		t.Fatalf("bundle: step %d, %d regions, %d events", bundle.Meta.ClockStep, len(bundle.Regions), len(bundle.Events))
	}
}

func TestDirectiveEditing(t *testing.T) {
	s := newTestServer(t, false)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp := postForm(t, ts, "/directive/add", url.Values{"text": {"The harbor fire must matter."}, "hard": {"1"}}, "")
	msg, isErr := flashOf(t, resp)
	if isErr || !strings.Contains(msg, "dir-000001") {
		t.Fatalf("add flash = %q (err=%v)", msg, isErr)
	}
	body := get(t, ts, "/", http.StatusOK)
	if !strings.Contains(body, "The harbor fire must matter.") {
		t.Fatal("added directive not shown")
	}

	resp = postForm(t, ts, "/directive/edit", url.Values{"id": {"dir-000001"}, "text": {"Revised: the harbor fire smoulders."}}, "")
	msg, isErr = flashOf(t, resp)
	if isErr {
		t.Fatalf("edit flash = %q", msg)
	}
	body = get(t, ts, "/", http.StatusOK)
	if !strings.Contains(body, "Revised: the harbor fire smoulders.") {
		t.Fatal("edited directive not shown")
	}

	resp = postForm(t, ts, "/directive/edit", url.Values{"id": {"dir-999999"}, "text": {"ghost"}}, "")
	if _, isErr = flashOf(t, resp); !isErr {
		t.Fatal("unknown directive edit should flash an error")
	}
	resp = postForm(t, ts, "/directive/add", url.Values{"text": {"  "}}, "")
	if _, isErr = flashOf(t, resp); !isErr {
		t.Fatal("empty directive should flash an error")
	}
}

func TestEventDetailPage(t *testing.T) {
	s := newTestServer(t, true)
	// Swap in a step that actually commits an event with a delivery.
	s.Engine.Client = &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: `{"prose":"p","events":[{"type":"development","title":"Riot in the capital","prose":"Crowds gather.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"delivery":{"prin-b":3}}]}`},
		{Role: "logographer:prin-b", Text: `{"prose":"q","events":[]}`},
		{Role: "orchestrator", Text: `{"notes":"ok","events":[{"origin":"king-a","type":"development","title":"Riot in the capital","prose":"Crowds gather.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"delivery":{"prin-b":3}}]}`},
	}}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	postForm(t, ts, "/step", url.Values{}, "").Body.Close()
	if pd := waitRunIdle(t, s); pd.LastErr != "" {
		t.Fatalf("step failed: %s", pd.LastErr)
	}
	body := get(t, ts, "/event?id=evt-000001", http.StatusOK)
	for _, want := range []string{"Riot in the capital", "opened a new thread", "unrest", "in transit", "prin-b"} {
		if !strings.Contains(body, want) {
			t.Errorf("event page missing %q", want)
		}
	}
	get(t, ts, "/event?id=evt-999999", http.StatusNotFound)
	get(t, ts, "/event", http.StatusOK) // no id: redirects to the timeline
}

func TestLiveAttemptView(t *testing.T) {
	s := newTestServer(t, true)
	s.Engine.Client = &slowClient{inner: &inference.Scripted{Replies: quietStep()}, delay: 300 * time.Millisecond}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	postForm(t, ts, "/step", url.Values{}, "").Body.Close()

	// The panel exposes the in-flight attempt with a live-watching link.
	deadline := time.Now().Add(5 * time.Second)
	var attemptID string
	for attemptID == "" && time.Now().Before(deadline) {
		if id, running, err := s.DB.RunningAttempt(context.Background()); err == nil && running {
			attemptID = id
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if attemptID == "" {
		t.Fatal("no running attempt appeared")
	}
	panel := get(t, ts, "/run/panel", http.StatusOK)
	if !strings.Contains(panel, "watch this step live") || !strings.Contains(panel, "/attempt?id="+attemptID) {
		t.Errorf("panel lacks live link: %s", panel)
	}

	// The attempt page announces itself live and polls itself for the
	// refreshable fragment only (outerHTML so polling stops on finish).
	body := get(t, ts, "/attempt?id="+attemptID, http.StatusOK)
	if !strings.Contains(body, "in progress") || !strings.Contains(body, `hx-trigger="every 4s"`) {
		t.Errorf("attempt page not live: %s", body)
	}
	if !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Error("live poll must swap outerHTML, not innerHTML")
	}
	frag := hxGet(t, ts, "/attempt?id="+attemptID)
	if strings.Contains(frag, "<header") || strings.Contains(frag, "<nav") || strings.Contains(frag, "all attempts") {
		t.Error("htmx poll returned a full page instead of a fragment: menu chrome would duplicate")
	}
	if !strings.Contains(frag, "attempt-status") {
		t.Errorf("poll fragment lacks live status: %s", frag)
	}

	// As model calls land they appear with their replies.
	deadline = time.Now().Add(5 * time.Second)
	for {
		calls, err := s.DB.ListModelCalls(context.Background(), attemptID)
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no model call appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	body = get(t, ts, "/attempt?id="+attemptID, http.StatusOK)
	if !strings.Contains(body, "logographer:king-a") || !strings.Contains(body, "reply") {
		t.Errorf("live calls/replies missing from page")
	}

	waitRunIdle(t, s)
	body = get(t, ts, "/attempt?id="+attemptID, http.StatusOK)
	if !strings.Contains(body, "committed") || strings.Contains(body, `hx-trigger="every 4s"`) {
		t.Error("finished attempt still polling or not marked committed")
	}
	// A late poll (browser still open) gets the static fragment and
	// carries no polling instructions: htmx stops.
	frag = hxGet(t, ts, "/attempt?id="+attemptID)
	if !strings.Contains(frag, "committed") || strings.Contains(frag, "hx-get") {
		t.Errorf("poll after finish must stop polling: %s", frag)
	}
	get(t, ts, "/attempt?id=att-999999", http.StatusNotFound)
}

func TestReferenceAdmin(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body := get(t, ts, "/references", http.StatusOK)
	if !strings.Contains(body, "Add a document") {
		t.Errorf("references page missing add form: %s", body)
	}

	// Add a region-scoped advisory reference.
	resp := postForm(t, ts, "/reference/add", url.Values{
		"scope": {"region"}, "region": {"king-a"},
		"title": {"Tide ritual"}, "description": {"how coastal folk read the tides"},
		"keywords": {"tide ritual coast"}, "body": {"The tide ritual binds salt to memory."},
	}, "")
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("add status %d", resp.StatusCode)
	}
	refs, err := s.DB.ListReferences(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Title != "Tide ritual" || !refs[0].Enabled {
		t.Fatalf("refs after add: %+v", refs)
	}
	// It is visible in the agent snapshot immediately.
	snap, err := s.DB.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.RegionRefs["king-a"]) != 1 {
		t.Errorf("new reference not in snapshot: %+v", snap.RegionRefs)
	}
	body = get(t, ts, "/references", http.StatusOK)
	if !strings.Contains(body, "Tide ritual") {
		t.Error("page does not list the new reference")
	}

	// Park it: same fields, but no enabled checkbox.
	postForm(t, ts, "/reference/update", url.Values{
		"id": {string(refs[0].ID)}, "scope": {"region"}, "region": {"king-a"},
		"title": {"Tide ritual"}, "keywords": {"tide ritual coast"},
		"body": {"The tide ritual binds salt to memory."},
	}, "").Body.Close()
	snap, err = s.DB.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.RegionRefs["king-a"]) != 0 {
		t.Errorf("parked reference still in snapshot")
	}
	refs, _ = s.DB.ListReferences(context.Background())
	if len(refs) != 1 || refs[0].Enabled {
		t.Errorf("parking not persisted: %+v", refs)
	}

	// Validation surfaces as a flash, not a broken world.
	postForm(t, ts, "/reference/add", url.Values{
		"scope": {"region"}, "title": {"no region"}, "body": {"x"},
	}, "").Body.Close()
	if refs, _ = s.DB.ListReferences(context.Background()); len(refs) != 1 {
		t.Errorf("invalid form created a reference: %+v", refs)
	}
	get(t, ts, "/references", http.StatusOK) // flash renders fine

	// Cross-origin mutation is refused.
	resp = postForm(t, ts, "/reference/add", url.Values{
		"title": {"evil"}, "body": {"x"},
	}, "http://evil.example")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin add status %d", resp.StatusCode)
	}

	// Delete.
	postForm(t, ts, "/reference/delete", url.Values{"id": {string(refs[0].ID)}}, "").Body.Close()
	if refs, _ = s.DB.ListReferences(context.Background()); len(refs) != 0 {
		t.Errorf("refs after delete: %+v", refs)
	}
}

func TestStepWithoutInference(t *testing.T) {
	s := newTestServer(t, false)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	body := get(t, ts, "/", http.StatusOK)
	if strings.Contains(body, "advance one step") {
		t.Error("step button shown without inference configured")
	}
	resp := postForm(t, ts, "/step", url.Values{}, "")
	if !strings.Contains(resp.Header.Get("Location"), "/attempts") {
		t.Fatalf("expected guidance redirect, got %q", resp.Header.Get("Location"))
	}
}

func TestCreativeInputRefusedDuringStep(t *testing.T) {
	s := newTestServer(t, true)
	s.Engine.Client = &slowClient{inner: &inference.Scripted{Replies: quietStep()}, delay: 400 * time.Millisecond}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	postForm(t, ts, "/step", url.Values{}, "").Body.Close()
	// Wait until the run is actually in flight, then try to change the
	// creative rules from under it.
	deadline := time.Now().Add(5 * time.Second)
	for !s.editingDuringRun() {
		if time.Now().After(deadline) {
			t.Fatal("run never became active")
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp := postForm(t, ts, "/directive/add", url.Values{"text": {"Sneaked in mid-step."}, "hard": {"1"}}, "")
	msg, isErr := flashOf(t, resp)
	if !isErr || !strings.Contains(msg, "mid-step") {
		t.Fatalf("mid-run directive add must be refused: %q (err=%v)", msg, isErr)
	}
	resp = postForm(t, ts, "/reference/add", url.Values{
		"id": {"ref-sneak"}, "scope": {"world"}, "title": {"T"}, "body": {"B"}, "enabled": {"1"},
	}, "")
	if _, isErr = flashOf(t, resp); !isErr {
		t.Fatal("mid-run reference add must be refused")
	}
	resp = postForm(t, ts, "/reference/delete", url.Values{"id": {"ref-world-rules"}}, "")
	if _, isErr = flashOf(t, resp); !isErr {
		t.Fatal("mid-run reference delete must be refused")
	}

	// Nothing changed under the running step, and it committed cleanly.
	waitRunIdle(t, s)
	refs, _ := s.DB.ListReferences(context.Background())
	for _, ref := range refs {
		if ref.ID == "ref-sneak" {
			t.Fatal("reference created during run")
		}
	}
	dirs, _ := s.DB.ListDirectives(context.Background())
	for _, dd := range dirs {
		if dd.Text == "Sneaked in mid-step." {
			t.Fatal("directive created during run")
		}
	}
	meta, _ := s.DB.GetMeta(context.Background())
	if meta.ClockStep != 1 {
		t.Fatalf("step did not commit: clock %d", meta.ClockStep)
	}
}

func TestEntityPageShowsCanonicalAndBeliefs(t *testing.T) {
	s := newTestServer(t, true)
	s.Engine.Client = &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: `{"prose":"Unrest spreads.","events":[{"type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"]}],"beliefs":[{"subject_id":"q-elinor","modality":"uncertain","statement":"the queen plans to abdicate"}],"memory":{"summary":"Rioting began."}}`},
		{Role: "logographer:prin-b", Text: quietPrin},
		{Role: "orchestrator", Text: `{"notes":"accepted","events":[{"origin":"king-a","type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"]}]}`},
	}}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	if _, err := s.Engine.RunStep(context.Background()); err != nil {
		t.Fatal(err)
	}

	body := get(t, ts, "/entity?id=q-elinor", http.StatusOK)
	for _, want := range []string{
		"Queen Elinor",                // name
		"heir: yes",                   // canonical attribute
		"the queen plans to abdicate", // belief about the entity
		`href="/region?id=king-a"`,    // path to its region
		"Rioting in the capital",      // event it appears in
	} {
		if !strings.Contains(body, want) {
			t.Errorf("entity page missing %q", want)
		}
	}

	body = get(t, ts, "/event?id=evt-000001", http.StatusOK)
	if !strings.Contains(body, `href="/entity?id=q-elinor"`) {
		t.Error("event page should link entity ids to the entity page")
	}

	get(t, ts, "/entity?id=nope-404", http.StatusNotFound)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/entity?id=bad%20id", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("invalid id: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRegionPageInspectionNoCrossLeak(t *testing.T) {
	s := newTestServer(t, true)
	s.Engine.Client = &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingBusy},
		{Role: "logographer:prin-b", Text: prinOpinion},
		{Role: "orchestrator", Text: orchAcceptWeb},
	}}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	if _, err := s.Engine.RunStep(context.Background()); err != nil {
		t.Fatal(err)
	}

	king := get(t, ts, "/region?id=king-a", http.StatusOK)
	for _, want := range []string{
		"Kingdom A", "unrest: high", // canonical conditions (fixture + commit)
		"Rioting began.",             // the region's own memory
		"Queen Elinor",               // entities located here
		`href="/entity?id=q-elinor"`, // link out
		"prin-b",                     // neighbour identity
	} {
		if !strings.Contains(king, want) {
			t.Errorf("king-a page missing %q", want)
		}
	}

	prin := get(t, ts, "/region?id=prin-b", http.StatusOK)
	if !strings.Contains(prin, "Kingdom A starves.") ||
		!strings.Contains(prin, `modality-mistaken`) {
		t.Errorf("prin-b page missing its own belief: %s", prin)
	}
	// prin-b's page must not expose king-a's canonical conditions.
	if strings.Contains(prin, "unrest:") {
		t.Error("region page leaked another region's canonical attributes")
	}
	// Memory is per region: prin-b's page must not carry king-a's memory.
	if strings.Contains(prin, "Rioting began.") {
		t.Error("region page leaked another region's agent memory")
	}

	get(t, ts, "/region?id=nope-404", http.StatusNotFound)
}

const kingBusy = `{"prose":"Unrest spreads.","events":[{"type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"]}],"memory":{"summary":"Rioting began."}}`

const prinOpinion = `{"prose":"We watch.","events":[],"beliefs":[{"modality":"mistaken","statement":"Kingdom A starves."}],"memory":{"summary":"Watching."}}`

const orchAcceptWeb = `{"notes":"accepted","events":[{"origin":"king-a","type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"]}]}`

// A failed attempt explains itself after the fact: stage, underlying
// cause, and the effective settings of each model call.
func TestFailedAttemptIsSelfContained(t *testing.T) {
	s := newTestServer(t, true)
	s.Engine.Client = &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: quietKing},
		{Role: "logographer:prin-b", Text: quietPrin},
		{Role: "orchestrator", Text: `{"notes":"n","events":[{"origin":"orchestrator","type":"x","title":"Nobody sees","prose":"p"}]}`},
		{Role: "orchestrator", Text: `{"notes":"n","events":[{"origin":"orchestrator","type":"x","title":"Nobody sees","prose":"p"}]}`},
		{Role: "orchestrator", Text: `{"notes":"n","events":[{"origin":"orchestrator","type":"x","title":"Nobody sees","prose":"p"}]}`},
	}}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	if _, err := s.Engine.RunStep(context.Background()); err == nil {
		t.Fatal("step should fail")
	}
	body := get(t, ts, "/attempt?id=att-000001", http.StatusOK)
	for _, want := range []string{"orchestrator failed", "needs &#34;observers&#34;", "max_out=", "t=0.3"} {
		if !strings.Contains(body, want) {
			t.Errorf("attempt page lacks %q", want)
		}
	}
}

// The idle panel offers a bounded run, and once steps commit after the
// page loaded it says the surrounding history is stale.
func TestPanelBoundedRunAndStaleNotice(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	if body := get(t, ts, "/", http.StatusOK); !strings.Contains(body, `name="steps" value="3"`) {
		t.Fatal("bounded run field missing")
	}
	if got := stepsFormValue("100000"); got != maxRunSteps {
		t.Fatalf("bounded run not capped: %d", got)
	}
	if _, err := s.Engine.RunStep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if frag := hxGet(t, ts, "/run/panel?seen=0"); !strings.Contains(frag, "1 new step(s) committed") {
		t.Fatalf("stale notice missing:\n%s", frag)
	}
	if frag := hxGet(t, ts, "/run/panel?seen=1"); strings.Contains(frag, "new step(s) committed") {
		t.Fatal("stale notice shown for a current page")
	}
}
