package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Groups is groups.json: kind ("movies", "series", "live") -> group-title -> subscribed.
type Groups map[string]map[string]bool

var kinds = []struct{ key, title string }{{"movies", "Movies"}, {"series", "Series"}, {"live", "Live"}}

func loadGroups(path string) (Groups, error) {
	g := Groups{}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return g, nil
}

func (g Groups) save(path string) error {
	b, _ := json.MarshalIndent(g, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// setConfigKeys rewrites some keys in config.json, keeping every other setting as is.
// shortcut: keys come out alphabetically ordered; fine for a small hand-edited file.
func setConfigKeys(path string, kv map[string]any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range kv {
		if m[k], err = json.Marshal(v); err != nil {
			return err
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // keep & in playlist URLs readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// on reports whether a group is subscribed, recording unknown groups with def.
func (g Groups) on(kind, group string, def bool) (on, added bool) {
	if g[kind] == nil {
		g[kind] = map[string]bool{}
	}
	v, ok := g[kind][group]
	if !ok {
		g[kind][group] = def
		return def, true
	}
	return v, false
}
