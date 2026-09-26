package provision_test

import (
	"regexp"
	"testing"

	"github.com/sinmetalcraft/champions/internal/provision"
)

// projectIDRe は Google Cloud Project ID として許される形式。
var projectIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

func TestNewProjectID(t *testing.T) {
	for _, code := range []string{"a1", "handson", "abcdefghijklmnopqrstuvwxy"} {
		id, err := provision.NewProjectID(code)
		if err != nil {
			t.Fatalf("NewProjectID(%q) = %v", code, err)
		}
		if len(id) < 6 || len(id) > 30 {
			t.Errorf("NewProjectID(%q) = %q, length %d is out of [6, 30]", code, id, len(id))
		}
		if !projectIDRe.MatchString(id) {
			t.Errorf("NewProjectID(%q) = %q, which is not a valid project id", code, id)
		}
		if want := code + "-"; id[:len(want)] != want {
			t.Errorf("NewProjectID(%q) = %q, want prefix %q", code, id, want)
		}
	}
}

func TestNewProjectIDIsRandom(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := provision.NewProjectID("handson")
		if err != nil {
			t.Fatal(err)
		}
		seen[id] = true
	}
	// 33^4 通りあるので 100 回引いて全部同じになることはまずない。
	if len(seen) < 90 {
		t.Errorf("NewProjectID() generated only %d unique ids out of 100", len(seen))
	}
}
