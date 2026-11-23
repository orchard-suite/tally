package ship

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/agents"
)

// Ion keeps each conversation in <ion>/conversations: <id>.tree.jsonl holds
// a header line and the conversation's entries, <id>.llm.jsonl the model's
// context behind a header of its own, and a conversation from before the
// split is one <id>.jsonl in the tree's shape. Ion rewrites a file whole on
// every save, so a changed file is read again from the start and its
// replies are counted once by their entry ids.
//
// A run Ion hands to Claude Code is in Claude Code's transcripts, and Ion
// names that session in its header's nativeSessions, or in an agent_dispatch
// entry when it ran as a dispatched child. From DelegatedUsageSince on, Ion
// also copies such a run's usage onto its own replies, which would count it
// twice. Ion's API run stamps every reply it writes with a stop reason and a
// copied reply never has one, so a reply from then on counts only with a
// stop reason. Before then a copied reply carried no usage, and many API
// replies carried no stop reason.

// IonFile names the Ion conversation a path holds, and whether the path is
// a conversation file at all: a tree, or a file from before the split.
func IonFile(path string) (string, bool) {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, ".tree.jsonl"):
		return strings.TrimSuffix(base, ".tree.jsonl"), true
	case strings.HasSuffix(base, ".llm.jsonl"), !strings.HasSuffix(base, ".jsonl"):
		return "", false
	}
	id := strings.TrimSuffix(base, ".jsonl")
	return id, id != "" && !strings.Contains(id, ".")
}

// IonHeader is the part of a conversation's header lines the shipper reads.
// A tree's header has the folder and the native sessions; the context
// file's header has when the conversation began and where it came from. A
// file from before the split has all of it in one.
type IonHeader struct {
	CreatedAt        int64  `json:"createdAt"`
	WorkingDirectory string `json:"workingDirectory"`
	ParentID         string `json:"parentId"`
	ForkOf           string `json:"forkOf"`
	NativeSessions   map[string]struct {
		Cursor string `json:"cursor"`
	} `json:"nativeSessions"`
}

// merge fills h's empty fields from o.
func (h *IonHeader) merge(o IonHeader) {
	if h.CreatedAt == 0 {
		h.CreatedAt = o.CreatedAt
	}
	if h.WorkingDirectory == "" {
		h.WorkingDirectory = o.WorkingDirectory
	}
	if h.ParentID == "" {
		h.ParentID = o.ParentID
	}
	if h.ForkOf == "" {
		h.ForkOf = o.ForkOf
	}
	if len(h.NativeSessions) == 0 {
		h.NativeSessions = o.NativeSessions
	}
}

// ReadIonHeader reads the first line of r as a header.
func ReadIonHeader(r io.Reader) (IonHeader, error) {
	line, err := bufio.NewReaderSize(r, 1<<20).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return IonHeader{}, err
	}
	var h IonHeader
	_ = json.Unmarshal(line, &h)
	return h, nil
}

type ionEntry struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// DelegatedUsageSince is when Ion began copying a delegated run's usage
// onto its own replies.
var DelegatedUsageSince = time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC).UnixMilli()

type ionMessage struct {
	Role       string `json:"role"`
	Model      string `json:"model"`
	StopReason string `json:"stopReason"`
	Usage      *struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type ionDispatch struct {
	AgentName       string   `json:"agentName"`
	DisplayName     string   `json:"displayName"`
	ConversationID  string   `json:"conversationId"`
	ConversationIDs []string `json:"conversationIds"`
}

var (
	markUsage    = []byte(`"usage"`)
	markDispatch = []byte(`"agent_dispatch"`)
)

// ReadIon counts every reply in an Ion conversation file into c and
// returns its header, merged over base (the context file's header), and the
// conversations it dispatched, by id, with the dispatched agent's name. A
// conversation that was cut from or forked off another starts with copies
// of the other's entries; only entries from its createdAt on are its own.
// A last line without its newline is left for the next read.
func ReadIon(r io.Reader, c *Conv, base IonHeader, claim Claim) (IonHeader, map[string]string, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	h := base
	dispatched := map[string]string{}
	first := true
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			return h, dispatched, nil
		}
		if err != nil {
			return h, dispatched, err
		}
		if first {
			first = false
			var own IonHeader
			_ = json.Unmarshal(line, &own)
			h.merge(own)
			continue
		}
		usage, dispatch := bytes.Contains(line, markUsage), bytes.Contains(line, markDispatch)
		if !usage && !dispatch {
			continue
		}
		var e ionEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "message":
			if h.CreatedAt > 0 && e.Timestamp < h.CreatedAt && (h.ParentID != "" || h.ForkOf != "") {
				continue
			}
			countIon(e, c, claim)
		case "agent_dispatch":
			var d ionDispatch
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			name := d.DisplayName
			if name == "" {
				name = d.AgentName
			}
			for _, id := range append([]string{d.ConversationID}, d.ConversationIDs...) {
				if id != "" {
					dispatched[id] = name
				}
			}
		}
	}
}

func countIon(e ionEntry, c *Conv, claim Claim) {
	var m ionMessage
	if json.Unmarshal(e.Data, &m) != nil || m.Role != "assistant" || m.Usage == nil || m.Model == "" || e.Timestamp <= 0 {
		return
	}
	if m.StopReason == "" && e.Timestamp >= DelegatedUsageSince {
		return
	}
	key := replyKey("ion:"+e.ID, time.UnixMilli(e.Timestamp).UTC().Format(time.RFC3339Nano))
	if _, ok := c.Seen[key]; ok || !claim(key) {
		return
	}
	u := m.Usage
	// Ion asks for Anthropic's default cache, which is kept five minutes.
	add(c, key, m.Model, time.UnixMilli(e.Timestamp), agents.Tokens{Requests: 1, Input: u.Input, CacheWrite5m: u.CacheCreation, CacheRead: u.CacheRead, Output: u.Output})
}
