package runners

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/orchard-suite/tally/internal/collect"
)

// GitLabNames looks up GitLab projects' paths by id, and keeps each one it
// learns for as long as tally runs: a project's id never changes owner.
type GitLabNames struct {
	base  string
	token string
	http  *http.Client
	inst  *collect.Instruments

	mu    sync.Mutex
	paths map[string]string
}

// GitLabFromEnv reads GITLAB_URL and GITLAB_TOKEN, a token that may read
// the API (read_api), or nil without both.
func GitLabFromEnv(inst *collect.Instruments) *GitLabNames {
	base, token := os.Getenv("GITLAB_URL"), os.Getenv("GITLAB_TOKEN")
	if base == "" || token == "" {
		return nil
	}
	return &GitLabNames{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}, inst: inst, paths: map[string]string{}}
}

// Names is each id's path, such as group/web. A project the token
// cannot see, or one deleted since its jobs ran, has no name.
func (g *GitLabNames) Names(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		g.mu.Lock()
		path, known := g.paths[id]
		g.mu.Unlock()
		if !known {
			var err error
			if path, err = g.project(ctx, id); err != nil {
				return nil, err
			}
			g.mu.Lock()
			g.paths[id] = path
			g.mu.Unlock()
		}
		if path != "" {
			out[id] = path
		}
	}
	return out, nil
}

func (g *GitLabNames) project(ctx context.Context, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/api/v4/projects/"+id, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	res, err := g.http.Do(req)
	if err != nil {
		g.inst.Upstream(ctx, "gitlab", collect.Failed)
		return "", fmt.Errorf("gitlab project %s: %w", id, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		g.inst.Upstream(ctx, "gitlab", collect.Failed)
		return "", err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		g.inst.Upstream(ctx, "gitlab", collect.OK)
		return "", nil
	case res.StatusCode == http.StatusTooManyRequests:
		g.inst.Upstream(ctx, "gitlab", collect.Refused)
		return "", fmt.Errorf("gitlab project %s: %s", id, res.Status)
	case res.StatusCode != http.StatusOK:
		g.inst.Upstream(ctx, "gitlab", collect.Failed)
		return "", fmt.Errorf("gitlab project %s: %s", id, res.Status)
	}
	g.inst.Upstream(ctx, "gitlab", collect.OK)
	return ProjectPath(raw)
}

// ProjectPath reads a project's path from GitLab's answer.
func ProjectPath(raw []byte) (string, error) {
	var p struct {
		Path string `json:"path_with_namespace"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("gitlab project: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("gitlab project: no path_with_namespace")
	}
	return p.Path, nil
}
