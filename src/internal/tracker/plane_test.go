package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlane is an in-memory Plane API: one workspace, one project.
type fakePlane struct {
	t  *testing.T
	mu sync.Mutex

	states   []map[string]any
	labels   []map[string]any
	issues   []map[string]any
	comments map[string][]map[string]any
	links    map[string][]string
	modules  map[string][]string
	// blockedBy maps an issue to the issues blocking it.
	blockedBy map[string][]string
	seq       int
	ids       int
	// throttle answers the next n requests with 429.
	throttle int
	requests int
}

const fakeProject = "proj-1"

func newFakePlane(t *testing.T) (*fakePlane, *Plane) {
	f := &fakePlane{
		t: t, comments: map[string][]map[string]any{}, links: map[string][]string{}, modules: map[string][]string{}, blockedBy: map[string][]string{},
		states: []map[string]any{
			{"id": "st-backlog", "name": "Backlog", "group": "backlog"},
			{"id": "st-todo", "name": "Todo", "group": "unstarted"},
			{"id": "st-progress", "name": "In Progress", "group": "started"},
			{"id": "st-done", "name": "Done", "group": "completed"},
			{"id": "st-cancelled", "name": "Cancelled", "group": "cancelled"},
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := NewPlane(PlaneConfig{BaseURL: srv.URL, AppURL: "https://app.example", APIKey: "pat", Workspace: "acme", Project: "float"})
	p.sleep = func(time.Duration) {}
	return f, p
}

func (f *fakePlane) id(prefix string) string {
	f.ids++
	return fmt.Sprintf("%s-%d", prefix, f.ids)
}

func (f *fakePlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("X-API-Key") != "pat" {
		http.Error(w, `{"detail":"bad key"}`, http.StatusUnauthorized)
		return
	}
	if f.throttle > 0 {
		f.throttle--
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	var body map[string]any
	if r.Method != http.MethodGet {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/acme/")
	proj := "projects/" + fakeProject + "/"
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, proj), "/"), "/")

	switch {
	case path == "projects/":
		f.page(w, r, []map[string]any{{"id": "proj-0", "identifier": "OTHER"}, {"id": fakeProject, "identifier": "FLOAT"}})
	case !strings.HasPrefix(path, proj):
		http.NotFound(w, r)
	case parts[0] == "states" && r.Method == http.MethodGet:
		f.page(w, r, f.states)
	case parts[0] == "states":
		body["id"] = f.id("st")
		f.states = append(f.states, body)
		f.json(w, body)
	case parts[0] == "labels" && r.Method == http.MethodGet:
		f.page(w, r, f.labels)
	case parts[0] == "labels":
		body["id"] = f.id("lb")
		f.labels = append(f.labels, body)
		f.json(w, body)
	case parts[0] == "issues" && len(parts) == 1 && r.Method == http.MethodGet:
		f.page(w, r, f.issues)
	case parts[0] == "issues" && len(parts) == 1:
		f.seq++
		body["id"], body["sequence_id"] = f.id("is"), f.seq
		body["description_stripped"] = body["description_html"]
		f.issues = append(f.issues, body)
		f.json(w, body)
	case parts[0] == "issues" && len(parts) == 2 && r.Method == http.MethodPatch:
		it := f.issue(parts[1])
		for k, v := range body {
			it[k] = v
		}
		f.json(w, it)
	case parts[0] == "issues" && len(parts) == 3 && parts[2] == "comments" && r.Method == http.MethodGet:
		f.page(w, r, f.comments[parts[1]])
	case parts[0] == "issues" && len(parts) == 3 && parts[2] == "comments":
		f.addComment(parts[1], body["comment_html"].(string), time.Now())
		f.json(w, body)
	case parts[0] == "issues" && len(parts) == 3 && parts[2] == "links":
		f.links[parts[1]] = append(f.links[parts[1]], body["url"].(string))
		f.json(w, body)
	case parts[0] == "work-items" && len(parts) == 3 && parts[2] == "relations" && r.Method == http.MethodGet:
		f.json(w, map[string]any{"blocked_by": append([]string{}, f.blockedBy[parts[1]]...), "blocking": []string{}})
	case parts[0] == "work-items" && len(parts) == 3 && parts[2] == "relations":
		if body["relation_type"] != "blocked_by" {
			f.t.Errorf("relation_type %v", body["relation_type"])
		}
		for _, id := range body["issues"].([]any) {
			f.blockedBy[parts[1]] = append(f.blockedBy[parts[1]], id.(string))
		}
		f.json(w, body)
	case parts[0] == "modules" && len(parts) == 1:
		id := f.id("mod")
		f.modules[id] = nil
		f.json(w, map[string]any{"id": id, "name": body["name"]})
	case parts[0] == "modules" && len(parts) == 3:
		for _, id := range body["issues"].([]any) {
			f.modules[parts[1]] = append(f.modules[parts[1]], id.(string))
		}
		f.json(w, body)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *fakePlane) addComment(item, htmlText string, at time.Time) {
	stripped := strings.NewReplacer("<p>", "", "</p>", "", "&#39;", "'", "&#34;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(htmlText)
	f.comments[item] = append(f.comments[item], map[string]any{"comment_stripped": stripped, "created_at": at.Format(time.RFC3339Nano)})
}

// page serves Plane's cursor pagination: cursor "<per_page>:<page>:0".
func (f *fakePlane) page(w http.ResponseWriter, r *http.Request, all []map[string]any) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per == 0 || per > 2 {
		per = 2 // small pages so tests cross page boundaries
	}
	page := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		page, _ = strconv.Atoi(strings.Split(c, ":")[1])
	}
	lo, hi := min(page*per, len(all)), min((page+1)*per, len(all))
	f.json(w, map[string]any{
		"results": all[lo:hi], "next_page_results": hi < len(all),
		"next_cursor": fmt.Sprintf("%d:%d:0", per, page+1),
	})
}

func (f *fakePlane) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePlane) issue(id string) map[string]any {
	for _, it := range f.issues {
		if it["id"] == id {
			return it
		}
	}
	f.t.Fatalf("no issue %s", id)
	return nil
}

// label adds a label to an issue by name, as a person would.
func (f *fakePlane) label(issueID, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.labels {
		if l["name"] == name {
			it := f.issue(issueID)
			labels, _ := it["labels"].([]any)
			it["labels"] = append(labels, l["id"])
			return
		}
	}
	f.t.Fatalf("no label %q", name)
}

func (f *fakePlane) stateName(issueID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	sid := f.issue(issueID)["state"]
	for _, s := range f.states {
		if s["id"] == sid {
			return s["name"].(string)
		}
	}
	return ""
}

// move sets an issue's state by name, as a person on the board would.
func (f *fakePlane) move(issueID, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.states {
		if s["name"] == state {
			f.issue(issueID)["state"] = s["id"]
			return
		}
	}
	f.t.Fatalf("no state %q", state)
}

func TestPlaneSetupCreatesAgenticStatesOnce(t *testing.T) {
	f, p := newFakePlane(t)
	ctx := context.Background()
	if err := p.EnsureSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureSetup(ctx); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range f.states {
		names = append(names, s["name"].(string)+"/"+s["group"].(string))
	}
	want := "Backlog/backlog Todo/unstarted In Progress/started Done/completed Cancelled/cancelled " +
		"Ready for agent/unstarted Agent working/started Needs you/started In review/started"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("states:\n got %s\nwant %s", got, want)
	}
	for _, st := range f.states[5:] {
		if st["sequence"] == nil {
			t.Fatalf("state %v created without a board position", st["name"])
		}
	}
	if len(f.labels) != 2 || f.labels[0]["name"] != Label || f.labels[1]["name"] != PlanLabel {
		t.Fatalf("labels: %v", f.labels)
	}
}

func TestPlaneItemsPaginatesAndMapsStates(t *testing.T) {
	f, p := newFakePlane(t)
	ctx := context.Background()
	if err := p.EnsureSetup(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := p.Create(ctx, NewItem{Title: fmt.Sprintf("story %d", i), State: StateBacklog, ExternalID: fmt.Sprintf("x/%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	f.issues[4]["state"] = "st-todo" // a state floatilla doesn't use
	f.throttle = 1

	items, err := p.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 {
		t.Fatalf("got %d items, want 5", len(items))
	}
	if it := items[2]; it.Key != "FLOAT-3" || it.State != StateBacklog || it.URL != "https://app.example/acme/browse/FLOAT-3/" || it.ExternalID != "x/2" {
		t.Fatalf("item: %+v", it)
	}
	if items[4].State != "" {
		t.Fatalf("Todo should map to no state, got %q", items[4].State)
	}
}

func TestPlaneCreateSendsStoryFields(t *testing.T) {
	f, p := newFakePlane(t)
	ctx := context.Background()
	if err := p.EnsureSetup(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := p.Create(ctx, NewItem{
		Title: "Add retry", Description: "Retry FX fetches.\n\nUse <jitter>.", AcceptanceCriteria: []string{"3 retries", "backoff"},
		Priority: "high", State: StateReady, ExternalID: "plan/p1/s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	it := f.issues[0]
	if it["description_html"] != "<p>Retry FX fetches.</p><p>Use &lt;jitter&gt;.</p><h3>Acceptance criteria</h3><ul><li>3 retries</li><li>backoff</li></ul>" {
		t.Fatalf("html: %v", it["description_html"])
	}
	if it["priority"] != "high" || it["external_source"] != Source || it["external_id"] != "plan/p1/s1" || f.stateName(it["id"].(string)) != "Ready for agent" {
		t.Fatalf("issue: %v", it)
	}
}

func TestPlaneCommentsSkipFloatillasOwn(t *testing.T) {
	f, p := newFakePlane(t)
	ctx := context.Background()
	if err := p.EnsureSetup(ctx); err != nil {
		t.Fatal(err)
	}
	it, _ := p.Create(ctx, NewItem{Title: "x", State: StateBacklog})
	since := time.Now()
	f.addComment(it.ID, "<p>too early</p>", since.Add(-time.Minute))
	if err := p.Comment(ctx, it.ID, "agent working"); err != nil {
		t.Fatal(err)
	}
	f.addComment(it.ID, "<p>rename the flag</p>", since.Add(time.Minute))
	got, err := p.Comments(ctx, it.ID, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "rename the flag" {
		t.Fatalf("comments: %q", got)
	}
}

func TestPlaneUnknownProject(t *testing.T) {
	_, p := newFakePlane(t)
	p.cfg.Project = "NOPE"
	if err := p.EnsureSetup(context.Background()); err == nil || !strings.Contains(err.Error(), "no project") {
		t.Fatalf("err = %v", err)
	}
}

func TestHTMLText(t *testing.T) {
	got := htmlText(`<p>Rates go stale.<br>repo: https://git.example/fx.git</p><h3>Notes</h3><ul><li>a &amp; b</li><li>c</li></ul><p></p><p></p>`, "")
	want := "Rates go stale.\nrepo: https://git.example/fx.git\nNotes\n- a & b\n- c"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if htmlText("", "plain") != "plain" {
		t.Fatal("fallback")
	}
}
