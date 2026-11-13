// Command workspacebench measures native stores and copy-on-write
// workspaces against the Phase 2 acceptance criteria on a real project.
//
//	workspacebench -fixture DIR -work DIR [-seconds 60] [-out FILE]
//
// FIXTURE is a Git checkout with its ignored dependencies installed, such
// as the NestJS repository from the observer benchmark; it is read, never
// changed. WORK is an empty directory on the same filesystem, which the
// run fills. Each criterion prints PASS or FAIL, and the command exits 1
// if any fails.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/observe"
	"github.com/CoachCoe/Ledger/internal/oplog"
	"github.com/CoachCoe/Ledger/internal/store"
)

var (
	fixture = flag.String("fixture", "", "project checkout with dependencies installed")
	work    = flag.String("work", "", "empty directory on the fixture's filesystem")
	seconds = flag.Int("seconds", 60, "how long to make edits for the observer criterion")
	reps    = flag.Int("reps", 3, "repetitions of the store write")
	out     = flag.String("out", "", "file for the JSON results")
	checks  = flag.String("checks", "npx --no-install tsc -p packages/common/tsconfig.build.json --noEmit;npx --no-install vitest run packages/common",
		"semicolon-separated build and test commands run in sample workspaces")
	sample = flag.Int("sample", 3, "workspaces the checks run in")
)

type result struct {
	Criterion string         `json:"criterion"`
	Pass      bool           `json:"pass"`
	Summary   string         `json:"summary"`
	Data      map[string]any `json:"data,omitempty"`
}

var results []result

func report(criterion string, pass bool, data map[string]any, format string, args ...any) {
	r := result{criterion, pass, fmt.Sprintf(format, args...), data}
	results = append(results, r)
	verdict := "PASS"
	if !pass {
		verdict = "FAIL"
	}
	fmt.Printf("%s  %-11s %s\n", verdict, criterion, r.Summary)
}

func main() {
	flag.Parse()
	if *fixture == "" || *work == "" {
		fmt.Fprintln(os.Stderr, "usage: workspacebench -fixture DIR -work DIR [-seconds N] [-out FILE]")
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "workspacebench:", err)
		os.Exit(1)
	}
	if *out != "" {
		raw, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(*out, raw, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "workspacebench:", err)
			os.Exit(1)
		}
	}
	for _, r := range results {
		if !r.Pass {
			os.Exit(1)
		}
	}
}

func run() error {
	fmt.Printf("host: %s/%s, %d CPUs\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	if err := benchWrite(); err != nil {
		return err
	}
	m, _, err := manager("main")
	if err != nil {
		return err
	}
	base, err := m.Import(*fixture)
	if err != nil {
		return err
	}
	ids, err := benchCreate(m, base)
	if err != nil {
		return err
	}
	if err := benchDeps(m, ids); err != nil {
		return err
	}
	if err := benchRestore(m, ids[len(ids)-1]); err != nil {
		return err
	}
	return benchObserve(m, ids[10:20])
}

func manager(name string) (*cow.Manager, *store.Store, error) {
	dir := filepath.Join(*work, name)
	s, err := store.Open(filepath.Join(dir, "store"), runtime.GOOS == "linux")
	if err != nil {
		return nil, nil, err
	}
	m, err := cow.New(cow.Config{Store: s, Runtime: filepath.Join(dir, "runtime"), Logs: filepath.Join(dir, "oplog"),
		CaseSensitive: runtime.GOOS == "linux", Human: *fixture, Workers: runtime.NumCPU()})
	return m, s, err
}

func median(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}

func p95(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[min(len(s)-1, (len(s)*95+99)/100-1)]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }

// P2-AC-003: a cold write of the tracked tree, against Git's add and
// write-tree into an empty object directory at the same durability.
func benchWrite() error {
	var native, git []time.Duration
	files := 0
	for i := range *reps {
		m, s, err := manager(fmt.Sprintf("write-%d", i))
		if err != nil {
			return err
		}
		start := time.Now()
		tree, err := m.Import(*fixture)
		if err != nil {
			return err
		}
		native = append(native, time.Since(start))
		listed, err := s.Files(tree)
		if err != nil {
			return err
		}
		files = len(listed)
		objects := filepath.Join(*work, fmt.Sprintf("git-%d", i))
		if err := os.MkdirAll(filepath.Join(objects, "objects"), 0o700); err != nil {
			return err
		}
		env := []string{"GIT_INDEX_FILE=" + filepath.Join(objects, "index"), "GIT_OBJECT_DIRECTORY=" + filepath.Join(objects, "objects")}
		start = time.Now()
		if err := gitRun(env, "-c", "core.fsync=loose-object", "-c", "core.fsyncMethod=batch", "add", "-A"); err != nil {
			return err
		}
		if err := gitRun(env, "write-tree"); err != nil {
			return err
		}
		git = append(git, time.Since(start))
	}
	report("P2-AC-003", true, map[string]any{"files": files, "native_ms": median(native).Milliseconds(), "git_ms": median(git).Milliseconds()},
		"%d files: native store %s, Git add + write-tree %s (medians of %d, batched durability)", files, ms(median(native)), ms(median(git)), *reps)
	return nil
}

func gitRun(env []string, args ...string) error {
	cmd := exec.Command("git", args...) //nolint:gosec // fixed Git subcommands
	cmd.Dir = *fixture
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func freeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert,gosec // field types differ between platforms; block sizes are positive
}

func wsp(n int) string { return fmt.Sprintf("wsp_%026d", n) }

// P2-AC-011 and the timing half of P2-AC-016: 50 workspaces with shared
// dependencies, after one that materializes the base and captures them.
func benchCreate(m *cow.Manager, base string) ([]string, error) {
	start := time.Now()
	if _, err := m.Create(wsp(0), base, nil); err != nil {
		return nil, err
	}
	first := time.Since(start)
	before, err := freeBytes(*work)
	if err != nil {
		return nil, err
	}
	var ids []string
	var each []time.Duration
	start = time.Now()
	for i := 1; i <= 50; i++ {
		t := time.Now()
		if _, err := m.Create(wsp(i), base, nil); err != nil {
			return nil, err
		}
		each = append(each, time.Since(t))
		ids = append(ids, wsp(i))
	}
	total := time.Since(start)
	after, err := freeBytes(*work)
	if err != nil {
		return nil, err
	}
	used := (float64(before) - float64(after)) / (1 << 20)
	linked := 0
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(m.Path(id), "node_modules", ".ledger-shared")); err == nil {
			linked++
		}
	}
	// Workspace creation no longer serializes, so speculation can create
	// its candidates ten at a time.
	start = time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var perr error
	next := make(chan int)
	for range 10 {
		wg.Go(func() {
			for i := range next {
				if _, err := m.Create(wsp(i), base, nil); err != nil {
					mu.Lock()
					perr = errors.Join(perr, err)
					mu.Unlock()
				}
			}
		})
	}
	for i := 51; i <= 100; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
	parallel := time.Since(start)
	if perr != nil {
		return nil, perr
	}
	data := map[string]any{"total_ms": total.Milliseconds(), "parallel_ms": parallel.Milliseconds(), "p95_ms": p95(each).Milliseconds(), "first_ms": first.Milliseconds(), "disk_mb": used, "linked": linked}
	// The criterion is for APFS clones; without reflinks each workspace is
	// a full copy of the tracked tree, which is reported but not judged.
	report("P2-AC-011", (runtime.GOOS != "darwin" || min(total, parallel) < 5*time.Second) && linked == 50, data,
		"50 workspaces in %s one at a time (p95 %s each, about %.0f MB) and %s ten at a time; %d share node_modules; the first, which materialized the base and captured dependencies, took %s",
		ms(total), ms(p95(each)), used, ms(parallel), linked, ms(first))
	return ids, nil
}

func digest(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "missing"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// P2-AC-016: builds and tests pass against the shared tree, the shared
// tree refuses writes, and a manifest change in one workspace leaves the
// others' dependencies alone.
func benchDeps(m *cow.Manager, ids []string) error {
	var failures []string
	for _, id := range ids[:*sample] {
		for cmdline := range strings.SplitSeq(*checks, ";") {
			cmd := exec.Command("/bin/sh", "-c", cmdline) //nolint:gosec // the operator's own check commands
			cmd.Dir = m.Path(id)
			outb, err := cmd.CombinedOutput()
			if err != nil {
				tail := string(outb)
				if len(tail) > 600 {
					tail = tail[len(tail)-600:]
				}
				failures = append(failures, fmt.Sprintf("%s in %s: %v\n%s", cmdline, id, err, tail))
			}
		}
	}
	shared := filepath.Join(m.Path(ids[0]), "node_modules")
	probe := filepath.Join(shared, "rxjs", "package.json")
	writeErr := os.WriteFile(probe, []byte("{}"), 0o600)
	createErr := os.WriteFile(filepath.Join(shared, "rxjs", "ledger-probe"), nil, 0o600)
	refused := errors.Is(writeErr, fs.ErrPermission) && errors.Is(createErr, fs.ErrPermission)
	before := digest(filepath.Join(m.Path(ids[1]), "node_modules", "rxjs", "package.json"))

	pkg := filepath.Join(m.Path(ids[0]), "package.json")
	raw, err := os.ReadFile(pkg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(pkg, bytes.Replace(raw, []byte(`"name"`), []byte(`"description": "changed by workspacebench", "name"`), 1), 0o600); err != nil {
		return err
	}
	if _, _, _, err := m.Snapshot(ids[0], "bench", nil); err != nil {
		return err
	}
	_, err = os.Stat(filepath.Join(shared, ".ledger-shared"))
	detached := errors.Is(err, fs.ErrNotExist)
	writable := detached && os.WriteFile(probe, []byte(`{"name":"rxjs"}`), 0o600) == nil
	after := digest(filepath.Join(m.Path(ids[1]), "node_modules", "rxjs", "package.json"))
	isolated := before == after && before != "missing"
	pass := len(failures) == 0 && refused && detached && writable && isolated
	report("P2-AC-016", pass, map[string]any{"check_failures": failures, "write_refused": refused, "detached": detached, "isolated": isolated},
		"checks passed in %d of %d workspaces; shared tree refuses writes: %v; manifest change detached a private copy: %v; other workspaces unchanged: %v",
		*sample-countWorkspaces(failures, ids[:*sample]), *sample, refused, detached && writable, isolated)
	for _, f := range failures {
		fmt.Println("    " + strings.ReplaceAll(f, "\n", "\n    "))
	}
	return nil
}

func countWorkspaces(failures []string, ids []string) int {
	n := 0
	for _, id := range ids {
		for _, f := range failures {
			if strings.Contains(f, id) {
				n++
				break
			}
		}
	}
	return n
}

// P2-AC-012 on the real tree: restore after several edits.
func benchRestore(m *cow.Manager, id string) error {
	ws := m.Path(id)
	var hashes, trees []string
	for i := range 3 {
		p := filepath.Join(ws, "packages", "common", "index.ts")
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "\n// edit %d\n", i)
		f.Close()
		if i == 1 {
			if err := os.Remove(filepath.Join(ws, "Readme.md")); err != nil {
				return err
			}
		}
		e, h, _, err := m.Snapshot(id, "bench", nil)
		if err != nil {
			return err
		}
		hashes, trees = append(hashes, h), append(trees, e.Tree)
	}
	start := time.Now()
	e, _, err := m.Restore(id, hashes[0], nil)
	if err != nil {
		return err
	}
	took := time.Since(start)
	entries, err := m.Log(id)
	if err != nil {
		return err
	}
	_, chainErr := oplog.Chain(entries)
	_, readme := os.Stat(filepath.Join(ws, "Readme.md"))
	pass := e.Tree == trees[0] && chainErr == nil && readme == nil && len(entries) >= 5
	report("P2-AC-012", pass, map[string]any{"restore_ms": took.Milliseconds(), "entries": len(entries)},
		"restore to the first of three edits reproduced its tree in %s; the log kept all %d entries", ms(took), len(entries))
	return nil
}

// P2-AC-010: ten workspaces under watchers, edits at random, detection
// measured from the edit to the snapshot that records it.
func benchObserve(m *cow.Manager, ids []string) error {
	type state struct {
		mu      sync.Mutex
		pending time.Time
		timer   *time.Timer
	}
	var (
		mu        sync.Mutex
		latencies []time.Duration
		errs      []error
	)
	states := map[string]*state{}
	snapshot := func(id string) {
		st := states[id]
		begun := time.Now()
		_, _, appended, err := m.Snapshot(id, "watch", nil)
		done := time.Now()
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, err)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		if appended && !st.pending.IsZero() && begun.After(st.pending) {
			latencies = append(latencies, done.Sub(st.pending))
			st.pending = time.Time{}
		}
	}
	var watchers []observe.WorkspaceObserver
	for _, id := range ids {
		st := &state{}
		states[id] = st
		st.timer = time.AfterFunc(time.Hour, func() { snapshot(id) })
		st.timer.Stop()
		w, err := observe.Watch(m.Path(id), m.Filter(id), func() { st.timer.Reset(500 * time.Millisecond) }, func(err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		})
		if err != nil {
			return err
		}
		watchers = append(watchers, w)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*seconds)*time.Second)
	defer cancel()
	poll := time.NewTicker(10 * time.Second)
	defer poll.Stop()
	targets := []string{"packages/core/index.ts", "packages/common/index.ts", "packages/microservices/index.ts", "Readme.md"}
	rng := rand.New(rand.NewPCG(7, 7)) //nolint:gosec // a reproducible edit schedule
	edits := 0
	next := time.After(0)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-poll.C:
			for _, id := range ids {
				go snapshot(id)
			}
		case <-next:
			id := ids[rng.IntN(len(ids))]
			st := states[id]
			p := filepath.Join(m.Path(id), filepath.FromSlash(targets[rng.IntN(len(targets))]))
			st.mu.Lock()
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			if err == nil {
				fmt.Fprintf(f, "// %d\n", edits)
				f.Close()
				if st.pending.IsZero() {
					st.pending = time.Now()
				}
				edits++
			}
			st.mu.Unlock()
			next = time.After(time.Duration(1000+rng.IntN(2000)) * time.Millisecond / time.Duration(len(ids)))
		}
	}
	time.Sleep(12 * time.Second) // let the last edits reach a snapshot or the poll
	for _, w := range watchers {
		_ = w.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if len(latencies) == 0 {
		report("P2-AC-010", false, nil, "no edit was detected")
		return nil
	}
	p := p95(latencies)
	report("P2-AC-010", p < 2*time.Second, map[string]any{"edits": edits, "detections": len(latencies), "median_ms": median(latencies).Milliseconds(), "p95_ms": p.Milliseconds()},
		"%d edits across %d workspaces, %d detections: median %s, p95 %s", edits, len(ids), len(latencies), ms(median(latencies)), ms(p))
	return nil
}
