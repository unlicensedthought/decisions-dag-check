package main

// json.loads and json.dumps as CPython 3.9's json package does them: the C scanner's error
// messages and positions (in code points), NaN/Infinity, lone surrogates, float repr.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// jsonError is json.JSONDecodeError (a ValueError).
type jsonError struct {
	msg string
	pos int
	doc []rune
}

func (e jsonError) Error() string {
	line, col := 1, e.pos+1
	for i := 0; i < e.pos; i++ {
		if e.doc[i] == '\n' {
			line++
			col = e.pos - i
		}
	}
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.msg, line, col, e.pos)
}

type scanner struct{ s []rune }

func (p *scanner) fail(msg string, pos int) { panic(jsonError{msg, pos, p.s}) }

func isWS(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func (p *scanner) ws(i int) int {
	for i < len(p.s) && isWS(p.s[i]) {
		i++
	}
	return i
}

// loads is json.loads(text). A decode error comes back as a jsonError.
func loads(text string) (v any, err error) {
	s := []rune(text)
	p := &scanner{s}
	defer func() {
		if r := recover(); r != nil {
			if je, ok := r.(jsonError); ok {
				err = je
				return
			}
			panic(r)
		}
	}()
	if len(s) > 0 && s[0] == 0xFEFF {
		p.fail("Unexpected UTF-8 BOM (decode using utf-8-sig)", 0)
	}
	v, end := p.scanOnce(p.ws(0))
	if end = p.ws(end); end != len(s) {
		p.fail("Extra data", end)
	}
	return v, nil
}

// mustLoads is json.loads where check.py does not catch the error.
func mustLoads(text string) any {
	v, err := loads(text)
	if err != nil {
		die("JSONDecodeError", "%s", err.Error())
	}
	return v
}

func (p *scanner) lit(i int, w string) bool {
	for k, r := range w {
		if p.s[i+k] != r {
			return false
		}
	}
	return true
}

func (p *scanner) scanOnce(i int) (any, int) {
	n := len(p.s)
	if i >= n {
		p.fail("Expecting value", i)
	}
	switch p.s[i] {
	case '"':
		return p.scanString(i + 1)
	case '{':
		return p.object(i + 1)
	case '[':
		return p.array(i + 1)
	case 'n':
		if i+3 < n && p.lit(i, "null") {
			return nil, i + 4
		}
	case 't':
		if i+3 < n && p.lit(i, "true") {
			return true, i + 4
		}
	case 'f':
		if i+4 < n && p.lit(i, "false") {
			return false, i + 5
		}
	case 'N':
		if i+2 < n && p.lit(i, "NaN") {
			return math.NaN(), i + 3
		}
	case 'I':
		if i+7 < n && p.lit(i, "Infinity") {
			return math.Inf(1), i + 8
		}
	case '-':
		if i+8 < n && p.lit(i, "-Infinity") {
			return math.Inf(-1), i + 9
		}
	}
	return p.number(i)
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func (p *scanner) number(start int) (any, int) {
	s, last := p.s, len(p.s)-1
	i := start
	if s[i] == '-' {
		i++
		if i > last {
			p.fail("Expecting value", start)
		}
	}
	switch {
	case s[i] >= '1' && s[i] <= '9':
		i++
		for i <= last && isDigit(s[i]) {
			i++
		}
	case s[i] == '0':
		i++
	default:
		p.fail("Expecting value", start)
	}
	isFloat := false
	if i < last && s[i] == '.' && isDigit(s[i+1]) {
		isFloat = true
		i += 2
		for i <= last && isDigit(s[i]) {
			i++
		}
	}
	if i < last && (s[i] == 'e' || s[i] == 'E') {
		e := i
		i++
		if i < last && (s[i] == '-' || s[i] == '+') {
			i++
		}
		for i <= last && isDigit(s[i]) {
			i++
		}
		if isDigit(s[i-1]) {
			isFloat = true
		} else {
			i = e
		}
	}
	txt := string(s[start:i])
	if isFloat {
		f, _ := strconv.ParseFloat(txt, 64) // out of range: +-Inf or 0, as float() gives
		return f, i
	}
	z, _ := new(big.Int).SetString(txt, 10)
	return z, i
}

func hexVal(r rune) (rune, bool) {
	switch {
	case r >= '0' && r <= '9':
		return r - '0', true
	case r >= 'a' && r <= 'f':
		return r - 'a' + 10, true
	case r >= 'A' && r <= 'F':
		return r - 'A' + 10, true
	}
	return 0, false
}

// scanString is scanstring_unicode(strict=True); end is the index after the opening quote.
func (p *scanner) scanString(end int) (string, int) {
	s, n := p.s, len(p.s)
	begin := end - 1
	var out []rune
	for {
		next := end
		var c rune
		for ; next < n; next++ {
			c = s[next]
			if c == '"' || c == '\\' {
				break
			}
			if c <= 0x1f {
				p.fail("Invalid control character at", next)
			}
		}
		if next >= n {
			p.fail("Unterminated string starting at", begin)
		}
		out = append(out, s[end:next]...)
		next++
		if c == '"' {
			end = next
			break
		}
		if next == n {
			p.fail("Unterminated string starting at", begin)
		}
		c = s[next]
		if c != 'u' {
			end = next + 1
			switch c {
			case '"', '\\', '/':
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			default:
				p.fail("Invalid \\escape", end-2)
			}
		} else {
			next++
			end = next + 4
			if end >= n {
				p.fail("Invalid \\uXXXX escape", next-1)
			}
			c = 0
			for ; next < end; next++ {
				d, ok := hexVal(s[next])
				if !ok {
					p.fail("Invalid \\uXXXX escape", end-5)
				}
				c = c<<4 | d
			}
			if c >= 0xD800 && c <= 0xDBFF && end+6 < n && s[end] == '\\' && s[end+1] == 'u' {
				var c2 rune
				end += 6
				for next = end - 4; next < end; next++ {
					d, ok := hexVal(s[next])
					if !ok {
						p.fail("Invalid \\uXXXX escape", end-5)
					}
					c2 = c2<<4 | d
				}
				if c2 >= 0xDC00 && c2 <= 0xDFFF {
					c = 0x10000 + (c-0xD800)<<10 + (c2 - 0xDC00)
				} else {
					end -= 6
				}
			}
		}
		out = append(out, c)
	}
	return fromRunes(out), end
}

func (p *scanner) object(i int) (any, int) {
	s, last := p.s, len(p.s)-1
	d := newDict()
	i = p.ws(i)
	if i > last || s[i] != '}' {
		for {
			if i > last || s[i] != '"' {
				p.fail("Expecting property name enclosed in double quotes", i)
			}
			var key string
			key, i = p.scanString(i + 1)
			i = p.ws(i)
			if i > last || s[i] != ':' {
				p.fail("Expecting ':' delimiter", i)
			}
			var v any
			v, i = p.scanOnce(p.ws(i + 1))
			d.set(key, v)
			i = p.ws(i)
			if i <= last && s[i] == '}' {
				break
			}
			if i > last || s[i] != ',' {
				p.fail("Expecting ',' delimiter", i)
			}
			i = p.ws(i + 1)
		}
	}
	return d, i + 1
}

func (p *scanner) array(i int) (any, int) {
	s, last := p.s, len(p.s)-1
	l := &List{}
	i = p.ws(i)
	if i > last || s[i] != ']' {
		for {
			var v any
			v, i = p.scanOnce(i)
			l.items = append(l.items, v)
			i = p.ws(i)
			if i <= last && s[i] == ']' {
				break
			}
			if i > last || s[i] != ',' {
				p.fail("Expecting ',' delimiter", i)
			}
			i = p.ws(i + 1)
		}
	}
	return l, i + 1
}

// ------------------------------------------------------------------------------- dumps ---

// encodeStr is json's encode_basestring (ensure_ascii=False).
func encodeStr(b []byte, s string) []byte {
	b = append(b, '"')
	for _, r := range runes(s) {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		default:
			if r < 0x20 {
				b = fmt.Appendf(b, "\\u%04x", r)
			} else {
				b = appendRune(b, r)
			}
		}
	}
	return append(b, '"')
}

func encodeScalar(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...)
	case bool:
		if x {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case *big.Int:
		return append(b, x.String()...)
	case float64:
		switch {
		case math.IsNaN(x):
			return append(b, "NaN"...)
		case math.IsInf(x, 1):
			return append(b, "Infinity"...)
		case math.IsInf(x, -1):
			return append(b, "-Infinity"...)
		}
		return append(b, floatRepr(x)...)
	case string:
		return encodeStr(b, x)
	}
	panic(fmt.Sprintf("decisions-check: encodeScalar of %T", v))
}

// dumpsCompactSorted is json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False).
func dumpsCompactSorted(b []byte, v any) []byte {
	switch x := v.(type) {
	case *List:
		b = append(b, '[')
		for i, it := range x.items {
			if i > 0 {
				b = append(b, ',')
			}
			b = dumpsCompactSorted(b, it)
		}
		return append(b, ']')
	case *Dict:
		keys := sortedStrings(x.keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = encodeStr(b, k)
			b = append(b, ':')
			b = dumpsCompactSorted(b, x.m[k])
		}
		return append(b, '}')
	}
	return encodeScalar(b, v)
}

// dumpsIndent2 is json.dumps(v, indent=2, ensure_ascii=False).
func dumpsIndent2(b []byte, v any, level int) []byte {
	nl := func(l int) []byte { return append(append(b, '\n'), strings.Repeat("  ", l)...) }
	switch x := v.(type) {
	case *List:
		if len(x.items) == 0 {
			return append(b, "[]"...)
		}
		b = append(b, '[')
		for i, it := range x.items {
			if i > 0 {
				b = append(b, ',')
			}
			b = nl(level + 1)
			b = dumpsIndent2(b, it, level+1)
		}
		b = nl(level)
		return append(b, ']')
	case *Dict:
		if len(x.keys) == 0 {
			return append(b, "{}"...)
		}
		b = append(b, '{')
		for i, k := range x.keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = nl(level + 1)
			b = encodeStr(b, k)
			b = append(b, ':', ' ')
			b = dumpsIndent2(b, x.m[k], level+1)
		}
		b = nl(level)
		return append(b, '}')
	}
	return encodeScalar(b, v)
}

// utf8Strict is str.encode("utf-8"): a lone surrogate raises.
func utf8Strict(s string) []byte {
	for i, r := range runes(s) {
		if isSurrogate(r) {
			die("UnicodeEncodeError", "'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", r, i)
		}
	}
	return []byte(s)
}

// fingerprint is the provenance a node may not lose silently: what it says, who decided, where
// it came from. sha256 of the sorted compact JSON, first 12 hex digits.
func fingerprint(node *Dict) string {
	src, ok := node.get("source").(*Dict)
	if !ok {
		src = newDict()
	}
	ks := newDict()
	for _, k := range []string{"session", "timestamp", "quote", "ref"} {
		ks.set(k, src.get(k))
	}
	keep := newDict()
	keep.set("id", node.get("id"))
	keep.set("title", node.get("title"))
	keep.set("decision", node.get("decision"))
	keep.set("decided_by", node.get("decided_by"))
	keep.set("source", ks)
	sum := sha256.Sum256(utf8Strict(string(dumpsCompactSorted(nil, keep))))
	return hex.EncodeToString(sum[:])[:12]
}

// sortStable sorts by less, stably (Python's sorted with a key).
func sortStable[T any](xs []T, less func(a, b T) bool) {
	sort.SliceStable(xs, func(i, j int) bool { return less(xs[i], xs[j]) })
}
