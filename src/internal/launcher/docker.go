package launcher

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Docker is a Runtime that talks to the Docker Engine API over its unix
// socket. Only the handful of endpoints a launcher needs are used.
type Docker struct {
	hc *http.Client
}

func NewDocker(socket string) *Docker {
	return &Docker{hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

type apiError struct {
	Status  int
	Message string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("docker: %d %s", e.Status, e.Message) }

func (d *Docker) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		e := &apiError{Status: resp.StatusCode}
		_ = json.NewDecoder(resp.Body).Decode(e)
		return e
	}
	switch out := out.(type) {
	case nil:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	case *[]byte:
		*out, err = io.ReadAll(resp.Body)
		return err
	default:
		return json.NewDecoder(resp.Body).Decode(out)
	}
}

func (d *Docker) Start(ctx context.Context, spec RunSpec) (string, error) {
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	type mount struct{ Type, Source, Target string }
	var mounts []mount
	for _, m := range spec.Mounts {
		mounts = append(mounts, mount{Type: "volume", Source: m.Volume, Target: m.Target})
	}
	body := map[string]any{
		"Image":  spec.Image,
		"Env":    env,
		"Labels": spec.Labels,
		"HostConfig": map[string]any{
			// tini as PID 1 reaps the agent's orphaned child processes.
			"Init":        true,
			"NetworkMode": spec.Network,
			"Mounts":      mounts,
		},
	}
	var created struct{ Id string }
	err := d.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {spec.Name}}, body, &created)
	if e, ok := err.(*apiError); ok && e.Status == http.StatusConflict {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+created.Id+"/start", nil, nil, nil); err != nil {
		_ = d.Remove(context.WithoutCancel(ctx), created.Id)
		return "", err
	}
	return created.Id, nil
}

func (d *Docker) Wait(ctx context.Context, id string) (int, error) {
	var res struct{ StatusCode int }
	if err := d.do(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, nil, &res); err != nil {
		return -1, err
	}
	return res.StatusCode, nil
}

func (d *Docker) Logs(ctx context.Context, id string, n int) (string, error) {
	var raw []byte
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {fmt.Sprint(n)}}
	if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil, &raw); err != nil {
		return "", err
	}
	return demux(raw), nil
}

// demux strips the 8-byte frame headers Docker adds to non-TTY log streams.
func demux(raw []byte) string {
	var sb strings.Builder
	for len(raw) >= 8 {
		size := int(binary.BigEndian.Uint32(raw[4:8]))
		raw = raw[8:]
		if size > len(raw) {
			size = len(raw)
		}
		sb.Write(raw[:size])
		raw = raw[size:]
	}
	return sb.String()
}

func (d *Docker) Remove(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}}, nil, nil)
}

func (d *Docker) List(ctx context.Context, labels map[string]string) ([]Sandbox, error) {
	var filter []string
	for k, v := range labels {
		filter = append(filter, k+"="+v)
	}
	f, _ := json.Marshal(map[string][]string{"label": filter})
	var res []struct {
		Id     string
		Labels map[string]string
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json", url.Values{"filters": {string(f)}}, nil, &res); err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(res))
	for _, c := range res {
		out = append(out, Sandbox{ID: c.Id, Labels: c.Labels})
	}
	return out, nil
}
