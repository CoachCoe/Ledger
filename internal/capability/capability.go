// Package capability implements the cooperative Phase 1 path authority model.
package capability

import (
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Normalize rejects ambiguous paths rather than resolving traversal segments.
func Normalize(p string, sensitive bool) (string, error) {
	if !utf8.ValidString(p) || strings.ContainsAny(p, "\\:\x00*?[]{}") {
		return "", apierr.New("invalid_path", "unsupported path %q", p)
	}
	for s := range strings.SplitSeq(p, "/") {
		if s == "" || s == "." || s == ".." {
			return "", apierr.New("invalid_path", "invalid path segment in %q", p)
		}
	}
	p = norm.NFC.String(p)
	if !sensitive {
		p = cases.Fold().String(p)
	}
	return p, nil
}

type Pattern struct {
	Path   string
	Prefix bool
}

func Parse(p string, sensitive bool) (Pattern, error) {
	if p == "**" {
		return Pattern{Prefix: true}, nil
	}
	prefix := strings.HasSuffix(p, "/**")
	if prefix {
		p = strings.TrimSuffix(p, "/**")
	}
	n, err := Normalize(p, sensitive)
	if err != nil {
		return Pattern{}, apierr.New("invalid_pattern", "%v", err)
	}
	return Pattern{Path: n, Prefix: prefix}, nil
}
func (p Pattern) matches(n string) bool {
	return p.Prefix && (p.Path == "" || strings.HasPrefix(n, p.Path+"/")) || !p.Prefix && p.Path == n
}
func (p Pattern) covers(c Pattern) bool {
	if p.Prefix {
		return p.Path == "" || c.Prefix && p.Path == c.Path || strings.HasPrefix(c.Path, p.Path+"/")
	}
	return !c.Prefix && p.Path == c.Path
}
func Validate(c schema.Capability, sensitive bool) error {
	for _, p := range append(append([]string{}, c.Allow...), c.Deny...) {
		if _, err := Parse(p, sensitive); err != nil {
			return err
		}
	}
	start, e := schema.ParseTimestamp(c.CreatedAt)
	if e != nil {
		return e
	}
	end, e := schema.ParseTimestamp(c.ExpiresAt)
	if e != nil {
		return e
	}
	if !end.After(start) {
		return apierr.New("invalid_expiry", "capability must expire after creation")
	}
	return nil
}
func Narrow(parent, child schema.Capability, sensitive bool) error {
	if err := Validate(child, sensitive); err != nil {
		return err
	}
	if err := Validate(parent, sensitive); err != nil {
		return err
	}
	pe, _ := schema.ParseTimestamp(parent.ExpiresAt)
	ce, _ := schema.ParseTimestamp(child.ExpiresAt)
	if ce.After(pe) || child.MayAddDependencies && !parent.MayAddDependencies {
		return apierr.New("capability_widening", "delegation extends expiry or dependency authority")
	}
	for _, c := range child.Allow {
		cp, _ := Parse(c, sensitive)
		covered := false
		for _, p := range parent.Allow {
			pp, _ := Parse(p, sensitive)
			covered = covered || pp.covers(cp)
		}
		if !covered {
			return apierr.New("capability_widening", "uncovered child pattern %q", c)
		}
	}
	return nil
}

// Chain loads leaf-to-root authority, rejecting cycles and malformed delegation.
func Chain(id string, load func(string) (schema.Capability, error), sensitive bool, at time.Time) ([]schema.Capability, error) {
	var out []schema.Capability
	seen := map[string]bool{}
	for {
		if seen[id] || len(out) >= 128 {
			return nil, apierr.New("invalid_capability_chain", "cycle or excessive depth")
		}
		seen[id] = true
		c, err := load(id)
		if err != nil {
			return nil, err
		}
		if c.ID != id {
			return nil, apierr.New("invalid_capability_chain", "capability id differs")
		}
		if err := Validate(c, sensitive); err != nil {
			return nil, err
		}
		start, _ := schema.ParseTimestamp(c.CreatedAt)
		end, _ := schema.ParseTimestamp(c.ExpiresAt)
		if at.Before(start) || !at.Before(end) {
			return nil, apierr.New("capability_expired", "capability %s is not valid at operation time", id)
		}
		if len(out) > 0 {
			if err := Narrow(c, out[len(out)-1], sensitive); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
		if c.Issuer.Kind == "root" {
			return out, nil
		}
		if c.Issuer.Kind != "capability" {
			return nil, apierr.New("invalid_capability_chain", "unknown issuer")
		}
		id = c.Issuer.CapabilityID
	}
}

func Authorize(chain []schema.Capability, p string, sensitive bool, at time.Time) error {
	n, err := Normalize(p, sensitive)
	if err != nil {
		return err
	}
	if len(chain) == 0 {
		return apierr.New("unauthorized", "empty capability chain")
	}
	for _, c := range chain {
		end, err := schema.ParseTimestamp(c.ExpiresAt)
		if err != nil {
			return err
		}
		start, err := schema.ParseTimestamp(c.CreatedAt)
		if err != nil {
			return err
		}
		if at.Before(start) || !at.Before(end) {
			return apierr.New("capability_expired", "capability %s expired", c.ID)
		}
		allowed := false
		for _, a := range c.Allow {
			pat, err := Parse(a, sensitive)
			if err != nil {
				return err
			}
			allowed = allowed || pat.matches(n)
		}
		for _, d := range c.Deny {
			pat, err := Parse(d, sensitive)
			if err != nil {
				return err
			}
			if pat.matches(n) {
				allowed = false
				break
			}
		}
		if !allowed {
			return apierr.New("scope_violation", "path %q is outside capability %s", p, c.ID).With("path", p)
		}
		if !c.MayAddDependencies && Dependency(n) {
			return apierr.New("scope_violation", "dependency file %q is forbidden", p).With("path", p)
		}
	}
	return nil
}

// Dependency conservatively recognizes JS/TS and Python manifests and locks.
func Dependency(p string) bool {
	b := strings.ToLower(path.Base(p))
	switch b {
	case "package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "bun.lockb", "deno.json", "deno.jsonc", "deno.lock", "pyproject.toml", "pipfile", "pipfile.lock", "poetry.lock", "uv.lock", "pdm.lock", "setup.py", "setup.cfg", "requirements.txt", "constraints.txt":
		return true
	}
	return (strings.HasPrefix(b, "requirements") || strings.HasPrefix(b, "constraints")) && strings.HasSuffix(b, ".txt")
}
