package ship

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/orchard-suite/tally/internal/agents"
)

// ReadInstalled reads the plugins Claude Code has installed from its own
// record of installs, installed_plugins.json: each is keyed
// <plugin>@<marketplace> and holds one entry a scope it is installed in.
// It reads a plugin's name, its marketplace, and its version, and nothing
// of where it is on disk. A missing file is a machine with no plugins.
func ReadInstalled(path string) (agents.Installed, error) {
	out := agents.Installed{Plugins: []agents.InstalledPlugin{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var v struct {
		Plugins map[string][]struct {
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return out, err
	}
	for key, installs := range v.Plugins {
		if len(installs) == 0 {
			continue
		}
		name, marketplace, _ := strings.Cut(key, "@")
		if name == "" {
			continue
		}
		// The newest version among its scopes: one machine, one plugin.
		version := ""
		for _, i := range installs {
			if i.Version > version {
				version = i.Version
			}
		}
		out.Plugins = append(out.Plugins, agents.InstalledPlugin{Name: name, Marketplace: marketplace, Version: version})
	}
	sort.Slice(out.Plugins, func(a, b int) bool {
		if out.Plugins[a].Name != out.Plugins[b].Name {
			return out.Plugins[a].Name < out.Plugins[b].Name
		}
		return out.Plugins[a].Marketplace < out.Plugins[b].Marketplace
	})
	if len(out.Plugins) > agents.MaxInstalled {
		out.Plugins = out.Plugins[:agents.MaxInstalled]
	}
	return out, nil
}
