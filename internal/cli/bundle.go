package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return "" }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parseFlags(name, usage string, args []string, define func(*flag.FlagSet)) error {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	define(fl)
	if err := fl.Parse(args); err != nil || fl.NArg() > 0 {
		return apierr.New("usage", "%s", usage)
	}
	return nil
}

// loadTrust builds the local trust list from --trust values and a file. The
// default file may be absent; a file named with --trust-file must exist. A
// bundle can never add to this list.
func loadTrust(flags []string, file string) (*identity.TrustList, error) {
	var list *identity.TrustList
	if file == "" {
		path, err := identity.DefaultTrustPath()
		if err != nil {
			list = identity.NewTrustList()
		} else if list, err = identity.LoadTrustFile(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			list = identity.NewTrustList()
		}
	} else {
		var err error
		if list, err = identity.LoadTrustFile(file); err != nil {
			return nil, err
		}
	}
	return list, list.Merge(flags)
}

// validated checks input against the closed API schema for op before the
// command acts, exactly as a daemon-served operation would.
func validated(op string, input map[string]any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	return schema.ValidateAPI(op, raw)
}

func cmdExport(args []string) (any, error) {
	const usage = "ledger export --out FILE [--manifest HASH]... [--intent ID]... [--allow-partial]"
	var out string
	var manifests, intents multiFlag
	var partial bool
	if err := parseFlags("export", usage, args, func(fl *flag.FlagSet) {
		fl.StringVar(&out, "out", "", "")
		fl.Var(&manifests, "manifest", "")
		fl.Var(&intents, "intent", "")
		fl.BoolVar(&partial, "allow-partial", false, "")
	}); err != nil {
		return nil, err
	}
	in := map[string]any{"out": out, "manifests": nonNil(manifests), "intents": nonNil(intents), "allow_partial": partial}
	if err := validated("bundle.export", in); err != nil {
		return nil, err
	}
	ctx := context.Background()
	repo, err := locate(ctx)
	if err != nil {
		return nil, err
	}
	opts := verify.ExportOptions{Out: out, Manifests: manifests, Intents: intents, AllowPartial: partial}
	if repo.Native() {
		return verify.ExportNative(repo.Root, opts)
	}
	return verify.Export(ctx, repo.Git, opts)
}

func nonNil(m multiFlag) []string {
	if m == nil {
		return []string{}
	}
	return m
}

func cmdVerify(args []string) (any, error) {
	const usage = "ledger verify [--bundle FILE] [--trust FINGERPRINT]... [--trust-file PATH] [--strict]"
	var bundle, trustFile string
	var trust multiFlag
	var strict bool
	if err := parseFlags("verify", usage, args, func(fl *flag.FlagSet) {
		fl.StringVar(&bundle, "bundle", "", "")
		fl.Var(&trust, "trust", "")
		fl.StringVar(&trustFile, "trust-file", "", "")
		fl.BoolVar(&strict, "strict", false, "")
	}); err != nil {
		return nil, err
	}
	in := map[string]any{"trust": nonNil(trust), "strict": strict}
	if bundle != "" {
		in["bundle"] = bundle
	}
	if trustFile != "" {
		in["trust_file"] = trustFile
	}
	if err := validated("bundle.verify", in); err != nil {
		return nil, err
	}
	list, err := loadTrust(trust, trustFile)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	var src *verify.Source
	if bundle != "" {
		src, err = verify.OpenBundleSource(ctx, bundle, verify.DefaultLimits())
	} else {
		repo, rerr := locate(ctx)
		switch {
		case rerr != nil:
			return nil, rerr
		case repo.Native():
			src, err = verify.OpenNativeSource(repo.Root)
		default:
			src, err = verify.OpenRepoSource(ctx, repo.Git)
		}
	}
	if err != nil {
		return nil, err
	}
	defer src.Close()
	rep, err := verify.Verify(ctx, src, verify.Options{Trusted: list})
	if err != nil {
		return nil, err
	}
	if strict && rep.Integrity != verify.Verified {
		return nil, apierr.New("verification_failed", "integrity is %s", rep.Integrity).With("report", rep)
	}
	return rep, nil
}

// storePublisher publishes through the RecordStore's recovery path.
type storePublisher struct{ s *record.Store }

func (p storePublisher) Publish(ctx context.Context, objs []envelope.Object) (verify.PublishResult, error) {
	r, err := p.s.ImportRecords(ctx, objs)
	if err != nil {
		return verify.PublishResult{}, err
	}
	return verify.PublishResult{Written: r.Imported, Indexed: r.Indexed, Detail: r}, nil
}

func cmdImport(args []string) (result any, err error) {
	const usage = "ledger import FILE [--trust FINGERPRINT]... [--trust-file PATH]"
	var trustFile string
	var trust multiFlag
	var file string
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		file, args = args[0], args[1:]
	}
	if err := parseFlags("import", usage, args, func(fl *flag.FlagSet) {
		fl.Var(&trust, "trust", "")
		fl.StringVar(&trustFile, "trust-file", "", "")
	}); err != nil || file == "" {
		return nil, apierr.New("usage", "%s", usage)
	}
	in := map[string]any{"bundle": file, "trust": nonNil(trust)}
	if trustFile != "" {
		in["trust_file"] = trustFile
	}
	if err := validated("bundle.import", in); err != nil {
		return nil, err
	}
	list, err := loadTrust(trust, trustFile)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	s, _, err := openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := s.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := os.Stat(file); err != nil {
		return nil, apierr.New("bundle_unreadable", "%v", err)
	}
	rep, err := verify.ImportNative(ctx, file, storePublisher{s}, filepath.Dir(s.Dir), verify.ImportOptions{Trusted: list})
	if err != nil {
		return nil, err
	}
	digest, err := s.Index.Digest(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"import": rep, "index_digest": digest}, nil
}
