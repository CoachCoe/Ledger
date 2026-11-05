package cli

import (
	"os"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"ledger": func() { os.Exit(Main(os.Args[1:], os.Stdout)) },
	})
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Setup: func(env *testscript.Env) error {
			env.Setenv("XDG_CONFIG_HOME", env.WorkDir+"/config")
			env.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			env.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			return nil
		},
	})
}
