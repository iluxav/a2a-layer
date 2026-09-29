package dashboard

import (
	"bytes"
	"errors"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The config file is edited one entry at a time (an agent, a CLI's settings, a top-level setting), and
// only that entry's lines are rewritten: the rest of the file keeps its comments, blank lines,
// wrapping and ${NAME} references byte for byte. Re-encoding the whole document with yaml.v3
// would not: it drops blank lines, unwraps long values and quotes some block scalars.
//
// A section names the mapping an entry lives in: "" for the top level, or a top-level key such
// as "agents".

// topLevelOrder is where a new top-level key goes: before the first existing key that comes
// after it here, so settings land above the cli section and that above agents.
var topLevelOrder = []string{"listen", "public_url", "env_file", "work_dir", "common_instructions", "cli", "agents"}

// entry is one key of a section with its value.
type entry struct {
	Key   string
	Value *yaml.Node
}

// entries lists a section's entries, in file order.
func entries(text []byte, section string) ([]entry, error) {
	p, err := parseDoc(text)
	if err != nil {
		return nil, err
	}
	m := p.section(section)
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []entry
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, entry{m.Content[i].Value, m.Content[i+1]})
	}
	return out, nil
}

// entryValue is the value node of key in a section, or nil.
func entryValue(text []byte, section, key string) (*yaml.Node, error) {
	p, err := parseDoc(text)
	if err != nil {
		return nil, err
	}
	_, v := lookup(p.section(section), key)
	return v, nil
}

// putEntry writes key: value into a section, in place of oldKey if the section has it (so a
// rename keeps the entry's position and the comments above it), else after its last entry.
func putEntry(text []byte, section, oldKey, key string, value *yaml.Node) ([]byte, error) {
	p, err := parseDoc(text)
	if err != nil {
		return nil, err
	}
	if section == "" {
		return p.put(p.root, oldKey, key, value)
	}
	m := p.section(section)
	if m == nil || m.Kind != yaml.MappingNode || m.Style&yaml.FlowStyle != 0 || len(m.Content) == 0 {
		// Nothing to edit in place (the section is missing, empty, or written as {…}): write the
		// whole section, as a block mapping, with the entry in it.
		whole := &yaml.Node{Kind: yaml.MappingNode}
		if m != nil && m.Kind == yaml.MappingNode {
			whole.Content = m.Content
		}
		setKey(whole, oldKey, key, value)
		return putEntry(text, "", section, section, whole)
	}
	return p.put(m, oldKey, key, value)
}

// removeEntry deletes key, and the comment lines directly above it, from a section.
func removeEntry(text []byte, section, key string) ([]byte, error) {
	p, err := parseDoc(text)
	if err != nil {
		return nil, err
	}
	m := p.section(section)
	if k, _ := lookup(m, key); k == nil {
		return text, nil
	}
	if m.Style&yaml.FlowStyle != 0 {
		whole := &yaml.Node{Kind: yaml.MappingNode, Content: slices.Clone(m.Content)}
		setKey(whole, key, "", nil)
		return putEntry(text, "", section, section, whole)
	}
	spans := p.spans(m)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		s := spans[i/2]
		p.lines = slices.Delete(p.lines, s.head, s.end+1)
		// Don't leave two blank lines where the entry was, or one under the section's key (unless
		// the section is now empty: the blank line then separates it from what follows).
		first := i == 0 && section != "" && len(m.Content) > 2
		if s.head < len(p.lines) && blank(p.lines[s.head]) && (first || s.head > 0 && blank(p.lines[s.head-1])) {
			p.lines = slices.Delete(p.lines, s.head, s.head+1)
		}
		break
	}
	return p.bytes(), nil
}

// doc is a config file's lines with its parsed node tree.
type doc struct {
	lines []string   // without their newlines
	root  *yaml.Node // the top-level mapping; nil for an empty file
}

func parseDoc(text []byte) (*doc, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(text, &n); err != nil {
		return nil, err
	}
	p := &doc{}
	if s := strings.TrimSuffix(string(text), "\n"); s != "" {
		p.lines = strings.Split(s, "\n")
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		switch r := n.Content[0]; {
		case r.Kind == yaml.MappingNode && r.Style&yaml.FlowStyle == 0:
			p.root = r
		case r.Kind == yaml.ScalarNode && r.Tag == "!!null":
		default:
			return nil, errors.New("the config's top level is not a block mapping (key: value lines), so it cannot be edited here")
		}
	}
	return p, nil
}

func (p *doc) bytes() []byte {
	if len(p.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(p.lines, "\n") + "\n")
}

// section is the value of a top-level key ("" is the top level itself), or nil.
func (p *doc) section(name string) *yaml.Node {
	if name == "" {
		return p.root
	}
	_, v := lookup(p.root, name)
	return v
}

// span is the lines of one mapping entry, 0-based and inclusive: head is the first of the
// comment lines directly above its key (or the key's own line), start the key's line, end its
// last line. Blank lines and less-indented comments after an entry belong to what follows.
type span struct{ head, start, end int }

func (p *doc) spans(m *yaml.Node) []span {
	out := make([]span, len(m.Content)/2)
	for i := range out {
		k := m.Content[2*i]
		col, start := k.Column-1, k.Line-1
		head := start
		for head > 0 && isComment(p.lines[head-1]) && indent(p.lines[head-1]) == col {
			head--
		}
		out[i] = span{head: head, start: start}
	}
	for i := range out {
		col := m.Content[2*i].Column - 1
		end := out[i].start
		if i+1 < len(out) {
			end = out[i+1].head - 1
		} else {
			for j := end + 1; j < len(p.lines); j++ {
				l := p.lines[j]
				// A block sequence may sit at its key's own indentation.
				if blank(l) || isComment(l) || indent(l) > col || indent(l) == col && isSeqItem(l) {
					end = j
					continue
				}
				break
			}
		}
		for end > out[i].start && (blank(p.lines[end]) || isComment(p.lines[end]) && indent(p.lines[end]) <= col) {
			end--
		}
		out[i].end = end
	}
	return out
}

// put replaces oldKey's lines in m with key: value, or adds the entry to m.
func (p *doc) put(m *yaml.Node, oldKey, key string, value *yaml.Node) ([]byte, error) {
	if m == nil || len(m.Content) == 0 {
		// An empty file (or one with only comments): the entry starts the document.
		text, err := emit(key, "", value, 0)
		if err != nil {
			return nil, err
		}
		if len(p.lines) > 0 && !blank(p.lines[len(p.lines)-1]) {
			text = append([]string{""}, text...)
		}
		p.lines = append(p.lines, text...)
		return p.bytes(), nil
	}
	col := m.Content[0].Column - 1
	spans := p.spans(m)
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Value != oldKey {
			continue
		}
		text, err := emit(key, k.LineComment, value, col)
		if err != nil {
			return nil, err
		}
		s := spans[i/2]
		p.lines = slices.Replace(p.lines, s.start, s.end+1, text...)
		return p.bytes(), nil
	}
	text, err := emit(key, "", value, col)
	if err != nil {
		return nil, err
	}
	if m == p.root {
		// A new top-level key goes after the last key that precedes it in topLevelOrder.
		if at := slices.Index(topLevelOrder, key); at >= 0 {
			for i := 0; i+1 < len(m.Content); i += 2 {
				if j := slices.Index(topLevelOrder, m.Content[i].Value); j > at {
					if i == 0 {
						p.lines = slices.Insert(p.lines, spans[0].head, append(text, "")...)
					} else {
						at := spans[i/2-1].end + 1
						// A section (a mapping) set off by blank lines gets one before it too.
						if value.Kind == yaml.MappingNode && at < len(p.lines) && blank(p.lines[at]) {
							text = append([]string{""}, text...)
						}
						p.lines = slices.Insert(p.lines, at, text...)
					}
					return p.bytes(), nil
				}
			}
		}
	}
	last := spans[len(spans)-1]
	// Entries separated by blank lines get one before the new entry too.
	if len(spans) > 1 && blank(p.lines[last.head-1]) {
		text = append([]string{""}, text...)
	}
	p.lines = slices.Insert(p.lines, last.end+1, text...)
	return p.bytes(), nil
}

// emit encodes key: value as block YAML, indented by col spaces.
func emit(key, lineComment string, value *yaml.Node, col int) ([]string, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	k := &yaml.Node{Kind: yaml.ScalarNode, Value: key, LineComment: lineComment}
	if err := enc.Encode(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{k, value}}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	pad := strings.Repeat(" ", col)
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return lines, nil
}

func blank(l string) bool     { return strings.TrimSpace(l) == "" }
func isComment(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }
func indent(l string) int     { return len(l) - len(strings.TrimLeft(l, " ")) }
func isSeqItem(l string) bool {
	t := strings.TrimSpace(l)
	return t == "-" || strings.HasPrefix(t, "- ")
}

// Node helpers, for building and merging the values put into the file.

// lookup finds key in mapping m.
func lookup(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// setKey sets key in mapping m to v, in place of oldKey if present; a nil v removes oldKey.
func setKey(m *yaml.Node, oldKey, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != oldKey {
			continue
		}
		if v == nil {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
		m.Content[i].Value = key
		if v.LineComment == "" {
			v.LineComment = m.Content[i+1].LineComment
		}
		m.Content[i+1] = v
		return
	}
	if v != nil {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	}
}

// strNode is a string value: a literal block when it spans lines, else plain or quoted as needed.
func strNode(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	if strings.Contains(s, "\n") {
		n.Style = yaml.LiteralStyle
	}
	return n
}

// plainNode is a value written as is, such as a number, a bool, or ${NAME} standing for one.
func plainNode(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: s} }

func listNode(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, it := range items {
		n.Content = append(n.Content, strNode(it))
	}
	return n
}

// setScalar sets key in m to the scalar s (removing it when s is empty). An unchanged value keeps
// its node, so its quoting and wrapping stay as they were.
func setScalar(m *yaml.Node, key, s string, node func(string) *yaml.Node) {
	_, old := lookup(m, key)
	switch {
	case s == "":
		setKey(m, key, key, nil)
	case old != nil && old.Kind == yaml.ScalarNode && old.Value == s:
	default:
		setKey(m, key, key, node(s))
	}
}

// setList sets key in m to a list (removing it when empty), keeping an unchanged list's node.
func setList(m *yaml.Node, key string, items []string) {
	_, old := lookup(m, key)
	var have []string
	if old != nil && old.Decode(&have) == nil && slices.Equal(have, items) && len(items) > 0 {
		return
	}
	if len(items) == 0 {
		setKey(m, key, key, nil)
		return
	}
	n := listNode(items)
	if old != nil && old.Kind == yaml.SequenceNode {
		n.Style = old.Style
	}
	setKey(m, key, key, n)
}

// setMap sets key in m to a string map in the given key order (removing it when empty), keeping
// the nodes of unchanged values.
func setMap(m *yaml.Node, key string, keys []string, values map[string]string) {
	if len(keys) == 0 {
		setKey(m, key, key, nil)
		return
	}
	_, old := lookup(m, key)
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		_, ov := lookup(old, k)
		if ov != nil && ov.Kind == yaml.ScalarNode && ov.Value == values[k] {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, ov)
			continue
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, strNode(values[k]))
	}
	setKey(m, key, key, n)
}

// childMapping is the mapping at key in m, created (unattached) when absent.
func childMapping(m *yaml.Node, key string) *yaml.Node {
	if _, v := lookup(m, key); v != nil && v.Kind == yaml.MappingNode {
		v.Style = 0
		return v
	}
	return &yaml.Node{Kind: yaml.MappingNode}
}

// setChild attaches mapping c at key in m, or removes the key when c is empty.
func setChild(m *yaml.Node, key string, c *yaml.Node) {
	if len(c.Content) == 0 {
		setKey(m, key, key, nil)
		return
	}
	if _, v := lookup(m, key); v != c {
		setKey(m, key, key, c)
	}
}
