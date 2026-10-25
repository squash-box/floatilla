package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PlaneConfig points at one Plane project.
type PlaneConfig struct {
	// BaseURL of the API, e.g. https://api.plane.so.
	BaseURL string
	// AppURL is the web app, for links; derived from BaseURL when empty.
	AppURL    string
	APIKey    string
	Workspace string
	// Project is the project identifier (e.g. FLOAT) or id.
	Project string
	// StateNames overrides the Plane state name for a State.
	StateNames map[State]string
}

// Plane implements Tracker on the Plane REST API (api/v1).
type Plane struct {
	cfg  PlaneConfig
	http *http.Client
	// sleep waits out rate limits; replaced in tests.
	sleep func(time.Duration)

	mu         sync.Mutex
	projectID  string
	identifier string
	stateID    map[State]string
	stateOf    map[string]State
	labelID    string
	labelName  map[string]string
}

func NewPlane(cfg PlaneConfig) *Plane {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.AppURL == "" {
		cfg.AppURL = strings.Replace(cfg.BaseURL, "://api.", "://app.", 1)
	}
	cfg.AppURL = strings.TrimRight(cfg.AppURL, "/")
	return &Plane{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, sleep: time.Sleep}
}

// stateSpecs are the states floatilla needs: Plane's defaults plus the
// agentic ones, which EnsureSetup adds when missing. sequence orders the
// board's columns; Plane's defaults sit at 15000 (Backlog), 30000 (Todo),
// 45000 (In Progress), 60000 (Done) and 75000 (Cancelled), and new states
// otherwise land after Cancelled.
var stateSpecs = []struct {
	state    State
	name     string
	group    string
	color    string
	sequence int
}{
	{StateBacklog, "Backlog", "backlog", "#A3A3A3", 15000},
	{StateReady, "Ready for agent", "unstarted", "#3B82F6", 35000},
	{StateWorking, "Agent working", "started", "#F59E0B", 47000},
	{StateNeedsYou, "Needs you", "started", "#EC4899", 50000},
	{StateInReview, "In review", "started", "#8B5CF6", 55000},
	{StateDone, "Done", "completed", "#16A34A", 60000},
	{StateCancelled, "Cancelled", "cancelled", "#EF4444", 75000},
}

func (p *Plane) stateName(s State, def string) string {
	if n := p.cfg.StateNames[s]; n != "" {
		return n
	}
	return def
}

func (p *Plane) Name() string { return "plane" }

func (p *Plane) EnsureSetup(ctx context.Context) error {
	var projects []struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
	}
	if err := p.list(ctx, p.ws("projects/"), &projects); err != nil {
		return fmt.Errorf("plane projects: %w", err)
	}
	var projectID, identifier string
	for _, pr := range projects {
		if strings.EqualFold(pr.Identifier, p.cfg.Project) || pr.ID == p.cfg.Project {
			projectID, identifier = pr.ID, pr.Identifier
		}
	}
	if projectID == "" {
		return fmt.Errorf("plane: no project %q in workspace %s", p.cfg.Project, p.cfg.Workspace)
	}
	p.mu.Lock()
	p.projectID, p.identifier = projectID, identifier
	p.mu.Unlock()

	var states []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := p.list(ctx, p.proj("states/"), &states); err != nil {
		return fmt.Errorf("plane states: %w", err)
	}
	byName := map[string]string{}
	for _, s := range states {
		byName[strings.ToLower(s.Name)] = s.ID
	}
	stateID, stateOf := map[State]string{}, map[string]State{}
	for _, spec := range stateSpecs {
		name := p.stateName(spec.state, spec.name)
		id, ok := byName[strings.ToLower(name)]
		if !ok {
			var created struct {
				ID string `json:"id"`
			}
			body := map[string]any{"name": name, "group": spec.group, "color": spec.color, "sequence": spec.sequence}
			if err := p.do(ctx, http.MethodPost, p.proj("states/"), body, &created); err != nil {
				return fmt.Errorf("plane: create state %q: %w", name, err)
			}
			id = created.ID
		}
		stateID[spec.state], stateOf[id] = id, spec.state
	}

	p.mu.Lock()
	p.stateID, p.stateOf = stateID, stateOf
	p.mu.Unlock()

	names, err := p.labels(ctx)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for id, name := range names {
		ids[strings.ToLower(name)] = id
	}
	for _, l := range []struct{ name, color string }{{Label, "#0EA5E9"}, {PlanLabel, "#8B5CF6"}} {
		if _, ok := ids[l.name]; ok {
			continue
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := p.do(ctx, http.MethodPost, p.proj("labels/"), map[string]string{"name": l.name, "color": l.color}, &created); err != nil {
			return fmt.Errorf("plane: create label %q: %w", l.name, err)
		}
		ids[l.name] = created.ID
		names[created.ID] = l.name
	}
	p.mu.Lock()
	p.labelID, p.labelName = ids[Label], names
	p.mu.Unlock()
	return nil
}

// labels fetches the project's labels by id.
func (p *Plane) labels(ctx context.Context) (map[string]string, error) {
	var raw []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := p.list(ctx, p.proj("labels/"), &raw); err != nil {
		return nil, fmt.Errorf("plane labels: %w", err)
	}
	out := make(map[string]string, len(raw))
	for _, l := range raw {
		out[l.ID] = l.Name
	}
	return out, nil
}

type planeItem struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	SequenceID          int      `json:"sequence_id"`
	State               string   `json:"state"`
	DescriptionHTML     string   `json:"description_html"`
	DescriptionStripped string   `json:"description_stripped"`
	Labels              []string `json:"labels"`
	Parent              string   `json:"parent"`
	ExternalSource      string   `json:"external_source"`
	ExternalID          string   `json:"external_id"`
}

func (p *Plane) item(it planeItem) Item {
	p.mu.Lock()
	state, identifier := p.stateOf[it.State], p.identifier
	var labels []string
	for _, id := range it.Labels {
		if name, ok := p.labelName[id]; ok {
			labels = append(labels, name)
		}
	}
	p.mu.Unlock()
	key := identifier + "-" + strconv.Itoa(it.SequenceID)
	return Item{
		ID: it.ID, Key: key, Title: it.Name, Description: htmlText(it.DescriptionHTML, it.DescriptionStripped), State: state,
		Labels: labels, ParentID: it.Parent,
		URL:            fmt.Sprintf("%s/%s/browse/%s/", p.cfg.AppURL, p.cfg.Workspace, key),
		ExternalSource: it.ExternalSource, ExternalID: it.ExternalID,
	}
}

func (p *Plane) Items(ctx context.Context) ([]Item, error) {
	// Labels people added since the last pass.
	names, err := p.labels(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.labelName = names
	p.mu.Unlock()
	var raw []planeItem
	if err := p.list(ctx, p.proj("issues/"), &raw); err != nil {
		return nil, err
	}
	out := make([]Item, len(raw))
	for i, it := range raw {
		out[i] = p.item(it)
	}
	return out, nil
}

func (p *Plane) Create(ctx context.Context, in NewItem) (Item, error) {
	p.mu.Lock()
	body := map[string]any{
		"name":             in.Title,
		"description_html": storyHTML(in.Description, in.AcceptanceCriteria),
		"state":            p.stateID[in.State],
		"priority":         planePriority(in.Priority),
		"labels":           []string{p.labelID},
		"external_source":  Source,
		"external_id":      in.ExternalID,
	}
	p.mu.Unlock()
	if in.ParentID != "" {
		body["parent"] = in.ParentID
	}
	var created planeItem
	if err := p.do(ctx, http.MethodPost, p.proj("issues/"), body, &created); err != nil {
		return Item{}, err
	}
	return p.item(created), nil
}

func (p *Plane) SetState(ctx context.Context, itemID string, s State) error {
	p.mu.Lock()
	id := p.stateID[s]
	p.mu.Unlock()
	if id == "" {
		return fmt.Errorf("plane: no state for %s", s)
	}
	return p.do(ctx, http.MethodPatch, p.proj("issues/"+itemID+"/"), map[string]string{"state": id}, nil)
}

func (p *Plane) Comment(ctx context.Context, itemID, text string, blocks ...Block) error {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>%s</p>", html.EscapeString(commentPrefix+text))
	for _, bl := range blocks {
		if bl.Heading != "" {
			fmt.Fprintf(&b, "<h3>%s</h3>", html.EscapeString(bl.Heading))
		}
		if bl.Pre != "" {
			fmt.Fprintf(&b, "<pre><code>%s</code></pre>", html.EscapeString(bl.Pre))
		}
		if len(bl.List) > 0 {
			b.WriteString("<ul>")
			for _, li := range bl.List {
				fmt.Fprintf(&b, "<li>%s</li>", html.EscapeString(li))
			}
			b.WriteString("</ul>")
		}
	}
	body := map[string]string{"comment_html": b.String()}
	return p.do(ctx, http.MethodPost, p.proj("issues/"+itemID+"/comments/"), body, nil)
}

func (p *Plane) Comments(ctx context.Context, itemID string, since time.Time) ([]string, error) {
	var raw []struct {
		Stripped  string    `json:"comment_stripped"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := p.list(ctx, p.proj("issues/"+itemID+"/comments/"), &raw); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range raw {
		text := strings.TrimSpace(c.Stripped)
		if c.CreatedAt.After(since) && text != "" && !strings.HasPrefix(text, commentPrefix) {
			out = append(out, text)
		}
	}
	return out, nil
}

func (p *Plane) Link(ctx context.Context, itemID, link, title string) error {
	return p.do(ctx, http.MethodPost, p.proj("issues/"+itemID+"/links/"), map[string]string{"url": link, "title": title}, nil)
}

func (p *Plane) BlockedBy(ctx context.Context, itemID string) ([]string, error) {
	var rel struct {
		BlockedBy []string `json:"blocked_by"`
	}
	if err := p.do(ctx, http.MethodGet, p.proj("work-items/"+itemID+"/relations/"), nil, &rel); err != nil {
		return nil, err
	}
	return rel.BlockedBy, nil
}

func (p *Plane) Block(ctx context.Context, itemID string, blockerIDs []string) error {
	body := map[string]any{"relation_type": "blocked_by", "issues": blockerIDs}
	return p.do(ctx, http.MethodPost, p.proj("work-items/"+itemID+"/relations/"), body, nil)
}

func (p *Plane) Group(ctx context.Context, name, description string, itemIDs []string) error {
	var mod struct {
		ID string `json:"id"`
	}
	if err := p.do(ctx, http.MethodPost, p.proj("modules/"), map[string]string{"name": name, "description": description}, &mod); err != nil {
		return err
	}
	return p.do(ctx, http.MethodPost, p.proj("modules/"+mod.ID+"/module-issues/"), map[string][]string{"issues": itemIDs}, nil)
}

func (p *Plane) ws(path string) string {
	return fmt.Sprintf("%s/api/v1/workspaces/%s/%s", p.cfg.BaseURL, url.PathEscape(p.cfg.Workspace), path)
}

func (p *Plane) proj(path string) string {
	p.mu.Lock()
	id := p.projectID
	p.mu.Unlock()
	return p.ws("projects/" + id + "/" + path)
}

// list follows Plane's cursor pagination into out (a pointer to a slice).
func (p *Plane) list(ctx context.Context, endpoint string, out any) error {
	var all []json.RawMessage
	cursor := ""
	for {
		u, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		q := u.Query()
		q.Set("per_page", "100")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		u.RawQuery = q.Encode()
		var page struct {
			Results         []json.RawMessage `json:"results"`
			NextCursor      string            `json:"next_cursor"`
			NextPageResults bool              `json:"next_page_results"`
		}
		if err := p.do(ctx, http.MethodGet, u.String(), nil, &page); err != nil {
			return err
		}
		all = append(all, page.Results...)
		if !page.NextPageResults || page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	data, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// do sends one request, waiting out 429s (Plane allows 60 requests/minute).
func (p *Plane) do(ctx context.Context, method, endpoint string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("X-API-Key", p.cfg.APIKey)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := p.http.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			wait := 5 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			p.sleep(wait)
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("%s %s: %s: %s", method, strings.TrimPrefix(endpoint, p.cfg.BaseURL), resp.Status, truncate(string(data), 300))
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		return json.Unmarshal(data, out)
	}
}

func planePriority(p string) string {
	switch p {
	case "urgent", "high", "medium", "low":
		return p
	}
	return "none"
}

func storyHTML(description string, criteria []string) string {
	var b strings.Builder
	for para := range strings.SplitSeq(strings.TrimSpace(description), "\n\n") {
		if para = strings.TrimSpace(para); para != "" {
			fmt.Fprintf(&b, "<p>%s</p>", strings.ReplaceAll(html.EscapeString(para), "\n", "<br>"))
		}
	}
	if len(criteria) > 0 {
		b.WriteString("<h3>Acceptance criteria</h3><ul>")
		for _, c := range criteria {
			fmt.Fprintf(&b, "<li>%s</li>", html.EscapeString(c))
		}
		b.WriteString("</ul>")
	}
	return b.String()
}

var (
	blockEnd = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|h[1-6]|pre|blockquote|tr)>`)
	listItem = regexp.MustCompile(`(?i)<li[^>]*>`)
	anyTag   = regexp.MustCompile(`<[^>]*>`)
	blankRun = regexp.MustCompile(`\n{3,}`)
)

// htmlText turns Plane's description HTML into plain text with line breaks
// (description_stripped runs paragraphs together).
func htmlText(h, fallback string) string {
	if strings.TrimSpace(h) == "" {
		return fallback
	}
	t := blockEnd.ReplaceAllString(h, "\n")
	t = listItem.ReplaceAllString(t, "- ")
	t = html.UnescapeString(anyTag.ReplaceAllString(t, ""))
	return strings.TrimSpace(blankRun.ReplaceAllString(t, "\n\n"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
