// Package index models a tool's index.json. It is pure: no AWS, no I/O beyond
// JSON marshalling, so it is cheap to unit test.
package index

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stephenwilliams/s3-registry/internal/semver"
)

// Artifact is a single downloadable build for one os-arch of one version.
type Artifact struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Version groups the artifacts for one release, keyed by "<os>-<arch>".
type Version struct {
	Version   string              `json:"version"`
	Artifacts map[string]Artifact `json:"artifacts"`
}

// Index is the per-tool manifest stored at <tool>/index.json.
type Index struct {
	Name     string    `json:"name"`
	Updated  time.Time `json:"updated"`
	Versions []Version `json:"versions"`
}

// New returns an empty index for a tool.
func New(name string) *Index {
	return &Index{Name: name, Versions: []Version{}}
}

// Load parses index.json bytes.
func Load(data []byte) (*Index, error) {
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	if idx.Versions == nil {
		idx.Versions = []Version{}
	}
	return &idx, nil
}

// Marshal renders the index as indented JSON.
func (i *Index) Marshal() ([]byte, error) {
	return json.MarshalIndent(i, "", "  ")
}

// findVersion returns the index of a version entry, or -1.
func (i *Index) findVersion(version string) int {
	for n := range i.Versions {
		if i.Versions[n].Version == version {
			return n
		}
	}
	return -1
}

// Upsert sets the artifact for a version+os-arch, creating the version if
// needed. It refreshes Updated and keeps versions sorted ascending.
func (i *Index) Upsert(version, osArch string, art Artifact) {
	n := i.findVersion(version)
	if n < 0 {
		i.Versions = append(i.Versions, Version{
			Version:   version,
			Artifacts: map[string]Artifact{},
		})
		n = len(i.Versions) - 1
	}
	if i.Versions[n].Artifacts == nil {
		i.Versions[n].Artifacts = map[string]Artifact{}
	}
	i.Versions[n].Artifacts[osArch] = art
	i.Updated = time.Now().UTC()
	i.SortVersions()
}

// Remove deletes an artifact. When osArch is empty the whole version is
// dropped; otherwise only that os-arch is removed (and the version is dropped
// if it becomes empty). Returns true if anything was removed.
func (i *Index) Remove(version, osArch string) bool {
	n := i.findVersion(version)
	if n < 0 {
		return false
	}
	removed := false
	if osArch == "" {
		i.Versions = append(i.Versions[:n], i.Versions[n+1:]...)
		removed = true
	} else if _, ok := i.Versions[n].Artifacts[osArch]; ok {
		delete(i.Versions[n].Artifacts, osArch)
		removed = true
		if len(i.Versions[n].Artifacts) == 0 {
			i.Versions = append(i.Versions[:n], i.Versions[n+1:]...)
		}
	}
	if removed {
		i.Updated = time.Now().UTC()
	}
	return removed
}

// SortVersions orders versions ascending by semver. Duplicate version strings
// (possible in a hand-edited index) are merged, artifacts combined last-writer-
// wins per os-arch; unparseable versions are kept in first-appearance order at
// the end.
func (i *Index) SortVersions() {
	merged := make(map[string]*Version, len(i.Versions))
	var seen []string // first-appearance order
	for _, v := range i.Versions {
		m, ok := merged[v.Version]
		if !ok {
			nv := Version{Version: v.Version, Artifacts: map[string]Artifact{}}
			merged[v.Version] = &nv
			seen = append(seen, v.Version)
			m = &nv
		}
		for oa, a := range v.Artifacts {
			m.Artifacts[oa] = a
		}
	}

	sortedParseable := semver.SortAscending(seen) // unique in, unparseable dropped
	inSorted := make(map[string]bool, len(sortedParseable))
	for _, vs := range sortedParseable {
		inSorted[vs] = true
	}

	out := make([]Version, 0, len(merged))
	for _, vs := range sortedParseable {
		out = append(out, *merged[vs])
	}
	for _, vs := range seen {
		if !inSorted[vs] {
			out = append(out, *merged[vs])
		}
	}
	i.Versions = out
}

func (i *Index) versionStrings() []string {
	out := make([]string, len(i.Versions))
	for n, v := range i.Versions {
		out[n] = v.Version
	}
	return out
}

// VersionStrings returns the version list in stored order.
func (i *Index) VersionStrings() []string {
	return i.versionStrings()
}

// ResolveVersion resolves a constraint to a concrete version and returns that
// version plus the artifact for the requested os-arch.
func (i *Index) ResolveVersion(constraint, osArch string) (Version, Artifact, error) {
	resolved, err := semver.Resolve(i.versionStrings(), constraint)
	if err != nil {
		return Version{}, Artifact{}, err
	}
	n := i.findVersion(resolved)
	if n < 0 {
		return Version{}, Artifact{}, fmt.Errorf("version %q not found", resolved)
	}
	ver := i.Versions[n]
	art, ok := ver.Artifacts[osArch]
	if !ok {
		return ver, Artifact{}, fmt.Errorf("no artifact for %s at version %s", osArch, resolved)
	}
	return ver, art, nil
}
