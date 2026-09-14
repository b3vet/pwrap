// Package admintokens issues and verifies scoped credentials for pwrapd's
// management API.
//
// PWRAP_BOOTSTRAP_TOKEN is reduced to a root credential that can do exactly one
// thing — mint tokens from this package. Everything else on /v1 requires a token
// carrying the matching scope, so an operator can hand out the ability to run
// migrations without also handing out arbitrary SQL execution.
package admintokens

import (
	"fmt"
	"sort"
	"strings"
)

// Scope is a coarse capability. Four of them, deliberately: fine-grained
// permissions nobody can hold in their head get granted wholesale, which is
// worse than a small set people actually reason about.
type Scope string

const (
	// ScopeProjects covers creating, listing, deleting and branching projects.
	ScopeProjects Scope = "projects"
	// ScopeKeys covers issuing, listing and revoking project API keys.
	ScopeKeys Scope = "keys"
	// ScopeMigrate covers applying tenant migrations and reading their status.
	ScopeMigrate Scope = "migrate"
	// ScopeSQL covers the arbitrary-SQL escape hatch. Separate from the rest
	// because it executes anything as the tenant role: the one capability most
	// worth being able to withhold.
	ScopeSQL Scope = "sql"
)

// All lists every scope, in a stable order for help text and error messages.
var All = []Scope{ScopeProjects, ScopeKeys, ScopeMigrate, ScopeSQL}

// ParseScopes validates a caller-supplied set, rejecting unknown values rather
// than silently dropping them — a typo in a grant should fail loudly, not hand
// out a token that quietly cannot do its job.
func ParseScopes(in []string) ([]Scope, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("at least one scope is required (%s)", JoinAll())
	}
	seen := map[Scope]bool{}
	out := make([]Scope, 0, len(in))
	for _, raw := range in {
		s := Scope(strings.ToLower(strings.TrimSpace(raw)))
		if !valid(s) {
			return nil, fmt.Errorf("unknown scope %q (valid: %s)", raw, JoinAll())
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func valid(s Scope) bool {
	for _, k := range All {
		if k == s {
			return true
		}
	}
	return false
}

// JoinAll renders the valid scopes for error messages.
func JoinAll() string {
	parts := make([]string, len(All))
	for i, s := range All {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// Strings converts for storage and JSON.
func Strings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// Has reports whether the set grants the scope.
func Has(scopes []Scope, want Scope) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
