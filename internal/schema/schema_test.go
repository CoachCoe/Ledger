package schema

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

func TestRuntimeSchemas(t *testing.T) {
	sym := `{"path":"a.py","language":"python","name":"f","kind":"function","disambiguator":null}`
	ses, wsp, run := "ses_"+strings.Repeat("a", 26), "wsp_"+strings.Repeat("a", 26), "run_"+strings.Repeat("a", 26)
	valid := map[string]string{
		"notification": `{"id":"ntf_` + strings.Repeat("a", 26) + `","recipient":"` + ses + `","kind":"overlap",
			"subject":{"symbol":` + sym + `},"counterpart":null,"confidence":"direct",
			"created_at":"2026-11-05T12:00:00Z","acknowledged_at":null}`,
		"lease": `{"id":"lse_` + strings.Repeat("a", 26) + `","holder":"` + ses + `","paths":["a.py"],"symbols":[],
			"reason":"refactor","expires_at":"2026-11-05T12:00:00Z"}`,
		"dependency_edge": `{"source":` + sym + `,"target":` + sym + `,"relation":"read","confidence":"unresolved","observed_by":"tree-sitter"}`,
		"run_handoff": `{"run_id":"` + run + `","attempts":[{"workspace_id":"` + wsp + `","session_id":"` + ses + `",
			"capability_id":"cap_` + strings.Repeat("a", 26) + `","strategy":"inline","path":"/tmp/w"}]}`,
		"run_status": `{"run_id":"` + run + `","state":"collecting","deadline":"2026-11-05T12:00:00Z",
			"attempts":[{"workspace_id":"` + wsp + `","manifest":null,"submitted":false}],"selection":null}`,
	}
	for name, doc := range valid {
		if err := ValidateRuntime(name, []byte(doc)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		bad := strings.Replace(doc, "{", `{"extra":1,`, 1)
		if err := ValidateRuntime(name, []byte(bad)); !apierr.HasCode(err, "schema_invalid") {
			t.Errorf("%s with unknown member: %v", name, err)
		}
	}
	if err := ValidateRuntime("nope", []byte(`{}`)); !apierr.HasCode(err, "unknown_schema") {
		t.Errorf("unknown runtime schema: %v", err)
	}
}

func TestNewID(t *testing.T) {
	re := regexp.MustCompile(`^int_[0-9a-v]{26}$`)
	a, b := NewID(PrefixIntent), NewID(PrefixIntent)
	if !re.MatchString(a) || a == b {
		t.Fatalf("ids %q %q", a, b)
	}
	if IDPrefix(a) != PrefixIntent || IDPrefix("plain") != "" {
		t.Fatal("IDPrefix")
	}
}

func TestTimestamp(t *testing.T) {
	ts := Timestamp(time.Date(2026, 11, 5, 7, 0, 0, 123456789, time.FixedZone("x", -5*3600)))
	if ts != "2026-11-05T12:00:00.123Z" {
		t.Fatalf("Timestamp = %s", ts)
	}
	if _, err := ParseTimestamp(ts); err != nil {
		t.Fatal(err)
	}
}
