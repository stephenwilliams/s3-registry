// Package semver wraps Masterminds/semver with the small subset the registry needs.
package semver

import (
	"fmt"
	"sort"

	msemver "github.com/Masterminds/semver/v3"
)

// SortAscending returns the input sorted ascending by semantic version.
// Unparseable entries are dropped.
func SortAscending(versions []string) []string {
	parsed := make([]*msemver.Version, 0, len(versions))
	for _, v := range versions {
		if pv, err := msemver.NewVersion(v); err == nil {
			parsed = append(parsed, pv)
		}
	}
	sort.Sort(msemver.Collection(parsed))
	out := make([]string, len(parsed))
	for i, pv := range parsed {
		out[i] = pv.Original()
	}
	return out
}

// Resolve picks the highest version satisfying the constraint. An empty
// constraint or "latest" selects the highest available version.
func Resolve(versions []string, constraint string) (string, error) {
	if len(versions) == 0 {
		return "", fmt.Errorf("no versions available")
	}
	sorted := SortAscending(versions)
	if len(sorted) == 0 {
		return "", fmt.Errorf("no parseable versions available")
	}

	if constraint == "" || constraint == "latest" {
		return sorted[len(sorted)-1], nil
	}

	c, err := msemver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("invalid constraint %q: %w", constraint, err)
	}

	// Walk descending so the first match is the highest satisfying version.
	for i := len(sorted) - 1; i >= 0; i-- {
		pv, err := msemver.NewVersion(sorted[i])
		if err != nil {
			continue
		}
		if c.Check(pv) {
			return sorted[i], nil
		}
	}
	return "", fmt.Errorf("no version satisfies constraint %q", constraint)
}
