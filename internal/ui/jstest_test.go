package ui_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestJavaScriptUnitTests runs the console's pure-module tests with node.
// Without node it skips, unless EACP_UI_NODE_REQUIRED=1 makes it fail.
func TestJavaScriptUnitTests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("EACP_UI_NODE_REQUIRED") == "1" {
			t.Fatal("node is required (EACP_UI_NODE_REQUIRED=1) but not on PATH")
		}
		t.Skip("node is not on PATH: the console's JavaScript tests did not run (EACP_UI_NODE_REQUIRED=1 fails instead)")
	}
	files, err := filepath.Glob(filepath.Join("jstest", "*.test.mjs"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no JavaScript tests found: %v", err)
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
