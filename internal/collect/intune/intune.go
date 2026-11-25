// Package intune enrolls the machines a fleet's management installed the
// shipper on. A script Intune runs on each machine makes the machine's
// shipper token there, keeps it on the machine, and reports only its hash
// as a line of its output: tally-ship=<sha256 hex>. This collector reads
// those outputs through Microsoft Graph and hands the agents collector the
// machines they name, each by its Intune device name in lowercase. A machine
// Intune no longer manages drops out with its run state. Nobody keeps a list.
package intune

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/orchard-suite/tally/internal/collect"
	"github.com/orchard-suite/tally/internal/collect/azure"
)

// Marker starts the line a script reports a token's hash on.
const Marker = "tally-ship="

// Graph is Microsoft Graph's audience.
const Graph = "https://graph.microsoft.com"

// Script is one Intune script whose output names a machine's token: a
// macOS shell script (mac) or a Windows remediation (windows), by its id.
type Script struct {
	Kind string
	ID   string
}

// ParseScripts reads SHIP_INTUNE: mac:<script id> and windows:<remediation
// id>, comma separated.
func ParseScripts(s string) ([]Script, error) {
	var out []Script
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, id, ok := strings.Cut(part, ":")
		if !ok || (kind != "mac" && kind != "windows") || id == "" {
			return nil, fmt.Errorf("SHIP_INTUNE: want mac:<id> or windows:<id>, got %q", part)
		}
		out = append(out, Script{Kind: kind, ID: id})
	}
	return out, nil
}

// path is the Graph path of a script's run on every device.
func (s Script) path() string {
	if s.Kind == "mac" {
		return "/beta/deviceManagement/deviceShellScripts/" + url.PathEscape(s.ID) + "/deviceRunStates"
	}
	return "/beta/deviceManagement/deviceHealthScripts/" + url.PathEscape(s.ID) + "/deviceRunStates"
}

// RunState is the part of a device's run of a script this reads.
type RunState struct {
	ResultMessage                  string `json:"resultMessage"`
	PreRemediationDetectionOutput  string `json:"preRemediationDetectionScriptOutput"`
	PostRemediationDetectionOutput string `json:"postRemediationDetectionScriptOutput"`
	LastStateUpdateDateTime        string `json:"lastStateUpdateDateTime"`
	ManagedDevice                  *struct {
		DeviceName string `json:"deviceName"`
	} `json:"managedDevice"`
}

// HashIn is the token hash a script's output reports, or "" for none. The
// last line that names one wins.
func HashIn(output string) string {
	var hash string
	for _, line := range strings.Split(output, "\n") {
		_, rest, ok := strings.Cut(strings.TrimSpace(line), Marker)
		if !ok {
			continue
		}
		h := strings.ToLower(strings.TrimSpace(strings.Fields(rest + " ")[0]))
		if b, err := hex.DecodeString(h); err == nil && len(b) == 32 {
			hash = h
		}
	}
	return hash
}

// Machines are the machines the run states name, by the hash of each one's
// token. A Windows remediation's output after the fix wins over its output
// before it. Two machines that report one hash name neither: a token is one
// machine's.
func Machines(states []RunState) map[string]string {
	out := map[string]string{}
	dup := map[string]bool{}
	for _, st := range states {
		if st.ManagedDevice == nil || st.ManagedDevice.DeviceName == "" {
			continue
		}
		hash := HashIn(st.ResultMessage)
		if hash == "" {
			hash = HashIn(st.PostRemediationDetectionOutput)
		}
		if hash == "" {
			hash = HashIn(st.PreRemediationDetectionOutput)
		}
		if hash == "" {
			continue
		}
		machine := strings.ToLower(st.ManagedDevice.DeviceName)
		if m, ok := out[hash]; ok && m != machine {
			dup[hash] = true
		}
		out[hash] = machine
	}
	for h := range dup {
		delete(out, h)
	}
	return out
}

// Enroller hands the agents collector the machines Intune's scripts name.
type Enroller interface {
	Enroll(byHash map[string]string)
}

// Collector reads the scripts' run states every interval.
type Collector struct {
	scripts  []Script
	token    func(context.Context) (string, error)
	agents   Enroller
	inst     *collect.Instruments
	client   *http.Client
	base     string
	interval time.Duration
	log      *slog.Logger
}

// FromEnv turns the collector on with SHIP_INTUNE and the Azure identity,
// which needs Graph's DeviceManagementScripts.Read.All and
// DeviceManagementManagedDevices.Read.All. It is off without SHIP_INTUNE or
// the agents collector.
func FromEnv(log *slog.Logger, inst *collect.Instruments, agents Enroller) (*Collector, error) {
	spec := os.Getenv("SHIP_INTUNE")
	if spec == "" {
		log.Info("intune collector off: SHIP_INTUNE is not set")
		return nil, nil
	}
	if agents == nil {
		log.Info("intune collector off: it needs the agents collector")
		return nil, nil
	}
	scripts, err := ParseScripts(spec)
	if err != nil {
		return nil, err
	}
	id, err := azure.FromIdentityEnv()
	if err != nil {
		return nil, fmt.Errorf("SHIP_INTUNE: %w", err)
	}
	return &Collector{scripts: scripts, token: azure.Tokens(id, Graph), agents: agents, inst: inst, client: &http.Client{Timeout: time.Minute},
		base: Graph, interval: collect.Interval(os.Getenv("SHIP_INTUNE_INTERVAL"), 15*time.Minute), log: log}, nil
}

func (c *Collector) Name() string { return "intune" }

// Interval is how often a round runs when the last one worked.
func (c *Collector) Interval() time.Duration { return c.interval }

// Run reads every interval.
func (c *Collector) Run(ctx context.Context, done func(ok bool)) error {
	return collect.Every(ctx, c.Name(), c.interval, c.log, done, c.Round)
}

// Round reads every script's run states and enrolls what they name. A
// failed read enrolls nothing new and keeps the last machines.
func (c *Collector) Round(ctx context.Context) error {
	var states []RunState
	for _, s := range c.scripts {
		got, err := c.read(ctx, s)
		if err != nil {
			return fmt.Errorf("%s script %s: %w", s.Kind, s.ID, err)
		}
		states = append(states, got...)
	}
	machines := Machines(states)
	c.agents.Enroll(machines)
	c.log.InfoContext(ctx, "intune machines enrolled", "machines", len(machines), "run_states", len(states))
	return nil
}

// read reads one script's run states, page by page.
func (c *Collector) read(ctx context.Context, s Script) ([]RunState, error) {
	next := c.base + s.path() + "?$expand=managedDevice($select=deviceName)&$top=200"
	var out []RunState
	for next != "" {
		token, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := c.client.Do(req)
		c.inst.Upstream(ctx, "graph", outcome(res, err))
		if err != nil {
			return nil, err
		}
		var page struct {
			Value []RunState `json:"value"`
			Next  string     `json:"@odata.nextLink"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		err = json.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			msg := res.Status
			if page.Error != nil {
				msg = page.Error.Message
			}
			return nil, errors.New(msg)
		}
		out = append(out, page.Value...)
		next = page.Next
	}
	return out, nil
}

func outcome(res *http.Response, err error) string {
	switch {
	case err != nil:
		return collect.Failed
	case res.StatusCode == http.StatusTooManyRequests:
		return collect.Refused
	case res.StatusCode != http.StatusOK:
		return collect.Failed
	}
	return collect.OK
}
