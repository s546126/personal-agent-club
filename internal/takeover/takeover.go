// Package takeover links a Grok Bot's own machine into a Persona hub as a node: the box side (persona-node running
// with reboot survival, a tailcat pairing, a dedicated sshd) is driven through the bot, and the hub side (one entry
// in a manager's machine list) is only prepared until the user confirms it.
//
// Nothing here prints or stores a secret. A report from the box is a fixed set of flags and a node name; a key, a
// token, an address or a path is never kept. The hub's machine list is read to see whether the node is already
// there, and written only by Apply, which refuses until the plan was shown to the user.
package takeover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Phases a takeover moves through, in order. A later one is not started while an earlier one is missing.
const (
	PhaseNode   = "node"   // the persona-node binary is present and the supervisor survives a reboot
	PhasePair   = "pair"   // tailcat is paired on the box; its address stays there and is not written to the hub
	PhaseSSH    = "ssh"    // the dedicated sshd accepts the hub's key, and the hub's ssh config names the node
	PhasePlan   = "plan"   // the hub change is described and waiting for the user
	PhaseLinked = "linked" // the user confirmed and the machine list was written, or it was already linked
)

// Report is what the box said about itself, reduced to flags. It carries no secret and no address: those stay on
// the box and on the hub, and are never sent back through the bot.
type Report struct {
	// Node is the machine's name on the hub (the ssh Host alias). Empty until the box has one.
	Node string
	// NodeReady: persona-node is installed and its supervisor is running, with the reboot hooks in place.
	NodeReady bool
	// Paired: tailcat was paired on the box. Its address stays there, so the hub entry Apply writes is ssh only.
	Paired bool
	// SSHReady: the dedicated sshd is up and the hub's ssh config names this node.
	SSHReady bool
	// Already: the box reports it was linked before, so nothing of it should be replaced.
	Already bool
	// Detail is one line safe to show. It must not contain a secret; a report that does is refused.
	Detail string
	// Reported is true once a line was parsed, so a takeover not yet asked is distinct from a node that is
	// missing.
	Reported bool
}

// secretLike is text a report must never carry: a private key block, a hub token, a database url, a tailcat key.
var secretLike = regexp.MustCompile(`(?i)(BEGIN [A-Z ]*PRIVATE KEY|database_url|access_token|refresh_token|"psk"|"key")`)

var nodeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ParseReport reads the one line the box's agent prints: "name=<node> node=ready|missing pair=ready|missing
// ssh=ready|missing already=yes|no". Anything else, or a line carrying a secret, is refused.
func ParseReport(line string) (Report, error) {
	line = strings.TrimSpace(line)
	if secretLike.MatchString(line) {
		return Report{}, fmt.Errorf("takeover: the box's report carries a secret and was dropped")
	}
	var r Report
	for _, f := range strings.Fields(line) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return Report{}, fmt.Errorf("takeover: a report field is not key=value")
		}
		switch k {
		case "name":
			r.Node = v
		case "node":
			r.NodeReady = v == "ready"
		case "pair":
			r.Paired = v == "ready"
		case "ssh":
			r.SSHReady = v == "ready"
		case "already":
			r.Already = v == "yes"
		default:
			return Report{}, fmt.Errorf("takeover: unknown report field %q", k)
		}
	}
	if r.Node != "" && !nodeName.MatchString(r.Node) {
		return Report{}, fmt.Errorf("takeover: the node name is not a plain name")
	}
	r.Reported = true
	return r, nil
}

// Phase is the first step still to do.
func (r Report) Phase() string {
	switch {
	case !r.Reported:
		return ""
	case r.Already:
		return PhaseLinked
	case !r.NodeReady:
		return PhaseNode
	case !r.Paired:
		return PhasePair
	case !r.SSHReady:
		return PhaseSSH
	default:
		return PhasePlan
	}
}

// Step is the instruction handed to the bot's own agent for one phase. It names what to set up and what one line to
// answer with; it carries no secret, no address and no path of the hub. The agent reads those on its own box.
func Step(phase, node string) (string, error) {
	if node != "" && !nodeName.MatchString(node) {
		return "", fmt.Errorf("takeover: the node name is not a plain name")
	}
	answer := "Answer with exactly one line: name=<node> node=ready|missing pair=ready|missing ssh=ready|missing already=yes|no. " +
		"Never print a key, a token, an address or a path."
	switch phase {
	case PhaseNode:
		return "Check this machine's persona-node. If ~/.persona/bin/persona-node is missing or older than the hub's build, " +
			"copy the hub's ~/.persona/bin/nodes/persona-node-linux-amd64 (check its SHA-256, back the old bin directory up first) " +
			"and restart only the persona-node child. Keep it running from ~/.persona/run-node.sh under setsid, with an autostart " +
			"entry and a bashrc guard, never from a terminal window. Then run persona-node node doctor and fix a generated-file mismatch. " + answer, nil
	case PhasePair:
		return "Pair this node with the hub once, with persona-node node tailcat init, and read the address with persona-node node " +
			"tailcat addr. Do not print that address. If it is already paired, leave it. " + answer, nil
	case PhaseSSH:
		name := node
		if name == "" {
			name = "the node name you report"
		}
		return "Give the hub a dedicated sshd on this machine, in ~/.persona/sshd, listening on localhost only, public-key only, " +
			"its own host keys, and an authorized_keys holding only the hub user's key restricted to localhost. Supervise it with " +
			"run-sshd.sh the same way as the node. On the hub, add an ssh config Host " + name + " for this machine and its host key " +
			"to known_hosts, then test a batch ssh. Change nothing about Tailscale. " + answer, nil
	default:
		return "", fmt.Errorf("takeover: nothing to ask the box for %q", phase)
	}
}

// Machine is the minimal view of a manager's machine entry used for lookup and for appending a new ssh entry.
// Existing entries stay as json.RawMessage so every Persona MachineConfig field (trust, address, launchers, etc.)
// is preserved untouched when Apply appends a new machine.
type Machine struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	SSHHost   string `json:"ssh_host,omitempty"`
	RemoteBin string `json:"remote_bin,omitempty"`
}

// managerFile is the part of manager.json this package reads and writes: the machine list (kept as raw JSON elements
// so existing entries are never truncated), and every other top-level key copied through untouched.
type managerFile struct {
	Machines []json.RawMessage
	rest     map[string]json.RawMessage
}

// Plan is the hub change waiting for the user. It describes the ssh entry and never contains a secret or tailcat
// address.
type Plan struct {
	ID            string
	Node          string
	Kind          string
	SSHHost       string
	RemoteBin     string
	Summary       string
	AlreadyLinked bool
}

// Prepare reads manager.json and describes the change for node without writing anything. An entry already present
// for that name or id is reported as already linked and would not be overwritten.
func Prepare(path, node, remoteBin string) (Plan, error) {
	if !nodeName.MatchString(node) {
		return Plan{}, fmt.Errorf("takeover: the node name is not a plain name")
	}
	m, err := readManager(path)
	if err != nil {
		return Plan{}, err
	}
	for _, raw := range m.Machines {
		e, err := parseMachine(raw)
		if err != nil {
			return Plan{}, err
		}
		if e.Name == node || e.ID == node {
			return Plan{Node: node, Kind: e.Kind, SSHHost: e.SSHHost, AlreadyLinked: true,
				Summary: "already linked; nothing will be written"}, nil
		}
	}
	return Plan{Node: node, Kind: "ssh", SSHHost: node, RemoteBin: remoteBin,
		Summary: "add " + node + " over ssh (no tailcat fallback address)"}, nil
}

// Apply writes the planned entry into manager.json, after copying it aside. It refuses when the plan says the node
// is already linked, and it never touches an entry that is already there: existing entries are kept as raw JSON and
// a second apply is a no-op that reports the node as already linked. The caller restarts nothing; that is a separate
// decision.
func Apply(path string, plan Plan, now time.Time) (backup string, err error) {
	if plan.AlreadyLinked {
		return "", fmt.Errorf("takeover: already linked; nothing to write")
	}
	if !nodeName.MatchString(plan.Node) || plan.Kind != "ssh" || plan.SSHHost == "" {
		return "", fmt.Errorf("takeover: the plan is not a complete ssh entry")
	}
	m, err := readManager(path)
	if err != nil {
		return "", err
	}
	for _, raw := range m.Machines {
		e, err := parseMachine(raw)
		if err != nil {
			return "", err
		}
		if e.Name == plan.Node || e.ID == plan.Node {
			return "", fmt.Errorf("takeover: %s is already in the machine list", plan.Node)
		}
	}
	backup = path + ".bak-" + now.UTC().Format("20060102T150405Z")
	if err := copyFile(path, backup); err != nil {
		return "", err
	}
	entry, err := json.Marshal(Machine{
		ID:        plan.Node,
		Name:      plan.Node,
		Kind:      plan.Kind,
		SSHHost:   plan.SSHHost,
		RemoteBin: plan.RemoteBin,
	})
	if err != nil {
		return backup, err
	}
	m.Machines = append(m.Machines, entry)
	if err := writeManager(path, m); err != nil {
		return backup, err
	}
	return backup, nil
}

func parseMachine(raw json.RawMessage) (Machine, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return Machine{}, fmt.Errorf("takeover: the machine list is not valid")
	}
	var e Machine
	if err := json.Unmarshal(raw, &e); err != nil {
		return Machine{}, fmt.Errorf("takeover: the machine list is not valid")
	}
	return e, nil
}

func readManager(path string) (managerFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return managerFile{}, fmt.Errorf("takeover: reading the machine list: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return managerFile{}, fmt.Errorf("takeover: the machine list is not valid")
	}
	machines := []json.RawMessage{}
	if v, ok := raw["machines"]; ok {
		var list []json.RawMessage
		if err := json.Unmarshal(v, &list); err != nil {
			return managerFile{}, fmt.Errorf("takeover: the machine list is not valid")
		}
		for _, item := range list {
			if _, err := parseMachine(item); err != nil {
				return managerFile{}, err
			}
		}
		if list != nil {
			machines = list
		}
		delete(raw, "machines")
	}
	return managerFile{Machines: machines, rest: raw}, nil
}

func writeManager(path string, m managerFile) error {
	out := map[string]any{}
	for k, v := range m.rest {
		out[k] = json.RawMessage(v)
	}
	out["machines"] = m.Machines
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o600)
}
