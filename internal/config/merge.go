package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// SaveEntries writes the config back, keeping everything that did not change
// exactly as it was on disk.
//
// The deck is edited by hand as often as by the WebUI: a person opens
// entries.yaml in nano, adds a comment or a field the UI does not know, and
// then locks a model from the browser. If every save re-marshalled the whole
// struct, those hand-written comments would be wiped on the next click.
//
// So the file is parsed into a node tree, and only the entries that really
// changed are replaced by a freshly built node. Unchanged entries keep their
// original node — comments included — byte for byte.
//
// raw holds the YAML text the editor sent, per entry id. Those nodes carry
// the comments the person typed, so they are written as they were typed.
func SaveEntries(path string, c *Config) error {
	return SaveEntriesWith(path, c, nil)
}

// SaveEntriesWith is SaveEntries with per-entry YAML overrides.
func SaveEntriesWith(path string, c *Config, raw map[string]*yaml.Node) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No file yet: write the whole thing.
			return Save(path, c)
		}
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		// The file on disk is broken. Do not overwrite it: the person who
		// broke it still wants to fix it.
		return fmt.Errorf("%s is not valid YAML: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return Save(path, c)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s does not contain a config mapping", path)
	}

	// What the file says, decoded, so we can tell "changed" from "same".
	var fileCfg Config
	if err := doc.Decode(&fileCfg); err != nil {
		return fmt.Errorf("%s is not a valid config: %w", path, err)
	}
	fileCfg.Normalize()
	fileEntries := map[string]*Entry{}
	for _, e := range fileCfg.Entries {
		fileEntries[e.ID] = e
	}

	// The new entries, in the order the config lists them.
	newNodes := make([]*yaml.Node, 0, len(c.Entries))
	for _, e := range c.Entries {
		fromEditor := false
		var node *yaml.Node
		if raw != nil {
			if n, ok := raw[e.ID]; ok && n != nil {
				node, fromEditor = n, true
			}
		}
		if node == nil {
			var err error
			node, err = entryNode(e)
			if err != nil {
				return err
			}
		}
		oldNode := findEntryNode(root, e.ID)
		if old, ok := fileEntries[e.ID]; ok && sameEntry(old, e) && !fromEditor {
			// Unchanged: reuse the node from the file so its comments live on.
			if oldNode != nil {
				newNodes = append(newNodes, oldNode)
				continue
			}
		}
		if oldNode != nil {
			// The comments the person wrote around the entry stay, unless
			// the text that came back already carries its own.
			if node.HeadComment == "" {
				node.HeadComment = oldNode.HeadComment
			}
			if node.FootComment == "" {
				node.FootComment = oldNode.FootComment
			}
			if node.LineComment == "" {
				node.LineComment = oldNode.LineComment
			}
		}
		newNodes = append(newNodes, node)
	}

	// Splice the merged sequence back into the document.
	oldSeq := mapValue(root, "entries")
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	seq.Content = newNodes
	if oldSeq != nil {
		// A comment written above the list stays where it was.
		seq.HeadComment = oldSeq.HeadComment
		seq.FootComment = oldSeq.FootComment
	}
	setMapValue(root, "entries", seq)

	// The other top-level blocks (server, budgets, version) are not edited
	// from the WebUI, so they are only replaced when they really differ.
	for _, key := range []string{"version", "server", "budgets"} {
		cur := mapValue(root, key)
		if cur == nil {
			continue
		}
		if sameBlock(key, cur, c) {
			continue
		}
		node, err := blockNode(key, c)
		if err != nil {
			return err
		}
		setMapValue(root, key, node)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// Keep the indentation the file already uses: a person who writes the
	// file with two spaces should not find it rewritten with four.
	enc.SetIndent(detectIndent(content))
	// The document is encoded, not just its mapping, so a comment at the top
	// of the file is kept as well.
	if err := enc.Encode(&doc); err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes())
}

// detectIndent guesses the indentation of a YAML file from its first indented
// line. Four spaces is the deck's own style and the fallback.
//
// A sequence item counts: yaml.v3 indents the items of a sequence under their
// key by exactly one indent step, so "  - id: x" means a two-space file.
func detectIndent(raw []byte) int {
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, " ") {
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		n := len(line) - len(trimmed)
		if n > 0 && n <= 8 {
			return n
		}
	}
	return 4
}

// writeFileAtomic writes the file with a .bak of the previous version.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o644)
	}
	tmp, err := os.CreateTemp(dir, ".entries-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// EntryYAML returns the YAML text of one entry as it stands in the file,
// comments included. It is what the WebUI editor shows, so the text that is
// edited is the text that is in the file.
func EntryYAML(path, id string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if len(doc.Content) == 0 {
		return "", fmt.Errorf("%s is empty", path)
	}
	node := findEntryNode(doc.Content[0], id)
	if node == nil {
		return "", fmt.Errorf("entry %q is not in %s", id, path)
	}
	b, err := yaml.Marshal(node)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseEntryYAML parses the YAML text of one entry. It returns the node (the
// text as it was written, comments included) and the entry itself.
func ParseEntryYAML(text string) (*yaml.Node, *Entry, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil, fmt.Errorf("the YAML is empty")
	}
	var e Entry
	// A field name that does not exist is an error, not a setting that is
	// quietly dropped: the editor is a text editor, so it has to be strict.
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&e); err != nil {
		return nil, nil, err
	}
	// A comment in front of the first key belongs to the entry as a whole.
	// Hoist it, so it is written above the entry in the file instead of
	// behind the list marker.
	if len(doc.Content) > 0 {
		m := doc.Content[0]
		if m.Kind == yaml.MappingNode && len(m.Content) > 0 && m.Content[0].HeadComment != "" {
			m.HeadComment = m.Content[0].HeadComment
			m.Content[0].HeadComment = ""
		}
	}
	return doc.Content[0], &e, nil
}

// entryNode turns an entry into a yaml node.
func entryNode(e *Entry) (*yaml.Node, error) {
	b, err := yaml.Marshal(e)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return nil, err
	}
	if len(node.Content) == 0 {
		return nil, fmt.Errorf("entry %q produced an empty node", e.ID)
	}
	return node.Content[0], nil
}

// blockNode turns a top-level block of the config into a yaml node.
func blockNode(key string, c *Config) (*yaml.Node, error) {
	var v any
	switch key {
	case "version":
		v = c.Version
	case "server":
		v = fileServer(c)
	case "budgets":
		v = c.Budgets
	default:
		return nil, fmt.Errorf("unknown config block %q", key)
	}
	b, err := yaml.Marshal(map[string]any{key: v})
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return nil, err
	}
	return node.Content[0].Content[1], nil
}

// sameBlock reports whether a top-level block on disk still matches the config.
func sameBlock(key string, node *yaml.Node, c *Config) bool {
	switch key {
	case "version":
		var v int
		if err := node.Decode(&v); err != nil {
			return false
		}
		return v == c.Version
	case "server":
		var s Server
		if err := node.Decode(&s); err != nil {
			return false
		}
		// The token is compared the way it is written: a token that lives in
		// the environment must not look like a change.
		want := fileServer(c)
		return reflect.DeepEqual(s, want)
	case "budgets":
		var b Budgets
		if err := node.Decode(&b); err != nil {
			return false
		}
		return reflect.DeepEqual(b, c.Budgets)
	}
	return false
}

// sameEntry compares two entries by what the deck cares about. The token-like
// and path-normalization differences are already resolved by Normalize().
func sameEntry(a, b *Entry) bool {
	ca, cb := a.Clone(), b.Clone()
	ca.Normalize()
	cb.Normalize()
	return reflect.DeepEqual(ca, cb)
}

// findEntryNode returns the mapping node of the entry with the given id.
func findEntryNode(root *yaml.Node, id string) *yaml.Node {
	seq := mapValue(root, "entries")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if scalarValue(item, "id") == id {
			return item
		}
	}
	return nil
}

// mapValue returns the value node for a key of a mapping node.
func mapValue(root *yaml.Node, key string) *yaml.Node {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}

// setMapValue replaces (or appends) the value of a key in a mapping node.
func setMapValue(root *yaml.Node, key string, val *yaml.Node) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = val
			return
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// scalarValue returns the scalar value of a key inside a mapping node.
func scalarValue(node *yaml.Node, key string) string {
	if node == nil || node.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value
		}
	}
	return ""
}
