package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// EnvPrefix starts every environment variable stellwerk reads.
const EnvPrefix = "STELLWERK_"

// Reserved variables that select where config comes from. They are not
// config keys themselves.
const (
	EnvFile = EnvPrefix + "CONFIG"     // path of the YAML file
	EnvDirs = EnvPrefix + "CONFIG_DIR" // comma-separated config directories
)

// LoadOptions says where configuration comes from. Three layers, later wins:
//
//  1. Config files: File (${VAR} references are expanded), then the *.yaml /
//     *.yml fragments in Dirs, e.g. a Kubernetes ConfigMap.
//  2. Environment: STELLWERK_<PATH>=value with "__" between levels, e.g.
//     STELLWERK_DELIVERY__MODE=async. STELLWERK_<PATH>_FILE=/path reads the
//     value from a file and beats the plain variable.
//  3. Secret files: every other file in Dirs is one value, its name the key
//     path ("ha.secret" or "ha__secret"), its content the value, e.g. a
//     Kubernetes Secret volume. Hidden entries (..data) are skipped.
type LoadOptions struct {
	File         string
	FileRequired bool // false: a missing file is fine, config may come from dirs and env only
	Dirs         []string
	Environ      []string // usually os.Environ()
}

// OptionsFromEnv fills File and Dirs from STELLWERK_CONFIG and
// STELLWERK_CONFIG_DIR where the flags left them empty.
func OptionsFromEnv(file string, dirs []string, environ []string) LoadOptions {
	o := LoadOptions{File: file, FileRequired: file != "", Dirs: dirs, Environ: environ}
	env := envMap(environ)
	if o.File == "" {
		if v, ok := env[EnvFile]; ok && v != "" {
			o.File, o.FileRequired = v, true
		} else {
			o.File = "stellwerk.yaml"
		}
	}
	if len(o.Dirs) == 0 {
		for _, d := range strings.Split(env[EnvDirs], ",") {
			if d = strings.TrimSpace(d); d != "" {
				o.Dirs = append(o.Dirs, d)
			}
		}
	}
	return o
}

// LoadFrom merges all sources, then decodes strictly and validates.
// Warnings are for things that were ignored, the caller should log them.
func LoadFrom(o LoadOptions) (*Config, []string, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if err := mergeFile(root, o); err != nil {
		return nil, nil, err
	}
	dirs, err := scanDirs(o.Dirs)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range dirs { // layer 1: YAML fragments
		if err := d.mergeYAML(root); err != nil {
			return nil, nil, err
		}
	}
	warnings, err := applyOverrides(root, o.Environ, dirs)
	if err != nil {
		return nil, warnings, err
	}

	out, err := yaml.Marshal(root)
	if err != nil {
		return nil, warnings, err
	}
	cfg, err := decode(out)
	return cfg, warnings, err
}

// mergeFile merges the config file, with ${VAR} expanded, into root.
func mergeFile(root *yaml.Node, o LoadOptions) error {
	if o.File == "" {
		return nil
	}
	raw, err := os.ReadFile(o.File)
	switch {
	case err == nil:
		n, err := parseYAML([]byte(os.Expand(string(raw), lookupIn(o.Environ))), o.File)
		if err != nil {
			return err
		}
		mergeNode(root, n)
	case os.IsNotExist(err) && !o.FileRequired:
		// fine, everything may come from dirs and env
	default:
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

func scanDirs(paths []string) ([]dirFiles, error) {
	dirs := make([]dirFiles, 0, len(paths))
	for _, dir := range paths {
		d, err := scanDir(dir)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, d)
	}
	return dirs, nil
}

// applyOverrides applies the environment, then the secret files, which win.
func applyOverrides(root *yaml.Node, environ []string, dirs []dirFiles) ([]string, error) {
	warnings, err := applyEnv(root, environ)
	if err != nil {
		return warnings, err
	}
	for _, d := range dirs {
		w, err := d.applyValues(root)
		warnings = append(warnings, w...)
		if err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

func parseYAML(b []byte, where string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("config %s: %w", where, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	n := doc.Content[0]
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config %s: top level must be a mapping", where)
	}
	return n, nil
}

type dirFiles struct {
	dir           string
	yamls, values []string
}

func scanDir(dir string) (dirFiles, error) {
	d := dirFiles{dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return d, fmt.Errorf("config dir %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // Kubernetes volume internals: ..data, ..2026_11_15...
		}
		info, err := os.Stat(filepath.Join(dir, name)) // follows Kubernetes' symlinks
		if err != nil || info.IsDir() {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(name)); ext == ".yaml" || ext == ".yml" {
			d.yamls = append(d.yamls, name)
		} else {
			d.values = append(d.values, name)
		}
	}
	sort.Strings(d.yamls)
	sort.Strings(d.values)
	return d, nil
}

func (d dirFiles) mergeYAML(root *yaml.Node) error {
	for _, name := range d.yamls {
		p := filepath.Join(d.dir, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n, err := parseYAML(raw, p)
		if err != nil {
			return err
		}
		mergeNode(root, n)
	}
	return nil
}

func (d dirFiles) applyValues(root *yaml.Node) ([]string, error) {
	var warnings []string
	for _, name := range d.values {
		p := filepath.Join(d.dir, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			return warnings, err
		}
		path := splitFileKey(name)
		if !topLevelKey(path[0]) {
			warnings = append(warnings, fmt.Sprintf("ignored %s: %q is not a config key", p, path[0]))
			continue
		}
		if err := setPath(root, path, valueNode(strings.TrimRight(string(raw), "\r\n"))); err != nil {
			return warnings, fmt.Errorf("config %s: %w", p, err)
		}
	}
	return warnings, nil
}

// splitFileKey: "targets.matrix.accounts.alerts.password" or with "__".
func splitFileKey(name string) []string {
	if strings.Contains(name, "__") {
		return strings.Split(name, "__")
	}
	return strings.Split(name, ".")
}

func applyEnv(root *yaml.Node, environ []string) ([]string, error) {
	var warnings []string
	env := envMap(environ)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	// Deterministic order: STELLWERK_HA__PEERS before STELLWERK_HA__PEERS__0__URL,
	// and STELLWERK_X before STELLWERK_X_FILE, so the file-based value wins.
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, EnvPrefix) || k == EnvFile || k == EnvDirs {
			continue
		}
		name, value, err := envEntry(k, env[k])
		if err != nil {
			return warnings, err
		}
		path := strings.Split(strings.ToLower(name), "__")
		if !topLevelKey(path[0]) {
			if w := ignoredEnv(k, name, path[0]); w != "" {
				warnings = append(warnings, w)
			}
			continue
		}
		if err := setPath(root, path, valueNode(value)); err != nil {
			return warnings, fmt.Errorf("config %s: %w", k, err)
		}
	}
	return warnings, nil
}

// envEntry returns the key name without prefix and the value, read from the
// file for a _FILE variable.
func envEntry(k, value string) (string, string, error) {
	name := strings.TrimPrefix(k, EnvPrefix)
	if !strings.HasSuffix(name, "_FILE") {
		return name, value, nil
	}
	b, err := os.ReadFile(value)
	if err != nil {
		return "", "", fmt.Errorf("config %s: %w", k, err)
	}
	return strings.TrimSuffix(name, "_FILE"), strings.TrimRight(string(b), "\r\n"), nil
}

// ignoredEnv is the warning for a variable that is not a config key, empty
// for Kubernetes service links.
func ignoredEnv(k, name, top string) string {
	if kubernetesServiceLink(name) {
		return ""
	}
	hint := ""
	if strings.Contains(top, "_") {
		hint = fmt.Sprintf(" (levels are separated by __, e.g. %s%s)", EnvPrefix, strings.Replace(name, "_", "__", 1))
	}
	return fmt.Sprintf("ignored %s: not a config key%s", k, hint)
}

// kubernetesServiceLink matches variables Kubernetes injects for a Service
// named "stellwerk": STELLWERK_PORT, STELLWERK_SERVICE_HOST, STELLWERK_PORT_8080_TCP...
func kubernetesServiceLink(name string) bool {
	return name == "PORT" || strings.HasPrefix(name, "PORT_") || strings.HasPrefix(name, "SERVICE_")
}

func topLevelKey(k string) bool {
	switch k {
	case "listen", "node_id", "sources", "targets", "destinations", "templates", "routes",
		"heartbeats", "delivery", "ha", "ui", "log", "metrics", "audit", "engine", "self_ping", "canary", "tests":
		return true
	}
	return false
}

// valueNode turns a raw string into a YAML node. [..] and {..} are parsed as
// YAML flow collections, everything else is a scalar resolved like in a file,
// except null-like values, which stay text.
func valueNode(v string) *yaml.Node {
	t := strings.TrimSpace(v)
	if strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(t), &doc) == nil && len(doc.Content) == 1 {
			return doc.Content[0]
		}
	}
	n := &yaml.Node{Kind: yaml.ScalarNode, Value: v}
	switch v {
	case "null", "Null", "NULL", "~":
		n.Tag = "!!str"
	}
	return n
}

// AnyKey is how "*" is written in env names and Secret keys, which cannot
// contain "*": STELLWERK_SOURCES___ANY__TOKEN, sources._any.token.
const AnyKey = "_any"

// setPath sets path in node, creating mappings and sequences on the way.
// Existing keys match case-insensitively with "_" and "-" treated alike, so
// STELLWERK_SOURCES__K8S_EVENTS__TOKEN finds sources.k8s-events.token.
func setPath(node *yaml.Node, path []string, value *yaml.Node) error {
	cur := node
	for i, seg := range path {
		if seg == AnyKey {
			seg = "*"
		}
		next, err := setStep(cur, path, i, seg, value)
		if err != nil || next == nil {
			return err
		}
		cur = next
	}
	return nil
}

// setStep descends from cur into path[i], or sets value there if it is the
// last segment. A nil node means done.
func setStep(cur *yaml.Node, path []string, i int, seg string, value *yaml.Node) (*yaml.Node, error) {
	switch cur.Kind {
	case yaml.MappingNode:
		return setInMapping(cur, path, i, seg, value), nil
	case yaml.SequenceNode:
		return setInSequence(cur, path, i, seg, value)
	case yaml.ScalarNode:
		if cur.Value != "" || cur.Tag == "!!str" {
			return nil, fmt.Errorf("%q is a value, cannot set %q below it", strings.Join(path[:i], "."), seg)
		}
		*cur = *newChild(path, i-1) // empty "key:" in YAML, turn it into a container
		return nil, setPath(cur, path[i:], value)
	default:
		return nil, fmt.Errorf("cannot set %q", strings.Join(path, "."))
	}
}

func setInMapping(cur *yaml.Node, path []string, i int, seg string, value *yaml.Node) *yaml.Node {
	idx := findKey(cur, seg)
	if idx < 0 {
		cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: seg}, newChild(path, i))
		idx = len(cur.Content) - 2
	}
	if i == len(path)-1 {
		cur.Content[idx+1] = value
		return nil
	}
	return cur.Content[idx+1]
}

func setInSequence(cur *yaml.Node, path []string, i int, seg string, value *yaml.Node) (*yaml.Node, error) {
	n, err := strconv.Atoi(seg)
	if err != nil || n < 0 || n > len(cur.Content) {
		return nil, fmt.Errorf("%q: list index %q out of range (have %d entries)", strings.Join(path[:i], "."), seg, len(cur.Content))
	}
	if n == len(cur.Content) {
		cur.Content = append(cur.Content, newChild(path, i))
	}
	if i == len(path)-1 {
		cur.Content[n] = value
		return nil, nil
	}
	return cur.Content[n], nil
}

// newChild creates the container for path[i+1]: a list if it is a number.
func newChild(path []string, i int) *yaml.Node {
	if i+1 < len(path) {
		if _, err := strconv.Atoi(path[i+1]); err == nil {
			return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		}
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func findKey(m *yaml.Node, seg string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == seg {
			return i
		}
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if normKey(m.Content[i].Value) == normKey(seg) {
			return i
		}
	}
	return -1
}

func normKey(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "_") }

// mergeNode merges src into dst. Mappings merge key by key, anything else
// (scalars, lists) is replaced.
func mergeNode(dst, src *yaml.Node) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		*dst = *src
		return
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		if j := findKey(dst, k.Value); j >= 0 && dst.Content[j].Value == k.Value {
			mergeNode(dst.Content[j+1], v)
			continue
		}
		dst.Content = append(dst.Content, k, v)
	}
}

func envMap(environ []string) map[string]string {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func lookupIn(environ []string) func(string) string {
	m := envMap(environ)
	return func(k string) string { return m[k] }
}

// Redacted returns the effective config as YAML with secrets replaced, for
// -print-config.
func Redacted(o LoadOptions) (string, error) {
	cfg, _, err := LoadFrom(o)
	if err != nil {
		return "", err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	redact(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func redact(n *yaml.Node) {
	if n.Kind != yaml.ScalarNode {
		n.Style = 0 // block style, flow mappings from env read badly
	} else if n.Style == yaml.SingleQuotedStyle || n.Style == yaml.DoubleQuotedStyle {
		n.Style = 0 // the encoder quotes again where needed
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := strings.ToLower(n.Content[i].Value), n.Content[i+1]
			if v.Kind == yaml.ScalarNode && v.Value != "" && sensitive(k) {
				v.Value, v.Tag, v.Style = "<redacted>", "!!str", 0
				continue
			}
			redact(v)
		}
		return
	}
	for _, c := range n.Content {
		redact(c)
	}
}

func sensitive(k string) bool {
	if strings.HasSuffix(k, "_header") {
		return false // the name of a header, not its value
	}
	for _, s := range []string{"password", "token", "secret", "key", "authorization"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Fingerprint hashes everything LoadFrom reads from disk: the file and
// every entry of the config dirs. A changed fingerprint means a reload is
// worth trying. Kubernetes updates mounted ConfigMaps and Secrets by
// swapping a symlink, which changes the content seen here.
func Fingerprint(o LoadOptions) string {
	h := sha256.New()
	if b, err := os.ReadFile(o.File); err == nil {
		h.Write(b)
	}
	for _, dir := range o.Dirs {
		d, err := scanDir(dir)
		if err != nil {
			fmt.Fprintf(h, "missing %s\x00", dir)
			continue
		}
		for _, name := range append(append([]string{}, d.yamls...), d.values...) {
			b, _ := os.ReadFile(filepath.Join(dir, name))
			fmt.Fprintf(h, "%s\x00%d\x00", name, len(b))
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
