// Package cli implements the ledger command line. Every command writes one
// JSON document to stdout: {"ok":true,"result":...} on success or
// {"ok":false,"error":{...}} with exit status 1 on failure.
package cli

import (
	"encoding/json"
	"io"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

// version is set at release build time with -ldflags "-X ...cli.version=v1.2.3".
var version = ""

type command struct {
	summary string
	run     func(args []string) (any, error)
}

func commands() map[string]command {
	return map[string]command{
		"daemon":       {"serve the core API over the repository socket", cmdDaemon},
		"capability":   {"issue or delegate scoped authority", liveCommand("capability")},
		"coordination": {"inspect advisory symbols, poll/ack notifications, manage advisory leases", liveCommand("coordination")},
		"session":      {"create, end or recover a signing session", liveCommand("session")},
		"evidence":     {"resolve a binding, run a check, judge a criterion, query independence", liveCommand("evidence")},
		"selection":    {"record a selection decision (not acceptance; nothing is merged)", liveCommand("selection")},
		"intent":       {"create, inspect, evaluate criteria of or close an intent", liveCommand("intent")},
		"workspace":    {"fork, inspect, propose, rework or close a workspace", liveCommand("workspace")},
		"system":       {"inspect daemon status", liveCommand("system")},
		"init":         {"initialize Ledger in the current Git repository", cmdInit},
		"recover":      {"reconcile record files, the records ref, and the index", cmdRecover},
		"reindex":      {"rebuild the index from the record files", cmdReindex},
		"export":       {"write a portable provenance bundle (no daemon needed)", cmdExport},
		"verify":       {"verify this repository or a bundle: integrity and trust reported separately", cmdVerify},
		"import":       {"stage, verify and import a bundle; grants no trust and imports no keys", cmdImport},
		"status":       {"list subjects and their derived state", cmdStatus},
		"version":      {"print version information", cmdVersion},
	}
}

// Main runs the command named by args[0] and returns the process exit status.
func Main(args []string, stdout io.Writer) int {
	cmds := commands()
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		return emit(stdout, usage(cmds), nil)
	}
	c, ok := cmds[args[0]]
	if !ok {
		return emit(stdout, nil, apierr.New("unknown_command", "unknown command %q", args[0]))
	}
	result, err := c.run(args[1:])
	return emit(stdout, result, err)
}

func emit(w io.Writer, result any, err error) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err != nil {
		_ = enc.Encode(map[string]any{"ok": false, "error": apierr.As(err)})
		return 1
	}
	if err := enc.Encode(map[string]any{"ok": true, "result": result}); err != nil {
		return 1
	}
	return 0
}

func usage(cmds map[string]command) map[string]any {
	names := make([]string, 0, len(cmds))
	for n := range cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	lines := make([]string, len(names))
	for i, n := range names {
		lines[i] = n + ": " + cmds[n].summary
	}
	return map[string]any{"usage": "ledger <command> [flags]", "commands": lines}
}

func cmdVersion(args []string) (any, error) {
	if err := noFlags("version", args); err != nil {
		return nil, err
	}
	v := version
	if v == "" {
		v = "devel"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	return map[string]string{"version": strings.TrimPrefix(v, "v")}, nil
}
