package evidence

import (
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/schema"
)

func valid() schema.CheckDefinition {
	return schema.CheckDefinition{Name: "unit", Category: "mechanical", Command: "go test ./...", WorkingDirectory: ".", EnvAllowlist: []string{"PATH"}, TimeoutSeconds: 60, OutputLimitBytes: 1 << 20}
}

func TestAC046_DefinitionValidationAndPinning(t *testing.T) {
	// AC-046: every field rule rejects with a message naming the field.
	if err := ValidateDefinition(valid()); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	empty, blank, nul := "", "  \t", "make\x00"
	setup := "npm ci"
	ok := valid()
	ok.Setup = &setup
	ok.WorkingDirectory = "packages/app"
	ok.OutputLimitBytes = MaxOutputLimit
	if err := ValidateDefinition(ok); err != nil {
		t.Fatalf("valid setup/subdirectory/limit rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*schema.CheckDefinition)
		want   string
	}{
		{"name uppercase", func(d *schema.CheckDefinition) { d.Name = "Unit" }, "name"},
		{"name empty", func(d *schema.CheckDefinition) { d.Name = "" }, "name"},
		{"category", func(d *schema.CheckDefinition) { d.Category = "manual" }, "category"},
		{"empty command", func(d *schema.CheckDefinition) { d.Command = empty }, "command"},
		{"blank command", func(d *schema.CheckDefinition) { d.Command = blank }, "command"},
		{"nul command", func(d *schema.CheckDefinition) { d.Command = nul }, "command"},
		{"empty setup", func(d *schema.CheckDefinition) { d.Setup = &empty }, "setup"},
		{"blank setup", func(d *schema.CheckDefinition) { d.Setup = &blank }, "setup"},
		{"empty dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = "" }, "working_directory"},
		{"absolute dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = "/tmp" }, "working_directory"},
		{"parent dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = ".." }, "working_directory"},
		{"traversal dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = "a/../../b" }, "working_directory"},
		{"dot segment dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = "./a" }, "working_directory"},
		{"trailing slash dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = "a/" }, "working_directory"},
		{"backslash dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = `a\b` }, "working_directory"},
		{"nul dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = "a\x00" }, "working_directory"},
		{"non-NFC dir", func(d *schema.CheckDefinition) { d.WorkingDirectory = "e\u0301" }, "working_directory"},
		{"env with equals", func(d *schema.CheckDefinition) { d.EnvAllowlist = []string{"A=B"} }, "env_allowlist"},
		{"env empty", func(d *schema.CheckDefinition) { d.EnvAllowlist = []string{""} }, "env_allowlist"},
		{"env digit first", func(d *schema.CheckDefinition) { d.EnvAllowlist = []string{"1A"} }, "env_allowlist"},
		{"env duplicate", func(d *schema.CheckDefinition) { d.EnvAllowlist = []string{"PATH", "PATH"} }, "env_allowlist"},
		{"timeout zero", func(d *schema.CheckDefinition) { d.TimeoutSeconds = 0 }, "timeout_seconds"},
		{"timeout too long", func(d *schema.CheckDefinition) { d.TimeoutSeconds = 86401 }, "timeout_seconds"},
		{"negative limit", func(d *schema.CheckDefinition) { d.OutputLimitBytes = -1 }, "output_limit_bytes"},
		{"limit over cap", func(d *schema.CheckDefinition) { d.OutputLimitBytes = MaxOutputLimit + 1 }, "output_limit_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := valid()
			tc.mutate(&d)
			err := ValidateDefinition(d)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error naming %s, got %v", tc.want, err)
			}
		})
	}
	judgment := valid()
	judgment.Category = "judgment"
	if err := ValidateDefinition(judgment); err != nil {
		t.Fatalf("judgment definition is valid to pin: %v", err)
	}
	if Runnable(judgment) == nil || Runnable(valid()) != nil {
		t.Fatal("only judgment definitions are non-runnable")
	}
}
