package store

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// archiveExts are the archive suffixes recognized when walking a dist dir.
var archiveExts = []string{".tar.gz", ".tgz", ".tar.xz", ".tar.bz2", ".zip", ".tar"}

// IsArchive reports whether a filename looks like a release archive.
func IsArchive(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range archiveExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// InferOSArch derives a GOOS-GOARCH pair from a goreleaser-style filename such
// as "mytool_1.2.3_Darwin_arm64.tar.gz" or "mytool-linux-x86_64.zip". It
// returns ok=false when either component cannot be identified.
func InferOSArch(filename string) (goos, goarch string, ok bool) {
	lower := strings.ToLower(filename)

	osTokens := map[string]string{
		"darwin":  "darwin",
		"macos":   "darwin",
		"mac":     "darwin",
		"linux":   "linux",
		"windows": "windows",
		"win":     "windows",
	}
	archTokens := map[string]string{
		"amd64":   "amd64",
		"x86_64":  "amd64",
		"x8664":   "amd64",
		"arm64":   "arm64",
		"aarch64": "arm64",
	}

	goos = firstToken(lower, osTokens)
	goarch = firstToken(lower, archTokens)
	if goos == "" || goarch == "" {
		return "", "", false
	}
	return goos, goarch, true
}

// firstToken returns the mapped value for the earliest whole-token match in s.
// A token matches only when it is bounded by a delimiter (`_`, `-`, `.`) or the
// string edges, so short aliases like "mac" or "win" cannot match inside a tool
// name or version. The alias itself may contain delimiters (e.g. "x86_64").
func firstToken(s string, tokens map[string]string) string {
	bestIdx := -1
	best := ""
	for tok, val := range tokens {
		if i := boundedIndex(s, tok); i >= 0 {
			if bestIdx == -1 || i < bestIdx {
				bestIdx = i
				best = val
			}
		}
	}
	return best
}

func isDelim(b byte) bool { return b == '_' || b == '-' || b == '.' }

// boundedIndex returns the earliest index at which tok appears in s bounded by
// delimiters or the string edges, or -1.
func boundedIndex(s, tok string) int {
	from := 0
	for {
		rel := strings.Index(s[from:], tok)
		if rel < 0 {
			return -1
		}
		i := from + rel
		end := i + len(tok)
		leftOK := i == 0 || isDelim(s[i-1])
		rightOK := end == len(s) || isDelim(s[end])
		if leftOK && rightOK {
			return i
		}
		from = i + 1
	}
}

// ParseChecksums reads a checksums.txt whose lines are "<hex>  <filename>" and
// returns a map of base filename to lowercase hex hash. Blank lines and lines
// beginning with '#' are ignored.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed checksum line: %q", line)
		}
		hash := strings.ToLower(fields[0])
		// The filename may itself contain spaces; join everything after the
		// hash and strip a leading '*' binary marker.
		name := strings.TrimPrefix(strings.Join(fields[1:], " "), "*")
		out[baseName(name)] = hash
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// baseName returns the final path element, handling both slash styles.
func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
