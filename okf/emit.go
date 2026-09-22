package okf

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// This file ports the parts of ruamel.yaml's emitter that frontmatter.join reaches:
// the round-trip representer with default_flow_style=None and width=4096, and no
// comments, anchors, tags or block scalars, none of which survive a jsonb round trip.

const (
	bestWidth          = 4096
	bestIndent         = 2
	maxSimpleKeyLength = 128
)

type indentEntry struct {
	val int
	seq bool
}

type emitter struct {
	out strings.Builder
	// indent is -1 before the root node, where ruamel has None.
	indent    int
	indents   []indentEntry
	flowLevel int
	column    int

	// ruamel's no_newline is only ever set by tags, anchors and comments, so it is left out.
	whitespace, indention bool
}

// emitRoot renders m as a block mapping, as join does after fa.set_block_style().
func emitRoot(m *Map) (string, error) {
	e := &emitter{indent: -1, whitespace: true, indention: true}
	if err := e.mapping(m, true, false); err != nil {
		return "", err
	}
	e.writeIndent()
	return e.out.String(), nil
}

func (e *emitter) node(v any, seqCtx, mapCtx, simpleKey bool) error {
	switch v := v.(type) {
	case []any:
		return e.sequence(v, mapCtx)
	case *Map:
		return e.mapping(v, false, mapCtx)
	}
	text, isStr, err := scalarText(v)
	if err != nil {
		return err
	}
	e.scalar(text, isStr, seqCtx, simpleKey)
	return nil
}

// scalarText is the representer: the text of a scalar and whether it is a string.
func scalarText(v any) (string, bool, error) {
	switch v := v.(type) {
	case string:
		return v, true, nil
	case nil:
		return "", false, nil
	case bool:
		return strconv.FormatBool(v), false, nil
	case int:
		return strconv.Itoa(v), false, nil
	case json.Number:
		if isIntLiteral(string(v)) {
			i, ok := new(big.Int).SetString(string(v), 10)
			if !ok {
				return "", false, fmt.Errorf("okf: %q is not a number", string(v))
			}
			return i.String(), false, nil
		}
		f, _ := strconv.ParseFloat(string(v), 64)
		switch {
		case math.IsInf(f, 1):
			return ".inf", false, nil
		case math.IsInf(f, -1):
			return "-.inf", false, nil
		}
		return pyFloatRepr(f), false, nil
	case NonFinite:
		return "", false, fmt.Errorf("okf: frontmatter holds %s, which the store cannot hold", string(v))
	}
	return "", false, fmt.Errorf("okf: %T is outside the frontmatter value domain", v)
}

// isLeaf is ruamel's best_style: a collection whose children are all scalars is flow style.
func isLeaf(v any) bool {
	scalar := func(x any) bool {
		switch x.(type) {
		case []any, *Map:
			return false
		}
		return true
	}
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			if !scalar(x) {
				return false
			}
		}
	case *Map:
		for _, k := range v.keys {
			if !scalar(v.vals[k]) {
				return false
			}
		}
	}
	return true
}

func (e *emitter) sequence(items []any, mapCtx bool) error {
	flow := e.flowLevel > 0 || isLeaf(items)
	if flow {
		e.writeIndicator(strings.Repeat(" ", max(e.seqFlowAlign(), 0))+"[", true, true, false)
		e.increaseIndent(true, true, false)
		e.flowLevel++
	} else {
		e.increaseIndent(false, true, mapCtx && !e.indention)
	}
	if e.seqSeq() {
		e.indention = true
	}
	for i, it := range items {
		if flow {
			if i > 0 {
				e.writeIndicator(",", false, false, false)
			}
			if e.column > bestWidth {
				e.writeIndent()
			}
		} else {
			e.writeIndent()
			e.writeIndicator("-", true, false, true)
		}
		if err := e.node(it, true, false, false); err != nil {
			return err
		}
	}
	e.popIndent()
	if flow {
		e.flowLevel--
		e.writeIndicator("]", false, false, false)
		if len(items) == 0 && e.flowLevel == 0 {
			e.writeLineBreak("\n")
		}
	}
	return nil
}

func (e *emitter) mapping(m *Map, root, mapCtx bool) error {
	flow := e.flowLevel > 0 || m.Len() == 0 || (!root && isLeaf(m))
	if flow {
		e.writeIndicator(strings.Repeat(" ", max(e.seqFlowAlign(), 0))+"{", true, true, false)
		e.increaseIndent(true, false, false)
		e.flowLevel++
	} else {
		e.increaseIndent(false, false, false)
	}
	for i, k := range m.keys {
		if flow {
			if i > 0 {
				e.writeIndicator(",", false, false, false)
			}
			if e.column > bestWidth {
				e.writeIndent()
			}
		} else {
			e.writeIndent()
		}
		if err := e.pair(k, m.vals[k], flow); err != nil {
			return err
		}
	}
	e.popIndent()
	if flow {
		e.flowLevel--
		e.writeIndicator("}", false, false, false)
		if m.Len() == 0 && e.flowLevel == 0 {
			e.writeLineBreak("\n")
		}
	}
	return nil
}

func (e *emitter) pair(k string, v any, flow bool) error {
	a := analyze(k)
	// ruamel counts the unwritten "!!str" tag against the simple-key limit.
	if len(a.scalar)+len("!!str") < maxSimpleKeyLength && !a.multiline {
		e.scalar(k, true, false, true)
		e.writeIndicator(":", false, false, false)
	} else if flow {
		e.writeIndicator("?", true, false, false)
		e.scalar(k, true, false, false)
		if e.column > bestWidth {
			e.writeIndent()
		}
		e.writeIndicator(":", true, false, false)
	} else {
		e.writeIndicator("?", true, false, true)
		e.scalar(k, true, false, false)
		e.writeIndent()
		e.writeIndicator(":", true, false, true)
	}
	return e.node(v, false, true, false)
}

func (e *emitter) scalar(text string, isStr, seqCtx, simpleKey bool) {
	a := analyze(text)
	implicit := !isStr || resolvesToStr(text)
	style := e.chooseStyle(a, implicit, simpleKey)
	// ruamel writes a null that cannot be plain as the word null.
	if !isStr && text == "" && style == '\'' {
		a = analyze("null")
		style = e.chooseStyle(a, true, simpleKey)
	}
	e.increaseIndent(true, false, false)
	split := !simpleKey
	if seqCtx && e.flowLevel == 0 {
		e.writeIndent()
	}
	switch style {
	case '"':
		e.writeDoubleQuoted(a.scalar, split)
	case '\'':
		e.writeSingleQuoted(a.scalar, split)
	default:
		e.writePlain(a.scalar, split)
	}
	e.popIndent()
}

// resolvesToStr reports whether a plain scalar would load back as a string under ruamel's YAML 1.2 resolver.
func resolvesToStr(s string) bool {
	switch s {
	case "<<", "=", "!", "&", "*":
		return false
	}
	return implicitTag(s) == "!!str"
}

func (e *emitter) chooseStyle(a analysis, implicit, simpleKey bool) byte {
	if implicit && !(simpleKey && (a.empty || a.multiline)) &&
		(e.flowLevel > 0 && a.allowFlowPlain || e.flowLevel == 0 && a.allowBlockPlain) {
		return 0
	}
	if strings.ContainsAny(string(a.scalar), "'\n") {
		return '"'
	}
	if a.allowSingleQuoted && !(simpleKey && a.multiline) {
		return '\''
	}
	return '"'
}

func (e *emitter) increaseIndent(flow, seq, indentless bool) {
	e.indents = append(e.indents, indentEntry{e.indent, seq})
	switch {
	case e.indent < 0 && !flow:
		e.indent = 0
	case e.indent >= 0 && !indentless:
		e.indent += bestIndent
	}
}

func (e *emitter) popIndent() {
	e.indent = e.indents[len(e.indents)-1].val
	e.indents = e.indents[:len(e.indents)-1]
}

func (e *emitter) seqSeq() bool {
	n := len(e.indents)
	return n >= 2 && e.indents[n-2].seq && e.indents[n-1].seq
}

// seqFlowAlign is the extra space ruamel puts after a dash before a flow collection.
func (e *emitter) seqFlowAlign() int {
	n := len(e.indents)
	if n < 2 || !e.indents[n-1].seq {
		return 0
	}
	return max(e.indents[n-1].val, 0) + bestIndent - e.column - 1
}

func (e *emitter) write(r []rune) {
	e.column += len(r)
	e.out.WriteString(string(r))
}

func (e *emitter) writeIndicator(ind string, needWhitespace, whitespace, indention bool) {
	if !e.whitespace && needWhitespace {
		ind = " " + ind
	}
	e.whitespace = whitespace
	e.indention = e.indention && indention
	e.write([]rune(ind))
}

func (e *emitter) writeIndent() {
	ind := max(e.indent, 0)
	if !e.indention || e.column > ind || (e.column == ind && !e.whitespace) {
		e.writeLineBreak("\n")
	}
	if e.column < ind {
		e.whitespace = true
		e.write([]rune(strings.Repeat(" ", ind-e.column)))
	}
}

func (e *emitter) writeLineBreak(br string) {
	e.whitespace, e.indention = true, true
	e.column = 0
	e.out.WriteString(br)
}

const noRune rune = -1

func isBreak(r rune) bool { return r == '\n' || r == '\x85' || r == ' ' || r == ' ' }

func isBlankOrEnd(r rune) bool { return r == 0 || r == ' ' || r == '\t' || r == '\r' || isBreak(r) }

type analysis struct {
	scalar                                             []rune
	empty, multiline                                   bool
	allowFlowPlain, allowBlockPlain, allowSingleQuoted bool
}

// analyze is ruamel's analyze_scalar with allow_unicode on and YAML 1.2.
func analyze(s string) analysis {
	t := []rune(s)
	if len(t) == 0 {
		return analysis{scalar: t, empty: true, allowBlockPlain: true, allowSingleQuoted: true}
	}
	var blockInd, flowInd, lineBreaks, special bool
	var leadingSpace, leadingBreak, trailingSpace, trailingBreak, breakSpace, spaceBreak bool
	if strings.HasPrefix(s, "---") || strings.HasPrefix(s, "...") {
		blockInd, flowInd = true, true
	}
	precededByWS := true
	followedByWS := len(t) == 1 || isBlankOrEnd(t[1])
	prevSpace, prevBreak := false, false
	for i, ch := range t {
		if i == 0 {
			if strings.ContainsRune("#,[]{}&*!|>'\"%@`", ch) {
				flowInd, blockInd = true, true
			}
			if ch == '?' || ch == ':' {
				if len(t) == 1 {
					flowInd = true
				}
				if followedByWS {
					blockInd = true
				}
			}
			if ch == '-' && followedByWS {
				flowInd, blockInd = true, true
			}
		} else {
			if strings.ContainsRune(",[]{}", ch) {
				flowInd = true
			}
			if ch == ':' && followedByWS {
				flowInd, blockInd = true, true
			}
			if ch == '#' && precededByWS {
				flowInd, blockInd = true, true
			}
		}
		if isBreak(ch) {
			lineBreaks = true
		}
		if !(ch == '\n' || ch >= 0x20 && ch <= 0x7E) {
			unicode := ch == 0x85 || ch >= 0xA0 && ch <= 0xD7FF || ch >= 0xE000 && ch <= 0xFFFD || ch >= 0x10000 && ch <= 0x10FFFF
			if !unicode || ch == 0xFEFF {
				special = true
			}
		}
		switch {
		case ch == ' ':
			leadingSpace = leadingSpace || i == 0
			trailingSpace = trailingSpace || i == len(t)-1
			breakSpace = breakSpace || prevBreak
			prevSpace, prevBreak = true, false
		case isBreak(ch):
			leadingBreak = leadingBreak || i == 0
			trailingBreak = trailingBreak || i == len(t)-1
			spaceBreak = spaceBreak || prevSpace
			prevSpace, prevBreak = false, true
		default:
			prevSpace, prevBreak = false, false
		}
		precededByWS = isBlankOrEnd(ch)
		followedByWS = i+2 >= len(t) || isBlankOrEnd(t[i+2])
	}
	a := analysis{scalar: t, multiline: lineBreaks, allowFlowPlain: true, allowBlockPlain: true, allowSingleQuoted: true}
	if leadingSpace || leadingBreak || trailingSpace || trailingBreak {
		a.allowFlowPlain, a.allowBlockPlain = false, false
	}
	if breakSpace || special || spaceBreak {
		a.allowFlowPlain, a.allowBlockPlain, a.allowSingleQuoted = false, false, false
	}
	if lineBreaks {
		a.allowFlowPlain, a.allowBlockPlain = false, false
	}
	if flowInd {
		a.allowFlowPlain = false
	}
	if blockInd {
		a.allowBlockPlain = false
	}
	return a
}

// writePlain never sees a line break: analyze forbids plain style for multiline scalars.
func (e *emitter) writePlain(t []rune, split bool) {
	if len(t) == 0 {
		return
	}
	if !e.whitespace {
		e.write([]rune(" "))
	}
	e.whitespace, e.indention = false, false
	spaces := false
	start := 0
	for end := 0; end <= len(t); end++ {
		ch := noRune
		if end < len(t) {
			ch = t[end]
		}
		if spaces {
			if ch != ' ' {
				if start+1 == end && e.column >= bestWidth && split {
					e.writeIndent()
					e.whitespace, e.indention = false, false
				} else {
					e.write(t[start:end])
				}
				start = end
			}
		} else if ch == noRune || ch == ' ' {
			if end-start+e.column > bestWidth && e.indent >= 0 && e.column > e.indent {
				// A word longer than the line gets a line of its own.
				e.writeIndent()
			}
			e.write(t[start:end])
			start = end
		}
		if ch != noRune {
			spaces = ch == ' '
		}
	}
}

func (e *emitter) writeSingleQuoted(t []rune, split bool) {
	e.writeIndicator("'", true, false, false)
	spaces, breaks := false, false
	start := 0
	for end := 0; end <= len(t); end++ {
		ch := noRune
		if end < len(t) {
			ch = t[end]
		}
		switch {
		case spaces:
			if ch != ' ' {
				if start+1 == end && e.column > bestWidth && split && start != 0 && end != len(t) {
					e.writeIndent()
				} else {
					e.write(t[start:end])
				}
				start = end
			}
		case breaks:
			if ch == noRune || !isBreak(ch) {
				if t[start] == '\n' {
					e.writeLineBreak("\n")
				}
				for _, br := range t[start:end] {
					e.writeLineBreak(string(br))
				}
				e.writeIndent()
				start = end
			}
		default:
			if (ch == noRune || ch == ' ' || isBreak(ch) || ch == '\'') && start < end {
				e.write(t[start:end])
				start = end
			}
		}
		if ch == '\'' {
			e.write([]rune("''"))
			start = end + 1
		}
		if ch != noRune {
			spaces, breaks = ch == ' ', isBreak(ch)
		}
	}
	e.writeIndicator("'", false, false, false)
}

var escapes = map[rune]string{
	0: "0", '\a': "a", '\b': "b", '\t': "t", '\n': "n", '\v': "v", '\f': "f", '\r': "r",
	0x1B: "e", '"': `"`, '\\': `\`, 0x85: "N", 0xA0: "_", 0x2028: "L", 0x2029: "P",
}

func needsEscape(ch rune) bool {
	switch ch {
	case '"', '\\', 0x85, 0x2028, 0x2029, 0xFEFF:
		return true
	}
	printable := ch >= 0x20 && ch <= 0x7E || ch >= 0xA0 && ch <= 0xD7FF || ch >= 0xE000 && ch <= 0xFFFD || ch >= 0x10000 && ch <= 0x10FFFF
	return !printable
}

func (e *emitter) writeDoubleQuoted(t []rune, split bool) {
	e.writeIndicator(`"`, true, false, false)
	start := 0
	for end := 0; end <= len(t); end++ {
		ch := noRune
		if end < len(t) {
			ch = t[end]
		}
		if ch == noRune || needsEscape(ch) {
			if start < end {
				e.write(t[start:end])
				start = end
			}
			if ch != noRune {
				esc, ok := escapes[ch]
				switch {
				case ok:
					esc = `\` + esc
				case ch <= 0xFF:
					esc = fmt.Sprintf(`\x%02X`, ch)
				case ch <= 0xFFFF:
					esc = fmt.Sprintf(`\u%04X`, ch)
				default:
					esc = fmt.Sprintf(`\U%08X`, ch)
				}
				e.write([]rune(esc))
				start = end + 1
			}
		}
		if 0 < end && end < len(t)-1 && (ch == ' ' || start >= end) && e.column+(end-start) > bestWidth && split {
			needBackslash := !canFoldAt(t, start, end)
			var data []rune
			if start < end {
				data = t[start:end:end]
			}
			if needBackslash {
				data = append(data, '\\')
			}
			if start < end {
				start = end
			}
			e.write(data)
			e.writeIndent()
			e.whitespace, e.indention = false, false
			if t[start] == ' ' {
				if needBackslash {
					e.write([]rune(`\`))
				} else {
					start++
				}
			}
		}
	}
	e.writeIndicator(`"`, false, false, false)
}

// canFoldAt is ruamel's test for breaking a double-quoted line without a trailing backslash.
// Python's IndexError and ValueError there both mean a backslash is needed.
func canFoldAt(t []rune, start, end int) bool {
	space := indexRune(t, ' ', end, len(t))
	if space < 0 {
		return false
	}
	if nl := indexRune(t, '\n', end, space); nl >= 0 {
		space = nl
	}
	if space+1 >= len(t) {
		return false
	}
	if t[space] == '\n' && t[space+1] != ' ' {
		return false
	}
	between := string(t[end:space])
	return !strings.ContainsAny(between, `"'`) &&
		t[space+1] != ' ' && t[space+1] != '\n' &&
		!(t[end-1] == ' ' && t[end] == ' ') &&
		start != end
}

func indexRune(t []rune, r rune, from, to int) int {
	for i := from; i < to; i++ {
		if t[i] == r {
			return i
		}
	}
	return -1
}
