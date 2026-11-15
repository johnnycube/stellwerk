package http

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// The UI's script has no build step, so a syntax error would only show in
// the browser. node --check catches it here (skipped without node).
func TestUIScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(uiHTML)
	if m == nil {
		t.Fatal("no script in the UI")
	}
	f := filepath.Join(t.TempDir(), "ui.js")
	_ = os.WriteFile(f, m[1], 0o600)
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("UI script does not parse:\n%s", out)
	}
}
