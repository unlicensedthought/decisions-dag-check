package main

// The credential-shape scan over the raw document text: check.py's CRED_PATTERNS, each matched
// the way CPython's re.finditer does it (leftmost, non-overlapping, greedy with backtracking,
// Unicode \b and \s, re.I folding). RE2 has no lookarounds and an ASCII \b, so every pattern is
// a small hand matcher; the regex each one implements is quoted above it.

type matcher func(rs []rune, i int) (end int, ok bool)

type credPattern struct {
	name  string
	match matcher
}

func inClass(r rune, set string, ranges ...[2]rune) bool {
	for _, c := range set {
		if r == c {
			return true
		}
	}
	for _, rg := range ranges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

var (
	rUpper = [2]rune{'A', 'Z'}
	rLower = [2]rune{'a', 'z'}
	rDigit = [2]rune{'0', '9'}
	rHexLo = [2]rune{'a', 'f'}
	rHexUp = [2]rune{'A', 'F'}
)

// boundary is \b at i.
func boundary(rs []rune, i int) bool {
	a := i > 0 && isWord(rs[i-1])
	b := i < len(rs) && isWord(rs[i])
	return a != b
}

func lit(rs []rune, i int, w string) bool {
	for _, c := range w {
		if i >= len(rs) || rs[i] != c {
			return false
		}
		i++
	}
	return true
}

// span counts the characters from i that satisfy in.
func span(rs []rune, i int, in func(rune) bool) int {
	n := 0
	for i+n < len(rs) && in(rs[i+n]) {
		n++
	}
	return n
}

// prefixRun matches \b<lit><class>{min,} (greedy; nothing follows, so the longest run wins).
func prefixRun(prefix func(rs []rune, i int) int, min int, in func(rune) bool) matcher {
	return func(rs []rune, i int) (int, bool) {
		if !boundary(rs, i) {
			return 0, false
		}
		j := prefix(rs, i)
		if j < 0 {
			return 0, false
		}
		n := span(rs, j, in)
		if n < min {
			return 0, false
		}
		return j + n, true
	}
}

func litPrefix(w string) func([]rune, int) int {
	return func(rs []rune, i int) int {
		if !lit(rs, i, w) {
			return -1
		}
		return i + len([]rune(w))
	}
}

// fold is re.I for the letters check.py's case-insensitive pattern uses: CPython 3.9 matches
// a letter against its ASCII case pair plus U+0130/U+0131 for i, U+017F for s, U+212A for k
// (measured over every code point, see main_test.go).
func fold(r, want rune) bool {
	if r == want || r == want-'a'+'A' {
		return true
	}
	switch want {
	case 'i':
		return r == 0x130 || r == 0x131
	case 's':
		return r == 0x17f
	case 'k':
		return r == 0x212a
	}
	return false
}

func foldLit(rs []rune, i int, w string) bool {
	for _, c := range w {
		if i >= len(rs) {
			return false
		}
		if c >= 'a' && c <= 'z' {
			if !fold(rs[i], c) {
				return false
			}
		} else if rs[i] != c {
			return false
		}
		i++
	}
	return true
}

// keywordEnds lists, in backtracking order, where (pass(?:word|wd)?|secret|token|api[_-]?key|
// sudo[_-]?pw) can end when it starts at i.
func keywordEnds(rs []rune, i int) []int {
	var ends []int
	if foldLit(rs, i, "pass") {
		if foldLit(rs, i+4, "word") {
			ends = append(ends, i+8)
		}
		if foldLit(rs, i+4, "wd") {
			ends = append(ends, i+6)
		}
		ends = append(ends, i+4)
	}
	if foldLit(rs, i, "secret") {
		ends = append(ends, i+6)
	}
	if foldLit(rs, i, "token") {
		ends = append(ends, i+5)
	}
	opt := func(head, tail string) {
		if !foldLit(rs, i, head) {
			return
		}
		j := i + len(head)
		if j < len(rs) && (rs[j] == '_' || rs[j] == '-') && foldLit(rs, j+1, tail) {
			ends = append(ends, j+1+len(tail))
		}
		if foldLit(rs, j, tail) {
			ends = append(ends, j+len(tail))
		}
	}
	opt("api", "key")
	opt("sudo", "pw")
	return ends
}

// assignedSecret is
// (?i)\b(<keyword>)\b\s*[:=]\s*["']?(?!\[REDACTED)[^\s"',;]{6,}
func assignedSecret(rs []rune, i int) (int, bool) {
	if !boundary(rs, i) {
		return 0, false
	}
	value := func(r rune) bool { return !isSpace(r) && r != '"' && r != '\'' && r != ',' && r != ';' }
	for _, k := range keywordEnds(rs, i) {
		if !boundary(rs, k) {
			continue
		}
		j := k + span(rs, k, isSpace)
		if j >= len(rs) || (rs[j] != ':' && rs[j] != '=') {
			continue
		}
		j++
		j += span(rs, j, isSpace)
		// ["']? then the lookahead then the value. Without the quote the value would start at the
		// quote, which the value class excludes, so only one branch can succeed.
		if j < len(rs) && (rs[j] == '"' || rs[j] == '\'') {
			j++
		}
		if foldLit(rs, j, "[redacted") {
			continue
		}
		if n := span(rs, j, value); n >= 6 {
			return j + n, true
		}
	}
	return 0, false
}

// privateKey is -----BEGIN [A-Z ]*PRIVATE KEY-----
func privateKey(rs []rune, i int) (int, bool) {
	if !lit(rs, i, "-----BEGIN ") {
		return 0, false
	}
	j := i + 11
	for k := span(rs, j, func(r rune) bool { return inClass(r, " ", rUpper) }); k >= 0; k-- {
		if lit(rs, j+k, "PRIVATE KEY-----") {
			return j + k + 16, true
		}
	}
	return 0, false
}

// bearer is \bBearer\s+[A-Za-z0-9._~+/=-]{20,}
func bearer(rs []rune, i int) (int, bool) {
	if !boundary(rs, i) || !lit(rs, i, "Bearer") {
		return 0, false
	}
	j := i + 6
	sp := span(rs, j, isSpace)
	if sp == 0 {
		return 0, false
	}
	j += sp
	n := span(rs, j, func(r rune) bool { return inClass(r, "._~+/=-", rUpper, rLower, rDigit) })
	if n < 20 {
		return 0, false
	}
	return j + n, true
}

// awsKey is \bAKIA[0-9A-Z]{16}\b
func awsKey(rs []rune, i int) (int, bool) {
	if !boundary(rs, i) || !lit(rs, i, "AKIA") {
		return 0, false
	}
	if span(rs, i+4, func(r rune) bool { return inClass(r, "", rDigit, rUpper) }) < 16 || !boundary(rs, i+20) {
		return 0, false
	}
	return i + 20, true
}

// longHex is \b[A-Fa-f0-9]{32,}\b: only the full run can end on a boundary.
func longHex(rs []rune, i int) (int, bool) {
	if !boundary(rs, i) {
		return 0, false
	}
	n := span(rs, i, func(r rune) bool { return inClass(r, "", rHexUp, rHexLo, rDigit) })
	if n < 32 || !boundary(rs, i+n) {
		return 0, false
	}
	return i + n, true
}

// longOpaque is (?<![A-Za-z0-9/._-])[A-Za-z0-9+/_]{40,}={0,2}(?![A-Za-z0-9/._-])
func longOpaque(rs []rune, i int) (int, bool) {
	guard := func(r rune) bool { return inClass(r, "/._-", rUpper, rLower, rDigit) }
	if i > 0 && guard(rs[i-1]) {
		return 0, false
	}
	n := span(rs, i, func(r rune) bool { return inClass(r, "+/_", rUpper, rLower, rDigit) })
	for l := n; l >= 40; l-- {
		eqs := span(rs, i+l, func(r rune) bool { return r == '=' })
		if eqs > 2 {
			eqs = 2
		}
		for e := eqs; e >= 0; e-- {
			p := i + l + e
			if p == len(rs) || !guard(rs[p]) {
				return p, true
			}
		}
	}
	return 0, false
}

var credPatterns = []credPattern{
	{"api key (sk-)", prefixRun(litPrefix("sk-"), 16, func(r rune) bool { return inClass(r, "_-", rUpper, rLower, rDigit) })},
	{"aws access key", awsKey},
	{"github token", prefixRun(func(rs []rune, i int) int {
		if lit(rs, i, "gh") && i+3 < len(rs) && inClass(rs[i+2], "pousr") && rs[i+3] == '_' {
			return i + 4
		}
		return -1
	}, 20, func(r rune) bool { return inClass(r, "", rUpper, rLower, rDigit) })},
	{"github fine-grained token", prefixRun(litPrefix("github_pat_"), 20, func(r rune) bool { return inClass(r, "_", rUpper, rLower, rDigit) })},
	{"slack token", prefixRun(func(rs []rune, i int) int {
		if lit(rs, i, "xox") && i+4 < len(rs) && inClass(rs[i+3], "abprs") && rs[i+4] == '-' {
			return i + 5
		}
		return -1
	}, 10, func(r rune) bool { return inClass(r, "-", rUpper, rLower, rDigit) })},
	{"private key block", privateKey},
	{"bearer token", bearer},
	{"assigned secret", assignedSecret},
	{"long hex string", longHex},
	{"long opaque string", longOpaque},
}

// checkText is check_text(raw).
func checkText(raw string) []string {
	rs := runes(raw)
	var problems []string
	for _, p := range credPatterns {
		for i := 0; i <= len(rs); {
			end, ok := p.match(rs, i)
			if !ok {
				i++
				continue
			}
			head := rs[i:end]
			if len(head) > 6 {
				head = head[:6]
			}
			problems = append(problems, "credential-like string ("+p.name+") near "+reprStr(fromRunes(head)+"..."))
			i = end
		}
	}
	return problems
}
