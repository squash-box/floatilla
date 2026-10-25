package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// GitConfig is the sandbox's git identity and GitHub access.
type GitConfig struct {
	// Token is a GitHub token (GH_TOKEN): HTTPS pushes and the gh CLI use it.
	Token string
	// Name and Email author the agent's commits. When empty and Token is
	// set, they come from the token's GitHub account (its noreply address).
	Name  string
	Email string
	// APIURL is GitHub's REST API; replaced in tests.
	APIURL string
	// Git runs git; replaced in tests.
	Git func(ctx context.Context, args ...string) error
}

func GitConfigFromEnv() GitConfig {
	return GitConfig{
		Token: os.Getenv("GH_TOKEN"),
		Name:  os.Getenv("FLOATILLA_GIT_NAME"),
		Email: os.Getenv("FLOATILLA_GIT_EMAIL"),
	}
}

// SetupGit configures git for the run: GitHub over HTTPS with the token
// (SSH remotes are rewritten, since sandboxes have no keys) and the commit
// identity. It returns environment entries for the agent runtime. Failures
// are returned with whatever setup succeeded, so the run can continue
// without git access.
func SetupGit(ctx context.Context, cfg GitConfig) ([]string, error) {
	if cfg.Git == nil {
		cfg.Git = func(ctx context.Context, args ...string) error {
			out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}

	var env []string
	var errs []string
	if cfg.Token != "" {
		for _, args := range [][]string{
			// gh reads GH_TOKEN from the environment.
			{"config", "--global", "credential.https://github.com.helper", ""},
			{"config", "--global", "--add", "credential.https://github.com.helper", "!gh auth git-credential"},
			{"config", "--global", "url.https://github.com/.insteadOf", "git@github.com:"},
			{"config", "--global", "--add", "url.https://github.com/.insteadOf", "ssh://git@github.com/"},
		} {
			if err := cfg.Git(ctx, args...); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if cfg.Name == "" || cfg.Email == "" {
			name, email, err := githubIdentity(ctx, cfg.APIURL, cfg.Token)
			if err != nil {
				errs = append(errs, "github identity: "+err.Error())
			}
			if cfg.Name == "" {
				cfg.Name = name
			}
			if cfg.Email == "" {
				cfg.Email = email
			}
		}
	}
	if cfg.Name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+cfg.Name, "GIT_COMMITTER_NAME="+cfg.Name)
	}
	if cfg.Email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+cfg.Email, "GIT_COMMITTER_EMAIL="+cfg.Email)
	}
	if len(errs) > 0 {
		return env, fmt.Errorf("git setup: %s", strings.Join(errs, "; "))
	}
	return env, nil
}

// githubIdentity returns the token's account name (or login) and its noreply
// email, which GitHub attributes to the account without exposing an address.
func githubIdentity(ctx context.Context, apiURL, token string) (name, email string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+"/user", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET /user: %s", resp.Status)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", "", err
	}
	name = u.Name
	if name == "" {
		name = u.Login
	}
	return name, fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login), nil
}
