package semver

import (
	"reflect"
	"testing"
)

func TestSortAscending(t *testing.T) {
	in := []string{"1.2.0", "1.0.0", "2.0.0", "1.10.0", "1.2.3", "not-a-version"}
	got := SortAscending(in)
	want := []string{"1.0.0", "1.2.0", "1.2.3", "1.10.0", "2.0.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SortAscending = %v, want %v", got, want)
	}
}

func TestResolve(t *testing.T) {
	versions := []string{"1.0.0", "1.1.0", "1.2.0", "1.2.9", "2.0.0", "2.1.0"}
	cases := []struct {
		name       string
		constraint string
		want       string
		wantErr    bool
	}{
		{"latest keyword", "latest", "2.1.0", false},
		{"empty means highest", "", "2.1.0", false},
		{"caret 1.2", "^1.2", "1.2.9", false},
		{"caret 1", "^1", "1.2.9", false},
		{"tilde 1.0", "~1.0", "1.0.0", false},
		{"range", ">=1.0.0 <2.0.0", "1.2.9", false},
		{"exact", "1.1.0", "1.1.0", false},
		{"exact missing", "1.5.0", "", true},
		{"no match", ">=3.0.0", "", true},
		{"bad constraint", "not-valid!!", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(versions, tc.constraint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tc.constraint, got, tc.want)
			}
		})
	}
}

func TestResolveEmptyInput(t *testing.T) {
	if _, err := Resolve(nil, "latest"); err == nil {
		t.Fatal("expected error for empty version list")
	}
}
