package pac

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The front half of the PAC interpreter: a lexer and parser for the part of JavaScript
// that proxy auto-config files use. pac_eval.go runs what this produces.

type jsTokKind uint8

const (
	jsEOF jsTokKind = iota
	jsNum
	jsStr
	jsName
	jsPunct
	jsRegex
)

type jsToken struct {
	kind  jsTokKind
	s     string  // name, punctuator, string value or regex source
	n     float64 // number value
	flags string  // regex flags
	nl    bool    // a line break comes before it (for automatic semicolons)
	line  int
}

// longest first, so ">>>=" wins over ">>" and ">"
var jsPuncts = []string{">>>=", "===", "!==", "**=", "<<=", ">>=", ">>>", "...", "=>", "==", "!=", "<=", ">=",
	"&&", "||", "??", "++", "--", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>", "**"}

// names that can't be variables
var jsReserved = map[string]bool{"break": true, "case": true, "catch": true, "class": true, "const": true,
	"continue": true, "debugger": true, "default": true, "delete": true, "do": true, "else": true, "export": true,
	"extends": true, "finally": true, "for": true, "function": true, "if": true, "import": true, "in": true,
	"instanceof": true, "new": true, "return": true, "super": true, "switch": true, "this": true, "throw": true,
	"try": true, "typeof": true, "var": true, "void": true, "while": true, "with": true, "yield": true,
	"true": true, "false": true, "null": true}

type jsSyntaxError struct{ msg string }

func (e jsSyntaxError) Error() string { return e.msg }

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isIdentRune(r rune, first bool) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || !first && unicode.IsDigit(r)
}

func lexJS(src string) ([]jsToken, error) {
	var toks []jsToken
	line, nl := 1, false
	fail := func(f string, a ...any) error {
		return jsSyntaxError{fmt.Sprintf("line %d: ", line) + fmt.Sprintf(f, a...)}
	}
	for i := 0; i < len(src); {
		c := src[i]
		r, size := rune(c), 1
		if c >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(src[i:])
		}
		switch {
		case c == '\n' || r == '\u2028' || r == '\u2029':
			line, nl, i = line+1, true, i+size
			continue
		case c == '\r': // a line break too (old Mac files), counted at its \n if one follows
			nl, i = true, i+1
			continue
		case unicode.IsSpace(r) || r == '\ufeff':
			i += size
			continue
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' && src[i] != '\r' {
				i++
			}
			continue
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fail("unterminated comment")
			}
			if k := strings.Count(src[i:i+2+end], "\n"); k > 0 {
				line, nl = line+k, true
			}
			i += end + 4
			continue
		}
		t := jsToken{nl: nl, line: line}
		nl = false
		switch {
		case isDigitByte(c) || c == '.' && i+1 < len(src) && isDigitByte(src[i+1]):
			j := i
			if c == '0' && i+1 < len(src) && strings.IndexByte("xXoObB", src[i+1]) >= 0 {
				for j += 2; j < len(src) && (isDigitByte(src[j]) || isIdentRune(rune(src[j]), true)); j++ {
				}
				base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[src[i+1]|0x20]
				v, err := strconv.ParseUint(src[i+2:j], base, 64)
				if err != nil {
					return nil, fail("bad number %q", src[i:j])
				}
				t.n = float64(v)
			} else {
				for j < len(src) && (isDigitByte(src[j]) || src[j] == '.') {
					j++
				}
				if j < len(src) && src[j]|0x20 == 'e' {
					k := j + 1
					if k < len(src) && (src[k] == '+' || src[k] == '-') {
						k++
					}
					if k < len(src) && isDigitByte(src[k]) {
						for j = k; j < len(src) && isDigitByte(src[j]); j++ {
						}
					}
				}
				v, err := strconv.ParseFloat(src[i:j], 64)
				if err != nil && !errors.Is(err, strconv.ErrRange) {
					return nil, fail("bad number %q", src[i:j])
				}
				t.n = v
			}
			if j < len(src) && isIdentRune(rune(src[j]), true) {
				return nil, fail("bad number %q", src[i:j+1])
			}
			t.kind, i = jsNum, j
		case isIdentRune(r, true):
			j := i
			for j < len(src) {
				r, n := utf8.DecodeRuneInString(src[j:])
				if !isIdentRune(r, false) {
					break
				}
				j += n
			}
			t.kind, t.s, i = jsName, src[i:j], j
		case c == '"' || c == '\'':
			s, j, err := lexJSString(src, i)
			if err != nil {
				return nil, fail("%v", err)
			}
			line += strings.Count(src[i:j], "\n") // line continuations
			t.kind, t.s, i = jsStr, s, j
		case c == '`':
			return nil, fail("template strings are not supported")
		case c == '/' && jsRegexAllowed(toks):
			end, class := -1, false
		scan:
			for j := i + 1; j < len(src); j++ {
				switch src[j] {
				case '\\':
					j++
				case '[':
					class = true
				case ']':
					class = false
				case '\n':
					break scan
				case '/':
					if !class {
						end = j
						break scan
					}
				}
			}
			if end < 0 {
				return nil, fail("unterminated regular expression")
			}
			j := end + 1
			for j < len(src) && src[j] >= 'a' && src[j] <= 'z' {
				j++
			}
			t.kind, t.s, t.flags, i = jsRegex, src[i+1:end], src[end+1:j], j
		default:
			for _, p := range jsPuncts {
				if strings.HasPrefix(src[i:], p) {
					t.s = p
					break
				}
			}
			if t.s == "" {
				if !strings.ContainsRune("{}()[];,<>+-*/%&|^!~?:=.", r) {
					return nil, fail("unexpected character %q", r)
				}
				t.s = src[i : i+1]
			}
			t.kind, i = jsPunct, i+len(t.s)
		}
		toks = append(toks, t)
	}
	return append(toks, jsToken{kind: jsEOF, nl: true, line: line}), nil
}

// jsRegexAllowed tells a regex literal from a division: "/" starts a regex unless it
// follows something that ends an expression.
func jsRegexAllowed(toks []jsToken) bool {
	if len(toks) == 0 {
		return true
	}
	switch t := toks[len(toks)-1]; t.kind {
	case jsNum, jsStr, jsRegex:
		return false
	case jsName:
		return jsReserved[t.s] && t.s != "this" && t.s != "true" && t.s != "false" && t.s != "null"
	default:
		return t.s != ")" && t.s != "]" && t.s != "++" && t.s != "--"
	}
}

func lexJSString(src string, i int) (string, int, error) {
	q := src[i]
	var b strings.Builder
	for j := i + 1; j < len(src); {
		c := src[j]
		switch {
		case c == q:
			return b.String(), j + 1, nil
		case c == '\n' || c == '\r':
			return "", j, errors.New("unterminated string")
		case c != '\\':
			b.WriteByte(c)
			j++
			continue
		}
		if j++; j >= len(src) {
			break
		}
		e := src[j]
		j++
		switch e {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '0':
			b.WriteByte(0)
		case '\n': // line continuation
		case '\r':
			if j < len(src) && src[j] == '\n' {
				j++
			}
		case 'x', 'u':
			hex := ""
			switch {
			case e == 'u' && j < len(src) && src[j] == '{':
				if end := strings.IndexByte(src[j:], '}'); end > 0 {
					hex, j = src[j+1:j+end], j+end+1
				}
			case e == 'x' && j+2 <= len(src):
				hex, j = src[j:j+2], j+2
			case e == 'u' && j+4 <= len(src):
				hex, j = src[j:j+4], j+4
			}
			v, err := strconv.ParseUint(hex, 16, 32)
			if err != nil {
				return "", j, fmt.Errorf("bad \\%c escape", e)
			}
			b.WriteRune(rune(v))
		default:
			b.WriteByte(e)
		}
	}
	return "", len(src), errors.New("unterminated string")
}

// ---------------------------------------------------------------- syntax tree

type (
	jsExpr any
	jsStmt any
)

type (
	jsLit      struct{ v any }
	jsIdent    struct{ name string }
	jsArrayLit struct{ elems []jsExpr }
	jsFuncLit  struct{ f *jsFuncNode }
	jsUnary    struct {
		op string
		x  jsExpr
	}
	jsUpdate struct {
		op     string
		prefix bool
		x      jsExpr
	}
	jsBinary struct {
		op   string
		l, r jsExpr
	}
	jsLogic struct {
		op   string
		l, r jsExpr
	}
	jsCond   struct{ c, a, b jsExpr }
	jsAssign struct {
		op        string // "" for plain =, else the operator ("+" for +=)
		target, v jsExpr
	}
	jsSeq    struct{ list []jsExpr }
	jsMember struct {
		obj  jsExpr
		name string // a.name
		key  jsExpr // a[key], when set
	}
	jsCall struct {
		fn    jsExpr
		args  []jsExpr
		isNew bool
	}
	jsObjectLit struct {
		keys []string
		vals []jsExpr
	}
	jsRegexLit struct {
		src, flags string
		re         *regexp.Regexp
		err        error // Go's regexp can't do everything JavaScript's can; reported if it runs
	}
)

type (
	jsExprStmt struct{ x jsExpr }
	jsVar      struct {
		kind  string // var, let or const
		names []string
		inits []jsExpr
	}
	jsIf struct {
		c         jsExpr
		then, els jsStmt
	}
	jsFor struct {
		init    jsStmt
		c, post jsExpr
		body    jsStmt
	}
	jsForIn struct {
		kind, name string // kind is "" for a plain variable
		of         bool
		obj        jsExpr
		body       jsStmt
	}
	jsWhile struct {
		c    jsExpr
		body jsStmt
		do   bool
	}
	jsReturn   struct{ x jsExpr }
	jsBreak    struct{}
	jsContinue struct{}
	jsThrowS   struct{ x jsExpr }
	jsSwitch   struct {
		x      jsExpr
		tests  []jsExpr // nil for default
		bodies [][]jsStmt
		def    int // index of default, or -1
	}
	jsTry struct {
		body, catch, finally *jsBlock
		param                string
	}
)

// jsBlock is a list of statements plus the functions declared in it, which exist
// from the moment the block is entered.
type jsBlock struct {
	list  []jsStmt
	funcs []*jsFuncNode
}

type jsFuncNode struct {
	name     string
	params   []string
	body     *jsBlock
	expr     jsExpr   // the body of an arrow function written as an expression
	vars     []string // var declarations anywhere inside, hoisted to the top
	arrow    bool
	usesArgs bool // refers to "arguments"
}

// ---------------------------------------------------------------- parser

// deep enough for any real script, shallow enough to never threaten Go's stack
const jsMaxNesting = 500

type jsParser struct {
	toks  []jsToken
	pos   int
	fn    *jsFuncNode // the function being parsed
	prog  *jsFuncNode
	depth int
}

func parseJS(src string) (prog *jsFuncNode, err error) {
	toks, err := lexJS(src)
	if err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			se, ok := r.(jsSyntaxError)
			if !ok {
				se = jsSyntaxError{fmt.Sprint("parser failed: ", r)}
			}
			prog, err = nil, se
		}
	}()
	prog = &jsFuncNode{}
	p := &jsParser{toks: toks, fn: prog, prog: prog}
	prog.body = p.block(true)
	return prog, nil
}

func (p *jsParser) fail(f string, a ...any) {
	panic(jsSyntaxError{fmt.Sprintf("line %d: ", p.peek().line) + fmt.Sprintf(f, a...)})
}

func (p *jsParser) unexpected() {
	switch t := p.peek(); t.kind {
	case jsEOF:
		p.fail("unexpected end of script")
	case jsNum:
		p.fail("unexpected number")
	case jsStr:
		p.fail("unexpected string")
	default:
		p.fail("unexpected %q", t.s)
	}
}

func (p *jsParser) peek() jsToken { return p.toks[p.pos] }

func (p *jsParser) next() jsToken {
	t := p.toks[p.pos]
	if t.kind != jsEOF {
		p.pos++
	}
	return t
}

func (p *jsParser) is(s string) bool {
	t := p.toks[p.pos]
	return (t.kind == jsPunct || t.kind == jsName) && t.s == s
}

func (p *jsParser) eat(s string) bool {
	if p.is(s) {
		p.pos++
		return true
	}
	return false
}

func (p *jsParser) expect(s string) {
	if !p.eat(s) {
		if t := p.peek(); t.kind == jsEOF {
			p.fail("unexpected end of script (expected %q)", s)
		} else {
			p.fail("expected %q, found %q", s, t.s)
		}
	}
}

// semi ends a statement: a semicolon, or one JavaScript inserts automatically.
func (p *jsParser) semi() {
	if !p.eat(";") && !p.is("}") && !p.peek().nl {
		p.unexpected()
	}
}

func (p *jsParser) enter() {
	if p.depth++; p.depth > jsMaxNesting {
		p.fail("script is nested too deeply")
	}
}

func (p *jsParser) leave() { p.depth-- }

func (p *jsParser) ident() string {
	t := p.peek()
	if t.kind != jsName || jsReserved[t.s] {
		p.unexpected()
	}
	p.next()
	return t.s
}

// block parses statements up to a closing brace (or the end of the script, at top level).
func (p *jsParser) block(top bool) *jsBlock {
	b := &jsBlock{}
	for {
		if p.peek().kind == jsEOF {
			if !top {
				p.fail("missing }")
			}
			return b
		}
		if !top && p.eat("}") {
			return b
		}
		if s := p.statement(b); s != nil {
			b.list = append(b.list, s)
		}
	}
}

func (p *jsParser) statement(b *jsBlock) jsStmt {
	p.enter()
	defer p.leave()
	t := p.peek()
	switch {
	case t.kind == jsPunct && t.s == "{":
		p.next()
		return p.block(false)
	case t.kind == jsPunct && t.s == ";":
		p.next()
		return nil
	case t.kind != jsName:
	case t.s == "var" || t.s == "let" || t.s == "const":
		p.next()
		s := p.varDecl(t.s, "")
		p.semi()
		return s
	case t.s == "function":
		p.next()
		b.funcs = append(b.funcs, p.function(true))
		return nil
	case t.s == "if":
		p.next()
		s := &jsIf{c: p.paren(), then: p.statement(b)}
		if p.eat("else") {
			s.els = p.statement(b)
		}
		return s
	case t.s == "for":
		p.next()
		return p.forStatement(b)
	case t.s == "while":
		p.next()
		c := p.paren()
		return &jsWhile{c: c, body: p.statement(b)}
	case t.s == "do":
		p.next()
		body := p.statement(b)
		p.expect("while")
		c := p.paren()
		p.eat(";")
		return &jsWhile{c: c, body: body, do: true}
	case t.s == "return":
		if p.fn == p.prog {
			p.fail("return outside a function")
		}
		p.next()
		s := &jsReturn{}
		if n := p.peek(); !n.nl && !p.is(";") && !p.is("}") {
			s.x = p.expr()
		}
		p.semi()
		return s
	case t.s == "break" || t.s == "continue":
		p.next()
		if n := p.peek(); n.kind == jsName && !n.nl {
			p.fail("labels are not supported")
		}
		p.semi()
		if t.s == "break" {
			return jsBreak{}
		}
		return jsContinue{}
	case t.s == "throw":
		p.next()
		s := &jsThrowS{p.expr()}
		p.semi()
		return s
	case t.s == "switch":
		p.next()
		return p.switchStatement(b)
	case t.s == "try":
		p.next()
		return p.tryStatement()
	case !jsReserved[t.s] && p.toks[p.pos+1].kind == jsPunct && p.toks[p.pos+1].s == ":":
		p.fail("labels are not supported")
	}
	x := p.expr()
	p.semi()
	return &jsExprStmt{x}
}

func (p *jsParser) paren() jsExpr {
	p.expect("(")
	x := p.expr()
	p.expect(")")
	return x
}

// varDecl parses "a = 1, b" after var/let/const; first is a name already read, if any.
func (p *jsParser) varDecl(kind, first string) *jsVar {
	s := &jsVar{kind: kind}
	for {
		name := first
		if name == "" {
			name = p.ident()
		}
		first = ""
		var init jsExpr
		if p.eat("=") {
			init = p.assign()
		}
		if kind == "var" {
			p.fn.vars = append(p.fn.vars, name)
		}
		s.names, s.inits = append(s.names, name), append(s.inits, init)
		if !p.eat(",") {
			return s
		}
	}
}

func (p *jsParser) forStatement(b *jsBlock) jsStmt {
	p.expect("(")
	var init jsStmt
	switch {
	case p.is("var") || p.is("let") || p.is("const"):
		kind := p.next().s
		name := p.ident()
		if p.is("in") || p.is("of") {
			return p.forIn(b, kind, name)
		}
		init = p.varDecl(kind, name)
	case !p.is(";"):
		x := p.expr()
		if id, ok := x.(*jsIdent); ok && (p.is("in") || p.is("of")) {
			return p.forIn(b, "", id.name)
		}
		init = &jsExprStmt{x}
	}
	s := &jsFor{init: init}
	p.expect(";")
	if !p.is(";") {
		s.c = p.expr()
	}
	p.expect(";")
	if !p.is(")") {
		s.post = p.expr()
	}
	p.expect(")")
	s.body = p.statement(b)
	return s
}

func (p *jsParser) forIn(b *jsBlock, kind, name string) jsStmt {
	s := &jsForIn{kind: kind, name: name, of: p.next().s == "of"}
	if kind == "var" {
		p.fn.vars = append(p.fn.vars, name)
	}
	s.obj = p.expr()
	p.expect(")")
	s.body = p.statement(b)
	return s
}

func (p *jsParser) switchStatement(b *jsBlock) jsStmt {
	s := &jsSwitch{x: p.paren(), def: -1}
	p.expect("{")
	for !p.eat("}") {
		var test jsExpr
		if p.eat("default") {
			s.def = len(s.tests)
		} else {
			p.expect("case")
			test = p.expr()
		}
		p.expect(":")
		var body []jsStmt
		for !p.is("case") && !p.is("default") && !p.is("}") {
			if p.peek().kind == jsEOF {
				p.fail("missing }")
			}
			if st := p.statement(b); st != nil {
				body = append(body, st)
			}
		}
		s.tests, s.bodies = append(s.tests, test), append(s.bodies, body)
	}
	return s
}

func (p *jsParser) tryStatement() jsStmt {
	s := &jsTry{}
	p.expect("{")
	s.body = p.block(false)
	if p.eat("catch") {
		if p.eat("(") {
			s.param = p.ident()
			p.expect(")")
		}
		p.expect("{")
		s.catch = p.block(false)
	}
	if p.eat("finally") {
		p.expect("{")
		s.finally = p.block(false)
	}
	if s.catch == nil && s.finally == nil {
		p.fail("try without catch or finally")
	}
	return s
}

func (p *jsParser) function(decl bool) *jsFuncNode {
	f := &jsFuncNode{}
	if p.peek().kind == jsName {
		f.name = p.ident()
	} else if decl {
		p.fail("function name missing")
	}
	p.expect("(")
	f.params = p.params()
	p.funcBody(f)
	return f
}

// params reads a parameter list after its "(".
func (p *jsParser) params() []string {
	var ps []string
	for !p.eat(")") {
		ps = append(ps, p.ident())
		if !p.is(")") {
			p.expect(",")
		}
	}
	return ps
}

func (p *jsParser) funcBody(f *jsFuncNode) {
	outer := p.fn
	p.fn = f
	p.expect("{")
	f.body = p.block(false)
	p.fn = outer
}

// ---------------------------------------------------------------- expressions

var jsAssignOps = map[string]string{"=": "", "+=": "+", "-=": "-", "*=": "*", "/=": "/", "%=": "%", "**=": "**",
	"<<=": "<<", ">>=": ">>", ">>>=": ">>>", "&=": "&", "|=": "|", "^=": "^"}

var jsBinPrec = map[string]int{"??": 1, "||": 2, "&&": 3, "|": 4, "^": 5, "&": 6, "==": 7, "!=": 7, "===": 7,
	"!==": 7, "<": 8, ">": 8, "<=": 8, ">=": 8, "<<": 9, ">>": 9, ">>>": 9, "+": 10, "-": 10, "*": 11, "/": 11,
	"%": 11, "**": 12}

func (p *jsParser) expr() jsExpr {
	x := p.assign()
	if !p.is(",") {
		return x
	}
	seq := &jsSeq{list: []jsExpr{x}}
	for p.eat(",") {
		seq.list = append(seq.list, p.assign())
	}
	return seq
}

func (p *jsParser) assign() jsExpr {
	p.enter()
	defer p.leave()
	if f := p.arrow(); f != nil {
		return &jsFuncLit{f}
	}
	x := p.conditional()
	if t := p.peek(); t.kind == jsPunct {
		if op, ok := jsAssignOps[t.s]; ok {
			p.target(x)
			p.next()
			return &jsAssign{op: op, target: x, v: p.assign()}
		}
	}
	return x
}

func (p *jsParser) target(x jsExpr) {
	switch x.(type) {
	case *jsIdent, *jsMember:
	default:
		p.fail("invalid assignment")
	}
}

// arrow parses an arrow function if one starts here: "x => …" or "(a, b) => …".
func (p *jsParser) arrow() *jsFuncNode {
	t, n := p.peek(), p.toks[min(p.pos+1, len(p.toks)-1)]
	f := &jsFuncNode{arrow: true}
	switch {
	case t.kind == jsName && !jsReserved[t.s] && n.kind == jsPunct && n.s == "=>":
		f.params = []string{t.s}
		p.pos += 2
	case t.kind == jsPunct && t.s == "(":
		depth, i := 0, p.pos
		for ; p.toks[i].kind != jsEOF; i++ {
			if k := p.toks[i]; k.kind == jsPunct {
				switch k.s {
				case "(", "[", "{":
					depth++
				case ")", "]", "}":
					depth--
				}
			}
			if depth == 0 {
				break
			}
		}
		if after := p.toks[min(i+1, len(p.toks)-1)]; after.kind != jsPunct || after.s != "=>" {
			return nil
		}
		p.next()
		f.params = p.params()
		p.expect("=>")
	default:
		return nil
	}
	if p.is("{") {
		p.funcBody(f)
	} else {
		outer := p.fn
		p.fn = f
		f.expr = p.assign()
		p.fn = outer
	}
	return f
}

func (p *jsParser) conditional() jsExpr {
	c := p.binary(1)
	if !p.eat("?") {
		return c
	}
	a := p.assign()
	p.expect(":")
	return &jsCond{c, a, p.assign()}
}

func (p *jsParser) binary(min int) jsExpr {
	p.enter()
	defer p.leave()
	x := p.unary()
	for {
		t := p.peek()
		prec := jsBinPrec[t.s]
		if t.kind != jsPunct || prec == 0 || prec < min {
			return x
		}
		p.next()
		next := prec + 1
		if t.s == "**" {
			next = prec // right-associative
		}
		y := p.binary(next)
		if t.s == "&&" || t.s == "||" || t.s == "??" {
			x = &jsLogic{t.s, x, y}
		} else {
			x = &jsBinary{t.s, x, y}
		}
	}
}

func (p *jsParser) unary() jsExpr {
	p.enter()
	defer p.leave()
	t := p.peek()
	switch {
	case t.kind == jsPunct && (t.s == "!" || t.s == "-" || t.s == "+" || t.s == "~"),
		t.kind == jsName && (t.s == "typeof" || t.s == "void" || t.s == "delete"):
		p.next()
		return &jsUnary{t.s, p.unary()}
	case t.kind == jsPunct && (t.s == "++" || t.s == "--"):
		p.next()
		x := p.unary()
		p.target(x)
		return &jsUpdate{op: t.s, prefix: true, x: x}
	}
	x := p.call()
	if t := p.peek(); t.kind == jsPunct && (t.s == "++" || t.s == "--") && !t.nl {
		p.target(x)
		p.next()
		return &jsUpdate{op: t.s, x: x}
	}
	return x
}

func (p *jsParser) call() jsExpr {
	var x jsExpr
	if p.eat("new") {
		x = p.primary()
		for { // new a.B(…): the member accesses belong to the constructor
			m, ok := p.member(x)
			if !ok {
				break
			}
			x = m
		}
		c := &jsCall{fn: x, isNew: true}
		if p.is("(") {
			c.args = p.args()
		}
		x = c
	} else {
		x = p.primary()
	}
	for {
		if m, ok := p.member(x); ok {
			x = m
		} else if p.is("(") {
			x = &jsCall{fn: x, args: p.args()}
		} else {
			return x
		}
	}
}

func (p *jsParser) member(x jsExpr) (jsExpr, bool) {
	switch {
	case p.eat("."):
		t := p.peek()
		if t.kind != jsName {
			p.unexpected()
		}
		p.next()
		return &jsMember{obj: x, name: t.s}, true
	case p.eat("["):
		k := p.expr()
		p.expect("]")
		return &jsMember{obj: x, key: k}, true
	}
	return nil, false
}

func (p *jsParser) args() []jsExpr {
	p.expect("(")
	var list []jsExpr
	for !p.eat(")") {
		list = append(list, p.assign())
		if !p.is(")") {
			p.expect(",")
		}
	}
	return list
}

func (p *jsParser) primary() jsExpr {
	t := p.peek()
	switch t.kind {
	case jsNum:
		p.next()
		return &jsLit{t.n}
	case jsStr:
		p.next()
		return &jsLit{t.s}
	case jsRegex:
		p.next()
		re, err := jsCompileRegex(t.s, t.flags)
		return &jsRegexLit{t.s, t.flags, re, err}
	case jsName:
		switch t.s {
		case "true", "false":
			p.next()
			return &jsLit{t.s == "true"}
		case "null":
			p.next()
			return &jsLit{nil}
		case "this":
			p.next()
			return &jsLit{jsUndefined}
		case "function":
			p.next()
			return &jsFuncLit{p.function(false)}
		case "arguments":
			p.fn.usesArgs = true
		}
		return &jsIdent{p.ident()}
	case jsPunct:
		switch t.s {
		case "(":
			p.next()
			x := p.expr()
			p.expect(")")
			return x
		case "[":
			p.next()
			a := &jsArrayLit{}
			for !p.eat("]") {
				if p.eat(",") { // a hole
					a.elems = append(a.elems, &jsLit{jsUndefined})
					continue
				}
				a.elems = append(a.elems, p.assign())
				if !p.is("]") {
					p.expect(",")
				}
			}
			return a
		case "{":
			p.next()
			return p.objectLit()
		}
	}
	p.unexpected()
	return nil
}

func (p *jsParser) objectLit() jsExpr {
	o := &jsObjectLit{}
	for !p.eat("}") {
		t := p.peek()
		var key string
		switch t.kind {
		case jsName, jsStr:
			key = t.s
		case jsNum:
			key = jsNumStr(t.n)
		default:
			p.unexpected()
		}
		p.next()
		if t.kind == jsName && (p.is(",") || p.is("}")) { // shorthand {a}
			o.vals = append(o.vals, &jsIdent{t.s})
		} else {
			p.expect(":")
			o.vals = append(o.vals, p.assign())
		}
		o.keys = append(o.keys, key)
		if !p.is("}") {
			p.expect(",")
		}
	}
	return o
}
