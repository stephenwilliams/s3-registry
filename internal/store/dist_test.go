package store

import (
	"strings"
	"testing"
)

func TestInferOSArch(t *testing.T) {
	cases := []struct {
		file     string
		os, arch string
		ok       bool
	}{
		{"mytool_1.2.3_Darwin_arm64.tar.gz", "darwin", "arm64", true},
		{"mytool_1.2.3_Linux_x86_64.tar.gz", "linux", "amd64", true},
		{"mytool-linux-aarch64.zip", "linux", "arm64", true},
		{"mytool_Windows_amd64.zip", "windows", "amd64", true},
		{"mytool-macos-arm64.tgz", "darwin", "arm64", true},
		{"mytool_1.2.3_amd64.tar.gz", "", "", false}, // no os token
		{"mytool_1.2.3_Linux.tar.gz", "", "", false}, // no arch token
		{"README.md", "", "", false},
		// Tool names embedding an os token must not misclassify: whole-token
		// matching ignores "mac" inside "macaroni" and "win" inside "winter".
		{"macaroni_1.0.0_linux_amd64.tar.gz", "linux", "amd64", true},
		{"winter_1.0.0_darwin_arm64.tgz", "darwin", "arm64", true},
		{"macaroni_1.0.0_amd64.tar.gz", "", "", false}, // "mac" is not an os here
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			os, arch, ok := InferOSArch(tc.file)
			if ok != tc.ok || os != tc.os || arch != tc.arch {
				t.Fatalf("InferOSArch(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.file, os, arch, ok, tc.os, tc.arch, tc.ok)
			}
		})
	}
}

func TestIsArchive(t *testing.T) {
	yes := []string{"x.tar.gz", "x.tgz", "x.zip", "x.tar.xz", "X.TAR.GZ"}
	no := []string{"checksums.txt", "x.md", "x.sha256"}
	for _, f := range yes {
		if !IsArchive(f) {
			t.Errorf("IsArchive(%q) = false, want true", f)
		}
	}
	for _, f := range no {
		if IsArchive(f) {
			t.Errorf("IsArchive(%q) = true, want false", f)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	in := `# checksums
abc123  mytool_Darwin_arm64.tar.gz
def456 *dist/mytool_Linux_amd64.tar.gz

DEF789  mytool_Windows_amd64.zip
`
	got, err := ParseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{
		"mytool_Darwin_arm64.tar.gz": "abc123",
		"mytool_Linux_amd64.tar.gz":  "def456",
		"mytool_Windows_amd64.zip":   "def789",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("checksum[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseChecksumsMalformed(t *testing.T) {
	if _, err := ParseChecksums(strings.NewReader("onlyonefield\n")); err == nil {
		t.Fatal("expected error on malformed line")
	}
}
