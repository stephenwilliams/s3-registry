package index

import (
	"reflect"
	"testing"
)

func art(key string) Artifact {
	return Artifact{Key: key, SHA256: "deadbeef", Size: 100}
}

func TestUpsertAndSort(t *testing.T) {
	idx := New("mytool")
	idx.Upsert("1.2.0", "darwin-arm64", art("mytool/1.2.0/darwin-arm64/mytool.tar.gz"))
	idx.Upsert("1.0.0", "linux-amd64", art("mytool/1.0.0/linux-amd64/mytool.tar.gz"))
	idx.Upsert("1.10.0", "linux-amd64", art("mytool/1.10.0/linux-amd64/mytool.tar.gz"))

	want := []string{"1.0.0", "1.2.0", "1.10.0"}
	if got := idx.VersionStrings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}

	// Upsert into an existing version adds an os-arch without duplicating.
	idx.Upsert("1.2.0", "linux-amd64", art("mytool/1.2.0/linux-amd64/mytool.tar.gz"))
	if len(idx.Versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(idx.Versions))
	}
	n := idx.findVersion("1.2.0")
	if len(idx.Versions[n].Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts for 1.2.0, got %d", len(idx.Versions[n].Artifacts))
	}
}

func TestRemove(t *testing.T) {
	idx := New("mytool")
	idx.Upsert("1.0.0", "darwin-arm64", art("a"))
	idx.Upsert("1.0.0", "linux-amd64", art("b"))
	idx.Upsert("2.0.0", "linux-amd64", art("c"))

	if !idx.Remove("1.0.0", "darwin-arm64") {
		t.Fatal("expected removal to report true")
	}
	n := idx.findVersion("1.0.0")
	if n < 0 || len(idx.Versions[n].Artifacts) != 1 {
		t.Fatalf("expected 1 artifact left on 1.0.0")
	}

	// Removing the last os-arch drops the version.
	idx.Remove("1.0.0", "linux-amd64")
	if idx.findVersion("1.0.0") >= 0 {
		t.Fatal("expected 1.0.0 to be pruned")
	}

	// Removing a whole version.
	if !idx.Remove("2.0.0", "") {
		t.Fatal("expected whole-version removal")
	}
	if len(idx.Versions) != 0 {
		t.Fatalf("expected empty index, got %d versions", len(idx.Versions))
	}

	if idx.Remove("9.9.9", "") {
		t.Fatal("removing missing version should return false")
	}
}

func TestResolveVersion(t *testing.T) {
	idx := New("mytool")
	idx.Upsert("1.0.0", "linux-amd64", art("mytool/1.0.0/linux-amd64/x"))
	idx.Upsert("1.2.0", "linux-amd64", art("mytool/1.2.0/linux-amd64/x"))
	idx.Upsert("1.2.0", "darwin-arm64", art("mytool/1.2.0/darwin-arm64/x"))

	ver, a, err := idx.ResolveVersion("^1.0", "linux-amd64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ver.Version != "1.2.0" || a.Key != "mytool/1.2.0/linux-amd64/x" {
		t.Fatalf("got version %s key %s", ver.Version, a.Key)
	}

	if _, _, err := idx.ResolveVersion("latest", "windows-amd64"); err == nil {
		t.Fatal("expected error for missing os-arch")
	}

	if _, _, err := idx.ResolveVersion(">=9.0.0", "linux-amd64"); err == nil {
		t.Fatal("expected error for unsatisfiable constraint")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	idx := New("mytool")
	idx.Upsert("1.2.3", "darwin-arm64", Artifact{Key: "mytool/1.2.3/darwin-arm64/mytool.tar.gz", SHA256: "abc123", Size: 12345})

	data, err := idx.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	loaded, err := Load(data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Name != "mytool" {
		t.Fatalf("name = %q", loaded.Name)
	}
	got, a, err := loaded.ResolveVersion("1.2.3", "darwin-arm64")
	if err != nil {
		t.Fatalf("resolve after round trip: %v", err)
	}
	if got.Version != "1.2.3" || a.Size != 12345 || a.SHA256 != "abc123" {
		t.Fatalf("round trip lost data: %+v %+v", got, a)
	}
}

func TestSortVersionsMergesDuplicates(t *testing.T) {
	// Simulate a hand-edited index.json with two entries for the same version.
	idx := &Index{
		Name: "mytool",
		Versions: []Version{
			{Version: "1.0.0", Artifacts: map[string]Artifact{
				"linux-amd64":  art("mytool/1.0.0/linux-amd64/x"),
				"darwin-arm64": art("old"),
			}},
			{Version: "1.0.0", Artifacts: map[string]Artifact{
				"darwin-arm64":  {Key: "new", SHA256: "feed", Size: 200}, // last-writer-wins
				"windows-amd64": art("mytool/1.0.0/windows-amd64/x"),
			}},
			{Version: "2.0.0", Artifacts: map[string]Artifact{
				"linux-amd64": art("mytool/2.0.0/linux-amd64/x"),
			}},
		},
	}
	idx.SortVersions()

	if got := idx.VersionStrings(); !reflect.DeepEqual(got, []string{"1.0.0", "2.0.0"}) {
		t.Fatalf("versions = %v, want [1.0.0 2.0.0]", got)
	}
	n := idx.findVersion("1.0.0")
	arts := idx.Versions[n].Artifacts
	if len(arts) != 3 {
		t.Fatalf("expected 3 merged artifacts, got %d: %v", len(arts), arts)
	}
	if arts["darwin-arm64"].Key != "new" || arts["darwin-arm64"].Size != 200 {
		t.Fatalf("last-writer-wins failed for darwin-arm64: %+v", arts["darwin-arm64"])
	}
	if _, ok := arts["windows-amd64"]; !ok {
		t.Fatal("expected windows-amd64 to survive the merge")
	}
}

func TestLoadEmptyVersions(t *testing.T) {
	idx, err := Load([]byte(`{"name":"x","updated":"2026-08-27T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if idx.Versions == nil {
		t.Fatal("expected non-nil versions slice")
	}
}
