package bootstrap

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/johnnycube/stellwerk/internal/config"
)

// Every stellwerk snippet in docs/recipes must be a valid config, with
// rules and expressions that compile.
func TestRecipesAreValid(t *testing.T) {
	files, _ := filepath.Glob("../../docs/recipes/*.md")
	if len(files) < 5 {
		t.Fatalf("recipes not found: %v", files)
	}
	block := regexp.MustCompile("(?s)```yaml\n(.*?)```")
	base := `
targets: { m: { type: matrix, settings: { homeserver: x, user_id: "@x:y", pickle_key: k } } }
destinations:
  critical: { target: m, address: "!a:x" }
  warning: { target: m, address: "!b:x" }
  misc: { target: m, address: "!c:x" }
  media: { target: m, address: "!d:x" }
`
	checked := 0
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		for _, m := range block.FindAllStringSubmatch(string(raw), -1) {
			y := m[1]
			first := strings.TrimSpace(strings.SplitN(y, "\n", 2)[0])
			if !(strings.HasPrefix(first, "# stellwerk") || strings.HasPrefix(first, "sources:") || strings.HasPrefix(first, "heartbeats:")) {
				continue // the app's own config
			}
			doc := base + y
			if !strings.Contains(y, "sources:") {
				doc += "\nsources: { x: { kind: generic, token: t } }\n"
			}
			if !strings.Contains(y, "routes:") {
				doc += "\nroutes: [ { to: misc } ]\n"
			}
			cfg, err := config.Parse([]byte(os.Expand(doc, func(string) string { return "secret" })))
			if err != nil {
				t.Errorf("%s:\n%s\n%v", filepath.Base(f), y, err)
				continue
			}
			if _, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
				t.Errorf("%s:\n%s\n%v", filepath.Base(f), y, err)
			}
			checked++
		}
	}
	if checked < 10 {
		t.Errorf("only %d snippets checked", checked)
	}
}
