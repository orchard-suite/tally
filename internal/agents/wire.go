// Package agents is what AI agent work costs: the records a machine's
// shipper posts, the store that keeps them, the prices that turn tokens
// into dollars, and the day report the board draws.
package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Batch is one post from a machine's shipper to POST /v1/usage. The machine
// is the token's, never the body's. Every record is an upsert: a bucket
// carries the whole minute's totals, so a batch sent twice stores once.
type Batch struct {
	Works         []Work         `json:"works,omitempty"`
	Conversations []Conversation `json:"conversations,omitempty"`
	Buckets       []Bucket       `json:"buckets,omitempty"`
	Uses          []Use          `json:"uses,omitempty"`
	// Installed is every plugin the machine has installed, when the shipper
	// says: the whole list, replacing what the machine said before. Nil says
	// nothing; an empty list says it has none.
	Installed *Installed `json:"installed,omitempty"`
}

// Installed is the plugins one machine has installed.
type Installed struct {
	Plugins []InstalledPlugin `json:"plugins"`
}

// InstalledPlugin is one plugin a machine has installed, as its agent's own
// record of installs names it.
type InstalledPlugin struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace,omitempty"`
	Version     string `json:"version,omitempty"`
}

// Bare is the id a use is posted under when its conversation must stay on
// the machine: a digest of the conversation's id, so a minute's count still
// replaces itself and nothing says which conversation it was.
func Bare(conversation string) string {
	h := sha256.Sum256([]byte("tally-use:" + conversation))
	return "x-" + hex.EncodeToString(h[:12])
}

// Work is one piece of work a person started: a T3 thread with everything
// it delegated, or a bare agent session.
type Work struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Project string `json:"project"`
}

// Conversation is one agent transcript: a session, or a subagent inside one.
type Conversation struct {
	ID string `json:"id"`
	// Work is the Work this conversation is part of.
	Work string `json:"work"`
	// Parent is the conversation that started this one, empty for a lead.
	Parent string `json:"parent,omitempty"`
	// Agent is the tool that wrote the transcript, such as claude.
	Agent string `json:"agent"`
	// Role is lead (the conversation a person drives) or child.
	Role  string `json:"role"`
	Title string `json:"title"`
}

// Roles.
const (
	Lead  = "lead"
	Child = "child"
)

// Bucket is one conversation's use of one model in one minute.
type Bucket struct {
	Conversation string    `json:"conversation"`
	Model        string    `json:"model"`
	Minute       time.Time `json:"minute"`
	Tokens
}

// Use is how many times one conversation used one skill, command, or tool
// in one minute, in replies of one model. It is an upsert like a bucket: it
// carries the whole minute's count. It names what was used and never what it
// was asked.
type Use struct {
	Conversation string    `json:"conversation"`
	Model        string    `json:"model"`
	Minute       time.Time `json:"minute"`
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	Count        int64     `json:"count"`
}

// Kinds of use. A skill is one the agent loaded; a command is a slash
// command the person typed, which may be a skill; a tool is any tool call,
// an MCP server's included.
const (
	UseSkill   = "skill"
	UseCommand = "command"
	UseTool    = "tool"
)

// Plugin is the plugin a use belongs to, or "" for one of no plugin. A
// plugin's skill or command is named <plugin>:<skill>, and a plugin's MCP
// tool mcp__plugin_<plugin>_<server>__<tool>; plugin names never hold an
// underscore.
func (u Use) Plugin() string {
	switch u.Kind {
	case UseSkill, UseCommand:
		if p, _, ok := strings.Cut(u.Name, ":"); ok {
			return p
		}
	case UseTool:
		if rest, ok := strings.CutPrefix(u.Name, "mcp__plugin_"); ok {
			if p, _, ok := strings.Cut(rest, "_"); ok {
				return p
			}
		}
	}
	return ""
}

// Skill is the skill a skill or command names, without its plugin.
func (u Use) Skill() string {
	if _, s, ok := strings.Cut(u.Name, ":"); ok {
		return s
	}
	return u.Name
}

// Tokens are counts by kind. A cache write is priced by how long it is
// kept: five minutes or an hour.
type Tokens struct {
	Requests     int64 `json:"requests"`
	Input        int64 `json:"input"`
	CacheWrite5m int64 `json:"cache_write_5m"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	CacheRead    int64 `json:"cache_read"`
	Output       int64 `json:"output"`
}

// Add adds o to t.
func (t *Tokens) Add(o Tokens) {
	t.Requests += o.Requests
	t.Input += o.Input
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
	t.CacheRead += o.CacheRead
	t.Output += o.Output
}

// Total is every token: input, cache writes, cache reads, and output.
func (t Tokens) Total() int64 {
	return t.Input + t.CacheWrite5m + t.CacheWrite1h + t.CacheRead + t.Output
}

// Limits on one batch, so one post stays small.
const (
	MaxRecords = 5000
	// MaxInstalled is the most plugins one machine may say it has.
	MaxInstalled = 500
	maxID        = 200
	maxText      = 300
	maxName      = 200
)

// Validate refuses a batch the store must not take.
func (b Batch) Validate() error {
	if n := len(b.Works) + len(b.Conversations) + len(b.Buckets) + len(b.Uses); n == 0 && b.Installed == nil {
		return errors.New("empty batch")
	} else if n > MaxRecords {
		return fmt.Errorf("%d records, more than %d", n, MaxRecords)
	}
	for _, w := range b.Works {
		if err := id("work", w.ID); err != nil {
			return err
		}
		if len(w.Title) > maxText || len(w.Project) > maxText {
			return fmt.Errorf("work %s: title or project too long", w.ID)
		}
	}
	for _, c := range b.Conversations {
		if err := id("conversation", c.ID); err != nil {
			return err
		}
		if err := id("conversation work", c.Work); err != nil {
			return err
		}
		if c.Role != Lead && c.Role != Child {
			return fmt.Errorf("conversation %s: role %q is not lead or child", c.ID, c.Role)
		}
		if c.Agent == "" || len(c.Agent) > 40 || len(c.Parent) > maxID || len(c.Title) > maxText {
			return fmt.Errorf("conversation %s: agent, parent, or title out of bounds", c.ID)
		}
	}
	for _, k := range b.Buckets {
		if err := id("bucket conversation", k.Conversation); err != nil {
			return err
		}
		if k.Model == "" || len(k.Model) > 100 {
			return fmt.Errorf("bucket %s: model out of bounds", k.Conversation)
		}
		if k.Minute.IsZero() || !k.Minute.Equal(k.Minute.Truncate(time.Minute)) {
			return fmt.Errorf("bucket %s: minute %s is not a whole minute", k.Conversation, k.Minute)
		}
		if k.Requests < 0 || k.Input < 0 || k.CacheWrite5m < 0 || k.CacheWrite1h < 0 || k.CacheRead < 0 || k.Output < 0 {
			return fmt.Errorf("bucket %s: a negative count", k.Conversation)
		}
	}
	if b.Installed != nil {
		if len(b.Installed.Plugins) > MaxInstalled {
			return fmt.Errorf("%d installed plugins, more than %d", len(b.Installed.Plugins), MaxInstalled)
		}
		for _, p := range b.Installed.Plugins {
			if p.Name == "" || len(p.Name) > maxName || len(p.Marketplace) > maxName || len(p.Version) > 100 {
				return fmt.Errorf("installed plugin %q: name, marketplace, or version out of bounds", p.Name)
			}
		}
	}
	for _, u := range b.Uses {
		if err := id("use conversation", u.Conversation); err != nil {
			return err
		}
		if u.Model == "" || len(u.Model) > 100 {
			return fmt.Errorf("use %s: model out of bounds", u.Conversation)
		}
		if u.Minute.IsZero() || !u.Minute.Equal(u.Minute.Truncate(time.Minute)) {
			return fmt.Errorf("use %s: minute %s is not a whole minute", u.Conversation, u.Minute)
		}
		if u.Kind != UseSkill && u.Kind != UseCommand && u.Kind != UseTool {
			return fmt.Errorf("use %s: kind %q is not skill, command, or tool", u.Conversation, u.Kind)
		}
		if u.Name == "" || len(u.Name) > maxName {
			return fmt.Errorf("use %s: name out of bounds", u.Conversation)
		}
		if u.Count < 0 {
			return fmt.Errorf("use %s: a negative count", u.Conversation)
		}
	}
	return nil
}

func id(what, v string) error {
	if v == "" || len(v) > maxID {
		return fmt.Errorf("%s id %q out of bounds", what, v)
	}
	return nil
}
