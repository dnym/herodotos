package web

import (
	"context"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/inference"
	"github.com/dnym/herodotos/internal/sim"
	"github.com/dnym/herodotos/internal/store"
)

func postNoRedirect(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Post(ts.URL+path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
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
	if err := db.InitWorld(ctx, w); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db}
	if engine {
		fake := &inference.Scripted{Replies: []inference.Reply{
			{Role: "logographer:king-a", Text: `{"prose":"q","events":[]}`},
			{Role: "logographer:prin-b", Text: `{"prose":"q","events":[]}`},
			{Role: "orchestrator", Text: `{"notes":"quiet step","events":[]}`},
		}}
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

// getAbs follows a redirect target that may be relative or absolute.
func getAbs(t *testing.T, ts *httptest.Server, loc string, want int) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("bad location %q: %v", loc, err)
	}
	base, _ := url.Parse(ts.URL)
	target := base.ResolveReference(u).String()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", loc, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s: status %d body %s", loc, resp.StatusCode, b)
	}
	return string(b)
}

func TestPagesRender(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body := get(t, ts, "/", http.StatusOK)
	for _, want := range []string{"Web World", "Kingdom A", "advance one step", "unrest"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	get(t, ts, "/events", http.StatusOK)
	get(t, ts, "/events?region=king-a", http.StatusOK)
	get(t, ts, "/attempts", http.StatusOK)
	get(t, ts, "/attempt?id=att-000001", http.StatusOK)
	get(t, ts, "/nope", http.StatusNotFound)
	get(t, ts, "/static/style.css", http.StatusOK)
	get(t, ts, "/static/htmx.min.js", http.StatusOK)
	get(t, ts, "/event?id=evt-000001", http.StatusNotFound) // nothing committed yet
}

func TestHXStepReturnsRedirect(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/step", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp, err := (&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("HX step status = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("HX-Redirect")
	if !strings.Contains(loc, "flash=step+1+committed") {
		t.Fatalf("HX-Redirect: %q", loc)
	}
	// htmx clients get a full refresh; the flash must carry the result.
	body := getAbs(t, ts, loc, http.StatusOK)
	if !strings.Contains(body, "step 1 committed") {
		t.Errorf("refreshed overview missing flash")
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

	resp := postNoRedirect(t, ts, "/step")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("step status = %d", resp.StatusCode)
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

func TestStepActionCommitsAndFailsCleanly(t *testing.T) {
	s := newTestServer(t, true)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	// POST /step runs one attempt; success redirects with a flash notice.
	resp := postNoRedirect(t, ts, "/step")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("step status = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "flash=") {
		t.Fatalf("no flash on %s", loc)
	}
	body := getAbs(t, ts, loc, http.StatusOK)
	if !strings.Contains(body, "committed") {
		t.Errorf("flash: %s", body[:200])
	}
	meta, _ := s.DB.GetMeta(context.Background())
	if meta.ClockStep != 1 {
		t.Fatalf("clock did not advance: %+v", meta)
	}

	// The scripted replies are exhausted: the next attempt must fail
	// without moving the clock, and must land on /attempts with an error.
	resp = postNoRedirect(t, ts, "/step")
	resp.Body.Close()
	loc = resp.Header.Get("Location")
	if !strings.Contains(loc, "/attempts") {
		t.Fatalf("failed step should redirect to attempts: %s", loc)
	}
	meta, _ = s.DB.GetMeta(context.Background())
	if meta.ClockStep != 1 {
		t.Fatal("failed step moved the clock")
	}
	attempts, _ := s.DB.ListAttempts(context.Background(), 5)
	if len(attempts) != 2 || attempts[0].Status != "failed" {
		t.Fatalf("attempts: %+v", attempts)
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
	resp := postNoRedirect(t, ts, "/step")
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "/attempts") {
		t.Fatalf("expected guidance redirect, got %q", resp.Header.Get("Location"))
	}
}
