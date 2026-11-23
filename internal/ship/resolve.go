package ship

import (
	"path/filepath"
	"strings"

	"github.com/orchard-suite/tally/internal/agents"
)

// Known is everything the shipper keeps about one conversation for good,
// so it can place the conversation in its work long after its transcript
// went quiet.
type Known struct {
	// Source is the tool that wrote the transcript: empty for Claude Code,
	// "ion" for Ion.
	Source string `json:"source,omitempty"`
	// Session is the Claude Code session id, or the Ion conversation id.
	Session string `json:"session"`
	// Agent is the Claude Code subagent's id, empty for a session.
	Agent string `json:"agent,omitempty"`
	// ParentAgent is the subagent that started this one, when one did.
	ParentAgent string `json:"parent_agent,omitempty"`
	Info
	Ion *IonLinks `json:"ion,omitempty"`
	// Routed is set once a reply of the conversation went through the
	// shipper's route; only then are it and its work posted.
	Routed bool `json:"routed,omitempty"`
}

// Sources.
const (
	SourceClaude = ""
	SourceIon    = "ion"
)

// IonLinks are how an Ion conversation joins others.
type IonLinks struct {
	// Parent is the conversation this one continues from a checkpoint.
	// A fork has none: it is a work of its own.
	Parent string `json:"parent,omitempty"`
	// Dispatched are the conversations this one dispatched, Ion or Claude
	// Code, by id, with the dispatched agent's name.
	Dispatched map[string]string `json:"dispatched,omitempty"`
	// Native are the Claude Code sessions this conversation's own runs used.
	Native []string `json:"native,omitempty"`
}

// Resolve places every known conversation in its work. A conversation T3
// ran belongs to the work of the thread a person started: a lead when it
// is that thread's own, a child when T3 delegated it. T3 runs Claude Code
// by its session id and Ion through an ACP session, which Ion's bindings
// name. A dispatched conversation and a subagent are children of their
// parent's work; a Claude Code session an Ion conversation ran itself, and
// an Ion conversation continued from a checkpoint, share their parent's
// place. Anything else is a work of its own.
func Resolve(known map[string]Known, t3 T3, bindings map[string]string) ([]agents.Work, []agents.Conversation) {
	r := &resolver{known: known, t3: t3, ionThread: map[string]string{}, dispatchedBy: map[string]string{},
		dispatchTitle: map[string]string{}, nativeOf: map[string]string{}, memo: map[string]place{}, visiting: map[string]bool{}}
	for id, k := range known {
		if k.Source != SourceIon || k.Ion == nil {
			continue
		}
		for child, name := range k.Ion.Dispatched {
			for _, cid := range []string{"ion:" + child, "claude:" + child} {
				r.dispatchedBy[cid] = id
				r.dispatchTitle[cid] = name
			}
		}
		for _, s := range k.Ion.Native {
			r.nativeOf["claude:"+s] = id
		}
	}
	for key, conv := range bindings {
		thread, ok := t3.BySession[key]
		if !ok {
			continue
		}
		// The binding names the latest conversation of the session; the
		// ones it continues from are the same thread's.
		seen := map[string]bool{}
		for id := "ion:" + conv; id != "" && !seen[id]; {
			seen[id] = true
			if _, set := r.ionThread[id]; !set {
				r.ionThread[id] = thread
			}
			k := known[id]
			if k.Ion == nil || k.Ion.Parent == "" || r.dispatchedBy[id] != "" {
				break
			}
			id = "ion:" + k.Ion.Parent
		}
	}

	works := map[string]agents.Work{}
	convs := make([]agents.Conversation, 0, len(known))
	for id, k := range known {
		p := r.place(id)
		works[p.work.ID] = p.work
		agent := "claude"
		if k.Source == SourceIon {
			agent = "ion"
		}
		convs = append(convs, agents.Conversation{ID: id, Work: p.work.ID, Parent: p.parent, Agent: agent, Role: p.role, Title: r.title(id, k)})
	}
	out := make([]agents.Work, 0, len(works))
	for _, w := range works {
		out = append(out, w)
	}
	return out, convs
}

type place struct {
	work   agents.Work
	role   string
	parent string
}

type resolver struct {
	known map[string]Known
	t3    T3
	// ionThread is an Ion conversation's T3 thread.
	ionThread map[string]string
	// dispatchedBy is a dispatched conversation's Ion parent.
	dispatchedBy  map[string]string
	dispatchTitle map[string]string
	// nativeOf is a Claude Code session's Ion conversation, for a session
	// an Ion conversation ran itself.
	nativeOf map[string]string
	memo     map[string]place
	visiting map[string]bool
}

func (r *resolver) place(id string) place {
	if p, ok := r.memo[id]; ok {
		return p
	}
	k := r.known[id]
	if r.visiting[id] {
		return r.own(id, k)
	}
	r.visiting[id] = true
	p := r.resolve(id, k)
	delete(r.visiting, id)
	r.memo[id] = p
	return p
}

func (r *resolver) resolve(id string, k Known) place {
	if k.Source == SourceClaude && k.Agent != "" {
		parent := "claude:" + k.Session
		if k.ParentAgent != "" {
			parent += "/" + k.ParentAgent
		}
		return place{work: r.place("claude:" + k.Session).work, role: agents.Child, parent: parent}
	}
	if k.Source == SourceClaude {
		if thread, ok := r.t3.BySession[k.Session]; ok {
			return r.thread(thread)
		}
		if ion := r.nativeOf[id]; ion != "" {
			p := r.place(ion)
			return place{work: p.work, role: p.role, parent: ion}
		}
	} else if thread, ok := r.ionThread[id]; ok {
		return r.thread(thread)
	}
	if parent := r.dispatchedBy[id]; parent != "" {
		return place{work: r.place(parent).work, role: agents.Child, parent: parent}
	}
	if k.Source == SourceIon && k.Ion != nil && k.Ion.Parent != "" {
		return r.place("ion:" + k.Ion.Parent)
	}
	return r.own(id, k)
}

func (r *resolver) thread(thread string) place {
	root := r.t3.Root(thread)
	th := r.t3.Threads[root]
	p := place{work: agents.Work{ID: "t3:" + root, Title: titleOf(th.Title), Project: clip(th.Project)}, role: agents.Lead}
	if root != thread {
		p.role = agents.Child
	}
	return p
}

// own is a conversation as a work of its own.
func (r *resolver) own(id string, k Known) place {
	return place{work: agents.Work{ID: id, Title: titleOf(k.Title), Project: project(k.CWD)}, role: agents.Lead}
}

// title is what a conversation is called: its T3 thread's title, the name
// of the agent it was dispatched as, or the title it gave itself.
func (r *resolver) title(id string, k Known) string {
	thread, ok := r.ionThread[id]
	if k.Source == SourceClaude && k.Agent == "" {
		thread, ok = r.t3.BySession[k.Session]
	}
	if ok {
		if th := r.t3.Threads[thread]; th.Title != "" {
			return titleOf(th.Title)
		}
	}
	if name := r.dispatchTitle[id]; name != "" {
		return titleOf(name)
	}
	return titleOf(k.Title)
}

const untitled = "Untitled"

func titleOf(t string) string {
	if t = clip(t); t == "" {
		return untitled
	}
	return t
}

// clip trims t and keeps its first 120 characters.
func clip(t string) string {
	t = strings.TrimSpace(t)
	if r := []rune(t); len(r) > 120 {
		t = string(r[:120])
	}
	return t
}

// project is the name of the folder a session ran in.
func project(cwd string) string {
	if cwd == "" {
		return ""
	}
	return clip(filepath.Base(cwd))
}
