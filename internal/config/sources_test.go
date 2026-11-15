package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A small but complete config, split the way it would be in Kubernetes:
// rules in a file or ConfigMap, secrets elsewhere.
const rulesYAML = `
sources:
  alertmanager: { kind: alertmanager, token: "${AM_TOKEN}" }
  k8s-events: { kind: generic, token: placeholder }
targets:
  matrix:
    type: matrix
    settings: { homeserver: https://hs.example.org }
    accounts:
      alerts: { user_id: "@stellwerk:example.org" }
destinations:
  critical: { target: matrix, account: alerts, address: "!crit:example.org" }
routes:
  - { name: all, to: critical }
`

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileOnlyWithExpansion(t *testing.T) {
	dir := t.TempDir()
	cfg, warnings, err := LoadFrom(LoadOptions{File: write(t, dir, "s.yaml", rulesYAML), Environ: []string{"AM_TOKEN=from-env"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources["alertmanager"].Token != "from-env" || len(warnings) != 0 {
		t.Errorf("token %q warnings %v", cfg.Sources["alertmanager"].Token, warnings)
	}
}

func TestEnvOverridesEverything(t *testing.T) {
	dir := t.TempDir()
	cfg, warnings, err := LoadFrom(LoadOptions{File: write(t, dir, "s.yaml", rulesYAML), Environ: []string{
		"AM_TOKEN=from-file-expansion",
		"STELLWERK_SOURCES__ALERTMANAGER__TOKEN=from-env-override",
		"STELLWERK_SOURCES__K8S_EVENTS__TOKEN=dash-key-found", // _ matches - in k8s-events
		"STELLWERK_TARGETS__MATRIX__ACCOUNTS__ALERTS__PASSWORD=12345",
		"STELLWERK_TARGETS__MATRIX__ACCOUNTS__ALERTS__PICKLE_KEY=null", // stays text
		"STELLWERK_DELIVERY__MODE=async",
		"STELLWERK_DELIVERY__TIMEOUT=45s",
		"STELLWERK_DELIVERY__QUEUE_SIZE=7",
		"STELLWERK_NODE_ID=node-7",
		"STELLWERK_HA__ENABLED=true",
		"STELLWERK_HA__SECRET=s",
		`STELLWERK_HA__PEERS=[{id: node-7, url: "http://a"}, {id: b, url: "http://b"}]`,
		"STELLWERK_HA__PEERS__1__URL=http://b-override",
		`STELLWERK_TARGETS__MATRIX__SETTINGS__ALLOWED_INVITERS=["@me:example.org", "@you:example.org"]`,
		"STELLWERK_LOG__FORMAT=json",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	acc := cfg.Targets["matrix"].Accounts["alerts"]
	var a struct {
		UserID    string `yaml:"user_id"`
		Password  string `yaml:"password"`
		PickleKey string `yaml:"pickle_key"`
	}
	if err := acc.Decode(&a); err != nil {
		t.Fatal(err)
	}
	var shared struct {
		Homeserver string   `yaml:"homeserver"`
		Inviters   []string `yaml:"allowed_inviters"`
	}
	settings := cfg.Targets["matrix"].Settings
	_ = settings.Decode(&shared)
	checks := map[string]bool{
		"override":       cfg.Sources["alertmanager"].Token == "from-env-override",
		"dash key":       cfg.Sources["k8s-events"].Token == "dash-key-found",
		"merged account": a.UserID == "@stellwerk:example.org" && a.Password == "12345" && a.PickleKey == "null",
		"shared kept":    shared.Homeserver == "https://hs.example.org" && len(shared.Inviters) == 2,
		"typed values":   cfg.Delivery.Mode == "async" && cfg.Delivery.Timeout == 45*time.Second && cfg.Delivery.QueueSize == 7,
		"node":           cfg.NodeID == "node-7",
		"ha list":        cfg.HA.Enabled && len(cfg.HA.Peers) == 2 && cfg.HA.Peers[1].URL == "http://b-override" && cfg.HA.Peers[1].ID == "b",
		"log":            cfg.Log.Format == "json" && cfg.Log.Level == "info",
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s failed: %+v", name, cfg)
		}
	}
}

func TestConfigOnlyFromEnvWithoutFile(t *testing.T) {
	cfg, _, err := LoadFrom(LoadOptions{File: filepath.Join(t.TempDir(), "missing.yaml"), Environ: []string{
		"STELLWERK_SOURCES__AM__KIND=alertmanager",
		"STELLWERK_SOURCES__AM__TOKEN=t",
		"STELLWERK_TARGETS__HOOK__TYPE=webhook",
		"STELLWERK_TARGETS__HOOK__SETTINGS__URL=http://x",
		"STELLWERK_DESTINATIONS__PAGER__TARGET=hook",
		"STELLWERK_ROUTES__0__TO=pager",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].To[0] != "pager" || cfg.Destinations["pager"].Account != "default" {
		t.Errorf("%+v", cfg)
	}
}

func TestRequiredFileMissing(t *testing.T) {
	if _, _, err := LoadFrom(LoadOptions{File: "/nope/stellwerk.yaml", FileRequired: true}); err == nil {
		t.Error("an explicitly given file must exist")
	}
}

// Simulates a Kubernetes Secret volume: values as files, symlinks through ..data.
func TestKubernetesSecretVolume(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "..2026_11_15_12_00_00.123")
	write(t, data, "targets.matrix.accounts.alerts.password", "s3cret\n")
	write(t, data, "sources__alertmanager__token", "am-token")
	write(t, data, "stellwerk.yaml", "routes:\n  - { name: from-configmap, to: critical }\n")
	write(t, data, "README", "not a key")
	if err := os.Symlink(filepath.Base(data), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"targets.matrix.accounts.alerts.password", "sources__alertmanager__token", "stellwerk.yaml", "README"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	file := write(t, t.TempDir(), "s.yaml", rulesYAML)
	cfg, warnings, err := LoadFrom(LoadOptions{File: file, Dirs: []string{dir}, Environ: []string{"AM_TOKEN=x"}})
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Password string `yaml:"password"`
	}
	acc := cfg.Targets["matrix"].Accounts["alerts"]
	_ = acc.Decode(&a)
	if a.Password != "s3cret" {
		t.Errorf("password %q (trailing newline must be trimmed)", a.Password)
	}
	if cfg.Sources["alertmanager"].Token != "am-token" {
		t.Errorf("token %q", cfg.Sources["alertmanager"].Token)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Name != "from-configmap" {
		t.Errorf("a YAML fragment replaces lists: %+v", cfg.Routes)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "README") {
		t.Errorf("warnings %v", warnings)
	}
}

// The hierarchy: config file < YAML fragment < env < env _FILE < secret file.
func TestPrecedence(t *testing.T) {
	secrets, configmap := t.TempDir(), t.TempDir()
	write(t, configmap, "extra.yaml", "delivery: { max_retries: 7, timeout: 11s, mode: sync }\n")
	write(t, secrets, "delivery.queue_size", "99")
	pwFile := write(t, t.TempDir(), "pw", "from-env-file")
	file := write(t, t.TempDir(), "s.yaml", rulesYAML+"delivery: { mode: sync, timeout: 10s, queue_size: 3, max_retries: 2, max_backoff: 5s }\n")
	cfg, _, err := LoadFrom(LoadOptions{File: file, Dirs: []string{configmap, secrets}, Environ: []string{
		"AM_TOKEN=x",
		"STELLWERK_DELIVERY__TIMEOUT=50s",  // env beats file and fragment
		"STELLWERK_DELIVERY__QUEUE_SIZE=5", // ...but the secret file beats env
		"STELLWERK_SOURCES__ALERTMANAGER__TOKEN=plain",
		"STELLWERK_SOURCES__ALERTMANAGER__TOKEN_FILE=" + pwFile, // _FILE beats the plain variable
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Delivery
	checks := map[string]bool{
		"file only":             d.MaxBackoff == 5*time.Second,
		"fragment beats file":   d.MaxRetries == 7,
		"env beats fragment":    d.Timeout == 50*time.Second,
		"secret file beats env": d.QueueSize == 99,
		"_FILE beats plain":     cfg.Sources["alertmanager"].Token == "from-env-file",
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s: %+v token=%q", name, d, cfg.Sources["alertmanager"].Token)
		}
	}
	// a secret file also beats a _FILE variable
	write(t, secrets, "sources.alertmanager.token", "from-secret-volume")
	cfg, _, _ = LoadFrom(LoadOptions{File: file, Dirs: []string{secrets}, Environ: []string{"AM_TOKEN=x", "STELLWERK_SOURCES__ALERTMANAGER__TOKEN_FILE=" + pwFile}})
	if cfg.Sources["alertmanager"].Token != "from-secret-volume" {
		t.Errorf("secret volume must win: %q", cfg.Sources["alertmanager"].Token)
	}
}

func TestFileSuffixReadsSecretFile(t *testing.T) {
	secret := write(t, t.TempDir(), "pw", "from-file\n")
	cfg, _, err := LoadFrom(LoadOptions{File: write(t, t.TempDir(), "s.yaml", rulesYAML), Environ: []string{
		"AM_TOKEN=x", "STELLWERK_SOURCES__ALERTMANAGER__TOKEN_FILE=" + secret,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources["alertmanager"].Token != "from-file" {
		t.Errorf("token %q", cfg.Sources["alertmanager"].Token)
	}
	_, _, err = LoadFrom(LoadOptions{File: write(t, t.TempDir(), "s.yaml", rulesYAML), Environ: []string{
		"AM_TOKEN=x", "STELLWERK_SOURCES__ALERTMANAGER__TOKEN_FILE=/does/not/exist",
	}})
	if err == nil {
		t.Error("a missing _FILE must fail loudly")
	}
}

func TestUnknownVariablesAndServiceLinks(t *testing.T) {
	_, warnings, err := LoadFrom(LoadOptions{File: write(t, t.TempDir(), "s.yaml", rulesYAML), Environ: []string{
		"AM_TOKEN=x",
		"STELLWERK_PORT=tcp://10.0.0.1:8080", // Kubernetes service links: silent
		"STELLWERK_SERVICE_HOST=10.0.0.1",
		"STELLWERK_PORT_8080_TCP_ADDR=10.0.0.1",
		"STELLWERK_DELIVERY_MODE=async",                  // single underscore: warn with a hint
		"STELLWERK_CONFIG=/x", "STELLWERK_CONFIG_DIR=/y", // reserved: silent
		"OTHER=1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "STELLWERK_DELIVERY__MODE") {
		t.Errorf("warnings %v", warnings)
	}
}

func TestTyposInsideKnownSectionsFail(t *testing.T) {
	_, _, err := LoadFrom(LoadOptions{File: write(t, t.TempDir(), "s.yaml", rulesYAML), Environ: []string{"AM_TOKEN=x", "STELLWERK_DELIVERY__MDOE=async"}})
	if err == nil || !strings.Contains(err.Error(), "mdoe") {
		t.Errorf("want strict error naming the key, got %v", err)
	}
}

func TestSetPathErrors(t *testing.T) {
	file := write(t, t.TempDir(), "s.yaml", rulesYAML)
	for name, env := range map[string]string{
		"below a value":  "STELLWERK_DELIVERY__MODE__X=1",
		"index too high": "STELLWERK_ROUTES__5__NAME=x",
	} {
		if _, _, err := LoadFrom(LoadOptions{File: file, Environ: []string{"AM_TOKEN=x", "STELLWERK_DELIVERY__MODE=sync", env}}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestOptionsFromEnv(t *testing.T) {
	o := OptionsFromEnv("", nil, []string{"STELLWERK_CONFIG=/etc/s.yaml", "STELLWERK_CONFIG_DIR=/a, /b"})
	if o.File != "/etc/s.yaml" || !o.FileRequired || len(o.Dirs) != 2 || o.Dirs[1] != "/b" {
		t.Errorf("%+v", o)
	}
	o = OptionsFromEnv("", nil, nil)
	if o.File != "stellwerk.yaml" || o.FileRequired {
		t.Errorf("default file is optional: %+v", o)
	}
	o = OptionsFromEnv("my.yaml", []string{"/c"}, []string{"STELLWERK_CONFIG_DIR=/a"})
	if o.File != "my.yaml" || !o.FileRequired || len(o.Dirs) != 1 || o.Dirs[0] != "/c" {
		t.Errorf("flags win: %+v", o)
	}
}

func TestRedacted(t *testing.T) {
	out, err := Redacted(LoadOptions{File: write(t, t.TempDir(), "s.yaml", rulesYAML), Environ: []string{
		"AM_TOKEN=very-secret", "STELLWERK_TARGETS__MATRIX__ACCOUNTS__ALERTS__PASSWORD=pw123",
		"STELLWERK_UI__PASSWORD=ui-pw",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"very-secret", "pw123", "ui-pw"} {
		if strings.Contains(out, leak) {
			t.Errorf("secret %q leaked:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "homeserver: https://hs.example.org") || !strings.Contains(out, "<redacted>") {
		t.Errorf("output:\n%s", out)
	}
}

func TestAnyKeyForTheFallbackSource(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "sources._any.token", "file-token")
	cfg, _, err := LoadFrom(LoadOptions{File: write(t, t.TempDir(), "s.yaml", rulesYAML+"  \"*\": { kind: generic }\n"[0:0]), Dirs: []string{dir}, Environ: []string{
		"AM_TOKEN=x", "STELLWERK_SOURCES___ANY__KIND=generic",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Sources["*"]; s.Kind != "generic" || s.Token != "file-token" {
		t.Errorf("fallback source: %+v", cfg.Sources)
	}
}

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stellwerk.yaml")
	secrets := filepath.Join(dir, "secrets")
	_ = os.Mkdir(secrets, 0o700)
	_ = os.WriteFile(file, []byte("a: 1"), 0o600)
	_ = os.WriteFile(filepath.Join(secrets, "ha.secret"), []byte("x"), 0o600)
	o := LoadOptions{File: file, Dirs: []string{secrets}}
	first := Fingerprint(o)
	if Fingerprint(o) != first {
		t.Fatal("stable")
	}
	_ = os.WriteFile(filepath.Join(secrets, "ha.secret"), []byte("y"), 0o600)
	second := Fingerprint(o)
	if second == first {
		t.Error("a changed secret must change the fingerprint")
	}
	_ = os.WriteFile(file, []byte("a: 2"), 0o600)
	if Fingerprint(o) == second {
		t.Error("a changed file must change the fingerprint")
	}
	if Fingerprint(LoadOptions{File: "/nope", Dirs: []string{"/nope"}}) == "" {
		t.Error("missing paths still give a fingerprint")
	}
}
