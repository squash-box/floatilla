package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeGitHub(t *testing.T, body string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer ghp_x" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSetupGitWithTokenUsesTheAccountIdentity(t *testing.T) {
	var calls []string
	env, err := SetupGit(context.Background(), GitConfig{
		Token:  "ghp_x",
		APIURL: fakeGitHub(t, `{"id": 42, "login": "octo", "name": "Octo Cat"}`),
		Git: func(_ context.Context, args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GIT_AUTHOR_NAME=Octo Cat", "GIT_COMMITTER_NAME=Octo Cat",
		"GIT_AUTHOR_EMAIL=42+octo@users.noreply.github.com", "GIT_COMMITTER_EMAIL=42+octo@users.noreply.github.com",
	}
	if strings.Join(env, "|") != strings.Join(want, "|") {
		t.Fatalf("env:\n got %q\nwant %q", env, want)
	}
	joined := strings.Join(calls, "\n")
	for _, w := range []string{"!gh auth git-credential", "url.https://github.com/.insteadOf git@github.com:"} {
		if !strings.Contains(joined, w) {
			t.Fatalf("git config missing %q:\n%s", w, joined)
		}
	}
	for _, c := range calls {
		if strings.Contains(c, "ghp_x") {
			t.Fatalf("the token must not be written to git config: %s", c)
		}
	}
}

func TestSetupGitExplicitIdentityWins(t *testing.T) {
	env, err := SetupGit(context.Background(), GitConfig{
		Token: "ghp_x", Name: "Alex", Email: "alex@example.com",
		APIURL: "http://127.0.0.1:1", // not called
		Git:    func(context.Context, ...string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(env, "|"), "GIT_AUTHOR_NAME=Alex|GIT_COMMITTER_NAME=Alex|GIT_AUTHOR_EMAIL=alex@example.com") {
		t.Fatalf("env: %q", env)
	}
}

func TestSetupGitBadTokenKeepsGoing(t *testing.T) {
	env, err := SetupGit(context.Background(), GitConfig{
		Token: "wrong", Email: "alex@example.com",
		APIURL: fakeGitHub(t, `{}`),
		Git:    func(context.Context, ...string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(env, "|") != "GIT_AUTHOR_EMAIL=alex@example.com|GIT_COMMITTER_EMAIL=alex@example.com" {
		t.Fatalf("env: %q", env)
	}
}

func TestSetupGitWithoutTokenOnlySetsIdentity(t *testing.T) {
	env, err := SetupGit(context.Background(), GitConfig{
		Name: "Alex",
		Git: func(context.Context, ...string) error {
			t.Fatal("no token: git config must be left alone")
			return nil
		},
	})
	if err != nil || strings.Join(env, "|") != "GIT_AUTHOR_NAME=Alex|GIT_COMMITTER_NAME=Alex" {
		t.Fatalf("env %q err %v", env, err)
	}
}
