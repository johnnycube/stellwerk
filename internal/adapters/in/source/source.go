// Package source is the inbound translation layer: it turns protocol-specific
// payloads into domain events. It is used by the HTTP adapter.
//
// A source kind is a Decoder registered under a name. To add a new kind
// (GitHub, Grafana, RSS, ...), implement Decoder in a new file and call
// Register from its init function. Config then uses it as
//
//	sources:
//	  mysource: { kind: <name>, token: ..., options: { ... } }
package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v3"
)

// Input is one received request.
type Input struct {
	Source string // configured source name
	Body   []byte
	Query  url.Values
	Header http.Header
}

// Decoder turns one request into zero or more events.
type Decoder interface {
	Decode(in Input) ([]*domain.Event, error)
}

// Factory builds a decoder from the source's options block (may be empty).
type Factory func(options *yaml.Node) (Decoder, error)

// Info describes a kind for the UI.
type Info struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Sample      string `json:"sample"` // example body for the payload tracer
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
	infos     = map[string]Info{}
)

func Register(info Info, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[info.Kind]; dup {
		panic("source kind registered twice: " + info.Kind)
	}
	factories[info.Kind] = f
	infos[info.Kind] = info
}

func New(kind string, options *yaml.Node) (Decoder, error) {
	mu.RLock()
	f, ok := factories[kind]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown source kind %q (known: %v)", kind, Kinds())
	}
	return f(options)
}

func Kinds() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func Infos() []Info {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Info, 0, len(infos))
	for _, i := range infos {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Kind < out[b].Kind })
	return out
}

// DecodeJSON decodes with json.Number so integers stay exact.
func DecodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// decodeOptions is a helper for factories: empty node means defaults.
func decodeOptions(n *yaml.Node, into any) error {
	if n == nil || n.Kind == 0 {
		return nil
	}
	return n.Decode(into)
}
