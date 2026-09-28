package bench

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWriteResultsRefusesASecret(t *testing.T) {
	dir := t.TempDir()
	r := Results{Schema: SchemaVersion, PDP: "local", StartedAt: time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC),
		Flags: map[string]string{"x": "eacp_sk_abc"}}
	leaky := filepath.Join(dir, "leaky.json")
	if err := WriteResults(leaky, r, []string{"", "eacp_sk_abc"}); err == nil {
		t.Fatal("a results file holding a key was written")
	}
	if _, err := os.Stat(leaky); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused file exists: %v", err)
	}
	clean := filepath.Join(dir, "sub", "clean.json")
	if err := WriteResults(clean, r, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	var back Results
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, r) {
		t.Fatalf("round trip = %+v %v, want %+v", back, err, r)
	}
}
