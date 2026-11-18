package main

// The value model: what CPython's json.loads hands check.py, and the handful of Python operations
// check.py performs on it. The port is byte-for-byte with the retired check.py, so these mirror
// CPython, including where it raises: a Python exception becomes a crash (exit 1).
//
//	None -> nil    bool -> bool    int -> *big.Int    float -> float64
//	str  -> string (WTF-8: a lone surrogate from a "\ud800" escape is kept as its 3-byte form)
//	list -> *List  dict -> *Dict (string keys, insertion order, a repeated key keeps its first
//	               position and its last value)

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// List is a Python list.
type List struct{ items []any }

// Dict is a Python dict with str keys (every dict json.loads builds).
type Dict struct {
	keys []string
	m    map[string]any
}

func newDict() *Dict { return &Dict{m: map[string]any{}} }

func (d *Dict) set(k string, v any) {
	if _, ok := d.m[k]; !ok {
		d.keys = append(d.keys, k)
	}
	d.m[k] = v
}

func (d *Dict) has(k string) bool { _, ok := d.m[k]; return ok }

// get is dict.get(k): nil (None) when absent.
func (d *Dict) get(k string) any { return d.m[k] }

// getOr is dict.get(k, def).
func (d *Dict) getOr(k string, def any) any {
	if v, ok := d.m[k]; ok {
		return v
	}
	return def
}

// crash is a Python exception check.py did not catch: the traceback, exit 1.
type crash struct{ typ, msg string }

func die(typ, format string, a ...any) { panic(crash{typ, fmt.Sprintf(format, a...)}) }

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case *List:
		return "list"
	case *Dict:
		return "dict"
	}
	panic(fmt.Sprintf("decisions-check: value of Go type %T", v))
}

// ---------------------------------------------------------------------------- strings ---

// runes decodes WTF-8: valid UTF-8 plus surrogate code points in their 3-byte form.
func runes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
			out = append(out, 0xD000|rune(s[i+1]&0x3F)<<6|rune(s[i+2]&0x3F))
			i += 3
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		out = append(out, r)
		i += n
	}
	return out
}

func isSurrogate(r rune) bool { return r >= 0xD800 && r <= 0xDFFF }

func appendRune(b []byte, r rune) []byte {
	if isSurrogate(r) {
		return append(b, 0xED, byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
	return utf8.AppendRune(b, r)
}

func fromRunes(rs []rune) string {
	b := make([]byte, 0, len(rs))
	for _, r := range rs {
		b = appendRune(b, r)
	}
	return string(b)
}

func hasSurrogate(s string) bool {
	for _, r := range runes(s) {
		if isSurrogate(r) {
			return true
		}
	}
	return false
}

// isWord is re's \w for str patterns: str.isalnum() or "_". str.isalpha() is the L categories;
// isdecimal/isdigit/isnumeric fall inside Nd, Nl and No (Go's tables are a newer Unicode than
// CPython 3.9's 13.0: they differ only on code points unassigned in 13.0, see main_test.go).
func isWord(r rune) bool {
	if r < 0x80 {
		return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}
	return r == '_' || unicode.IsLetter(r) || unicode.In(r, unicode.Nd, unicode.Nl, unicode.No)
}

// isSpace is str.isspace() and re's \s: White_Space plus the C0 separators \x1c-\x1f.
func isSpace(r rune) bool { return (r >= 0x1c && r <= 0x1f) || unicode.Is(unicode.White_Space, r) }

// isDecimal is re's \d for str patterns: category Nd.
func isDecimal(r rune) bool { return unicode.Is(unicode.Nd, r) }

// isPrintable is str.isprintable(): not C*, not Z* except the space.
func isPrintable(r rune) bool { return r == ' ' || (!isSurrogate(r) && unicode.IsPrint(r)) }

// ----------------------------------------------------------------------- repr and str ---

func reprStr(s string) string {
	rs := runes(s)
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	b := []byte{byte(quote)}
	for _, r := range rs {
		switch {
		case r == quote || r == '\\':
			b = append(b, '\\', byte(r))
		case r == '\t':
			b = append(b, '\\', 't')
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r < ' ' || r == 0x7f:
			b = fmt.Appendf(b, "\\x%02x", r)
		case r < 0x7f:
			b = append(b, byte(r))
		case isPrintable(r):
			b = utf8.AppendRune(b, r)
		case r <= 0xff:
			b = fmt.Appendf(b, "\\x%02x", r)
		case r <= 0xffff:
			b = fmt.Appendf(b, "\\u%04x", r)
		default:
			b = fmt.Appendf(b, "\\U%08x", r)
		}
	}
	return string(append(b, byte(quote)))
}

// floatRepr is float.__repr__: the shortest round-trip digits, positional when the decimal
// point position decpt satisfies -4 < decpt <= 16, else d.ddde+XX.
func floatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	mant, exps, _ := strings.Cut(s, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(exps)
	decpt := exp + 1
	if decpt > -4 && decpt <= 16 {
		switch {
		case decpt <= 0:
			return sign + "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			return sign + digits[:decpt] + "." + digits[decpt:]
		}
	}
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es, exp = "-", -exp
	}
	return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
}

func repr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case *big.Int:
		return x.String()
	case float64:
		return floatRepr(x)
	case string:
		return reprStr(x)
	case *List:
		parts := make([]string, len(x.items))
		for i, it := range x.items {
			parts[i] = repr(it)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Dict:
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = reprStr(k) + ": " + repr(x.m[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	panic(fmt.Sprintf("decisions-check: repr of Go type %T", v))
}

// str is Python's str(): the string itself, repr() for everything else.
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return repr(v)
}

// ------------------------------------------------------------------ truth and iteration ---

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case *big.Int:
		return x.Sign() != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case *List:
		return len(x.items) > 0
	case *Dict:
		return len(x.keys) > 0
	}
	return true
}

// iter is iter(v) / list(v): a str yields its characters, a dict its keys.
func iter(v any) []any {
	switch x := v.(type) {
	case *List:
		return x.items
	case string:
		rs := runes(x)
		out := make([]any, len(rs))
		for i, r := range rs {
			out[i] = fromRunes([]rune{r})
		}
		return out
	case *Dict:
		out := make([]any, len(x.keys))
		for i, k := range x.keys {
			out[i] = k
		}
		return out
	}
	die("TypeError", "'%s' object is not iterable", typeName(v))
	return nil
}

// iterOr is `for t in v or []`.
func iterOr(v any) []any {
	if !truthy(v) {
		return nil
	}
	return iter(v)
}

func pyLen(v any) int {
	switch x := v.(type) {
	case string:
		return len(runes(x))
	case *List:
		return len(x.items)
	case *Dict:
		return len(x.keys)
	}
	die("TypeError", "object of type '%s' has no len()", typeName(v))
	return 0
}

// sub is v[key] for a str key.
func sub(v any, key string) any {
	switch x := v.(type) {
	case *Dict:
		if !x.has(key) {
			die("KeyError", "%s", reprStr(key))
		}
		return x.m[key]
	case *List:
		die("TypeError", "list indices must be integers or slices, not str")
	case string:
		die("TypeError", "string indices must be integers")
	}
	die("TypeError", "'%s' object is not subscriptable", typeName(v))
	return nil
}

// dictGet is v.get(key) where check.py assumes v is a dict.
func dictGet(v any, key string) any {
	d, ok := v.(*Dict)
	if !ok {
		die("AttributeError", "'%s' object has no attribute 'get'", typeName(v))
	}
	return d.get(key)
}

// --------------------------------------------------------------- numbers and comparison ---

// num returns v as an exact rational (bool and int and finite float), or ok=false.
func num(v any) (r *big.Rat, f float64, isFloat, ok bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewRat(1, 1), 0, false, true
		}
		return new(big.Rat), 0, false, true
	case *big.Int:
		return new(big.Rat).SetInt(x), 0, false, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, x, true, true
		}
		r := new(big.Rat)
		r.SetFloat64(x)
		return r, x, true, true
	}
	return nil, 0, false, false
}

// numCmp compares two numbers exactly; nan reports unordered.
func numCmp(a, b any) (c int, unordered bool) {
	ra, fa, _, _ := num(a)
	rb, fb, _, _ := num(b)
	if ra == nil || rb == nil {
		if math.IsNaN(fa) || math.IsNaN(fb) {
			return 0, true
		}
		xa, xb := fa, fb
		if ra != nil {
			xa, _ = ra.Float64()
		}
		if rb != nil {
			xb, _ = rb.Float64()
		}
		switch {
		case xa < xb:
			return -1, false
		case xa > xb:
			return 1, false
		}
		return 0, false
	}
	return ra.Cmp(rb), false
}

func isNum(v any) bool { _, _, _, ok := num(v); return ok }

// eq is Python ==.
func eq(a, b any) bool {
	if isNum(a) && isNum(b) {
		c, un := numCmp(a, b)
		return !un && c == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case *List:
		y, ok := b.(*List)
		if !ok || len(x.items) != len(y.items) {
			return false
		}
		for i := range x.items {
			if !eq(x.items[i], y.items[i]) {
				return false
			}
		}
		return true
	case *Dict:
		y, ok := b.(*Dict)
		if !ok || len(x.keys) != len(y.keys) {
			return false
		}
		for _, k := range x.keys {
			if !y.has(k) || !eq(x.m[k], y.m[k]) {
				return false
			}
		}
		return true
	}
	return false
}

// less is Python a < b (op names the operator for the TypeError text).
func less(a, b any, op string) bool {
	if isNum(a) && isNum(b) {
		c, un := numCmp(a, b)
		return !un && c < 0
	}
	switch x := a.(type) {
	case string:
		if y, ok := b.(string); ok {
			return x < y // WTF-8 byte order is code point order
		}
	case *List:
		if y, ok := b.(*List); ok {
			for i := 0; i < len(x.items) && i < len(y.items); i++ {
				if !eq(x.items[i], y.items[i]) {
					return less(x.items[i], y.items[i], "<")
				}
			}
			return len(x.items) < len(y.items)
		}
	}
	die("TypeError", "'%s' not supported between instances of '%s' and '%s'", op, typeName(a), typeName(b))
	return false
}

func lt(a, b any) bool { return less(a, b, "<") }

// gt is a > b: CPython tries a.__gt__(b) then b.__lt__(a); for these types the outcome is b < a.
func gt(a, b any) bool {
	defer func() {
		if r := recover(); r != nil {
			if c, ok := r.(crash); ok {
				die(c.typ, "'>' not supported between instances of '%s' and '%s'", typeName(a), typeName(b))
			}
			panic(r)
		}
	}()
	return less(b, a, ">")
}

// --------------------------------------------------------------------- hashing and `in` ---

// hkey is the identity of a hashable value as a set or dict key: equal values (1, 1.0, True)
// share one key. A list or dict raises TypeError, as in CPython.
func hkey(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case string:
		return "S" + x
	case *List, *Dict:
		die("TypeError", "unhashable type: '%s'", typeName(v))
	}
	r, f, _, _ := num(v)
	if r == nil {
		return "F" + floatRepr(f)
	}
	if r.IsInt() {
		return "I" + r.Num().String()
	}
	return "R" + r.String()
}

// hset is a Python set.
type hset map[string]bool

func (s hset) add(v any)      { s[hkey(v)] = true }
func (s hset) has(v any) bool { return s[hkey(v)] }
func (s hset) equal(o hset) bool {
	if len(s) != len(o) {
		return false
	}
	for k := range s {
		if !o[k] {
			return false
		}
	}
	return true
}

// hdict is a Python dict with arbitrary hashable keys: insertion order, first key kept.
type hdict struct {
	order []string
	key   map[string]any
	val   map[string]any
}

func newHdict() *hdict { return &hdict{key: map[string]any{}, val: map[string]any{}} }

func (d *hdict) set(k, v any) {
	h := hkey(k)
	if _, ok := d.key[h]; !ok {
		d.order = append(d.order, h)
		d.key[h] = k
	}
	d.val[h] = v
}

// in is Python `needle in container`.
func in(needle, container any) bool {
	switch x := container.(type) {
	case string:
		s, ok := needle.(string)
		if !ok {
			die("TypeError", "'in <string>' requires string as left operand, not %s", typeName(needle))
		}
		return strings.Contains(x, s)
	case *List:
		for _, it := range x.items {
			if eq(needle, it) {
				return true
			}
		}
		return false
	case *Dict:
		h := hkey(needle)
		return h[0] == 'S' && x.has(h[1:])
	}
	die("TypeError", "argument of type '%s' is not iterable", typeName(container))
	return false
}

// sortedStrings is sorted() of str: code point order, which is WTF-8 byte order.
func sortedStrings(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}
