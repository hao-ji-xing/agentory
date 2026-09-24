// Package source holds the registry of conversation-history providers.
//
// Adding a provider means writing a package that implements model.Source
// and appending its constructor to builtin below.
package source

import (
	"fmt"
	"sort"

	"github.com/hao-ji-xing/agentory/internal/model"
	"github.com/hao-ji-xing/agentory/internal/source/claudecode"
	"github.com/hao-ji-xing/agentory/internal/source/codex"
)

// builtin lists the constructors of every shipped source.
var builtin = []func() model.Source{
	func() model.Source { return claudecode.New() },
	func() model.Source { return codex.New() },
}

// All returns a fresh instance of every registered source.
func All() []model.Source {
	out := make([]model.Source, 0, len(builtin))
	for _, f := range builtin {
		out = append(out, f())
	}
	return out
}

// Names returns the registered source names, sorted.
func Names() []string {
	var names []string
	for _, s := range All() {
		names = append(names, s.Name())
	}
	sort.Strings(names)
	return names
}

// Select returns the sources whose names are listed, or all of them when
// names is empty.
func Select(names []string) ([]model.Source, error) {
	all := All()
	if len(names) == 0 {
		return all, nil
	}
	var out []model.Source
	for _, n := range names {
		found := false
		for _, s := range all {
			if s.Name() == n {
				out = append(out, s)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown source %q (available: %v)", n, Names())
		}
	}
	return out, nil
}
