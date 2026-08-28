package buildinfo

// These are set at link time via -ldflags -X. Defaults keep dev builds sane.
var (
	Version = "dev"
	Commit  = "unknown"
	BuiltAt = "unknown"
	GitTag  = "unknown"
	Service = "s3reg"
)

// Info is a snapshot of the build metadata.
type Info struct {
	Service       string  `json:"service"`
	Version       string  `json:"version"`
	Commit        string  `json:"commit"`
	BuiltAt       string  `json:"built_at"`
	GitTag        string  `json:"git_tag"`
	Dirty         bool    `json:"dirty"`
	SourceRepoURL *string `json:"source_repo_url"`
}

// Get returns the current build metadata.
func Get() Info {
	return Info{
		Service:       Service,
		Version:       Version,
		Commit:        Commit,
		BuiltAt:       BuiltAt,
		GitTag:        GitTag,
		Dirty:         false,
		SourceRepoURL: nil,
	}
}
