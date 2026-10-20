package pac

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The back half of the PAC interpreter: runs the syntax tree from pac_parse.go.
//
// JavaScript values are Go values: nil is null, jsUndefined is undefined, and booleans,
// numbers (float64) and strings are themselves. Everything else is a pointer, so ==
// between two values is JavaScript's ===.

type jsUndef struct{}

var jsUndefined = jsUndef{}

type (
	jsArray  struct{ a []any }
	jsObject struct {
		keys []string // in insertion order, for for…in
		m    map[string]any
	}
	jsNS      struct{ m map[string]any } // a read-only built-in object (Math, console)
	jsClosure struct {
		f   *jsFuncNode
		env *jsScope
	}
	jsNative struct {
		name string
		call func(vm *pacVM, args []any) any
		ctor func(vm *pacVM, args []any) any // for the few built-ins that work with new
	}
	jsRegexp struct {
		re         *regexp.Regexp
		src, flags string
	}
	jsDate struct{ t time.Time }
)

type jsScope struct {
	vars   map[string]any
	parent *jsScope
}

func (s *jsScope) declare(name string, v any) {
	if s.vars == nil {
		s.vars = map[string]any{}
	}
	s.vars[name] = v
}

// jsThrow is a JavaScript exception on its way to a catch block.
type jsThrow struct{ v any }

// pacAbort stops a script for good (catch blocks don't see it): it ran too long,
// recursed too deeply or built something too big.
type pacAbort struct{ err error }

// Limits that keep a broken or hostile PAC script from hanging or exhausting Zipline.
const (
	pacMaxSteps = 1_000_000
	pacMaxDepth = 200
	pacMaxTime  = 10 * time.Second
	pacMaxStr   = 4 << 20 // bytes in one string
	pacMaxArray = 100_000 // elements in one array or keys in one object
)

var (
	errPACSteps = errors.New("PAC script ran too long (endless loop?)")
	errPACTime  = errors.New("PAC script took too long")
	errPACDepth = errors.New("PAC script recursed too deeply")
	errPACSize  = errors.New("PAC script built a value that is too big")
)

func jsCheckLen(n int) {
	if n > pacMaxStr {
		panic(pacAbort{errPACSize})
	}
}

func jsCheckArray(n int) {
	if n > pacMaxArray {
		panic(pacAbort{errPACSize})
	}
}

// pacBuiltins holds the global functions and objects; scripts read it but never write
// to it, so it is shared by every run. Filled in by pac.go.
var pacBuiltins *jsScope

type pacVM struct {
	global   *jsScope
	steps    int
	depth    int
	deadline time.Time
	now      time.Time // for the date and time functions
	localIPs []string  // myIpAddress, looked up once per run
	gotIPs   bool
}

func (vm *pacVM) step() {
	vm.steps++
	if vm.steps > pacMaxSteps {
		panic(pacAbort{errPACSteps})
	}
	if vm.steps&1023 == 0 && time.Now().After(vm.deadline) {
		panic(pacAbort{errPACTime})
	}
}

// throwf raises a JavaScript error, catchable by the script.
func (vm *pacVM) throwf(kind, f string, a ...any) {
	o := &jsObject{}
	o.set("name", kind)
	o.set("message", fmt.Sprintf(f, a...))
	panic(&jsThrow{o})
}

// catch runs f, returning the JavaScript exception it threw, if any.
func (vm *pacVM) catch(f func() (jsCtl, any)) (c jsCtl, v any, exc *jsThrow) {
	defer func() {
		if r := recover(); r != nil {
			t, ok := r.(*jsThrow)
			if !ok {
				panic(r)
			}
			exc = t
		}
	}()
	c, v = f()
	return
}

func (vm *pacVM) lookup(name string, s *jsScope) (any, bool) {
	for ; s != nil; s = s.parent {
		if v, ok := s.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func (vm *pacVM) assign(name string, v any, s *jsScope) {
	for ; s != nil && s != pacBuiltins; s = s.parent {
		if _, ok := s.vars[name]; ok {
			s.vars[name] = v
			return
		}
	}
	vm.global.declare(name, v) // undeclared, or a built-in's name: becomes a global
}

// ---------------------------------------------------------------- statements

type jsCtl uint8

const (
	ctlNone jsCtl = iota
	ctlBreak
	ctlContinue
	ctlReturn
)

func (vm *pacVM) execBlock(b *jsBlock, s *jsScope) (jsCtl, any) {
	for _, f := range b.funcs {
		s.declare(f.name, &jsClosure{f, s})
	}
	for _, st := range b.list {
		if c, v := vm.exec(st, s); c != ctlNone {
			return c, v
		}
	}
	return ctlNone, nil
}

func (vm *pacVM) exec(st jsStmt, s *jsScope) (jsCtl, any) {
	vm.step()
	switch st := st.(type) {
	case *jsExprStmt:
		vm.eval(st.x, s)
	case *jsVar:
		for i, name := range st.names {
			var v any = jsUndefined
			if st.inits[i] != nil {
				v = vm.eval(st.inits[i], s)
			} else if st.kind == "var" {
				continue // already declared, and "var x;" doesn't reset it
			}
			if st.kind == "var" {
				vm.assign(name, v, s)
			} else {
				s.declare(name, v)
			}
		}
	case *jsIf:
		if jsTruthy(vm.eval(st.c, s)) {
			return vm.exec(st.then, s)
		} else if st.els != nil {
			return vm.exec(st.els, s)
		}
	case *jsBlock:
		return vm.execBlock(st, &jsScope{parent: s})
	case *jsFor:
		ls := &jsScope{parent: s}
		if st.init != nil {
			vm.exec(st.init, ls)
		}
		for st.c == nil || jsTruthy(vm.eval(st.c, ls)) {
			if c, v := vm.exec(st.body, ls); c == ctlBreak {
				break
			} else if c == ctlReturn {
				return c, v
			}
			if st.post != nil {
				vm.eval(st.post, ls)
			}
		}
	case *jsForIn:
		for _, it := range vm.iterate(vm.eval(st.obj, s), st.of) {
			ls := &jsScope{parent: s}
			if st.kind == "let" || st.kind == "const" {
				ls.declare(st.name, it)
			} else {
				vm.assign(st.name, it, s)
			}
			if c, v := vm.exec(st.body, ls); c == ctlBreak {
				break
			} else if c == ctlReturn {
				return c, v
			}
		}
	case *jsWhile:
		for first := true; ; first = false {
			if !(st.do && first) && !jsTruthy(vm.eval(st.c, s)) {
				break
			}
			if c, v := vm.exec(st.body, s); c == ctlBreak {
				break
			} else if c == ctlReturn {
				return c, v
			}
		}
	case *jsReturn:
		if st.x == nil {
			return ctlReturn, jsUndefined
		}
		return ctlReturn, vm.eval(st.x, s)
	case jsBreak:
		return ctlBreak, nil
	case jsContinue:
		return ctlContinue, nil
	case *jsThrowS:
		panic(&jsThrow{vm.eval(st.x, s)})
	case *jsSwitch:
		v := vm.eval(st.x, s)
		start := st.def
		for i, t := range st.tests {
			if t != nil && v == vm.eval(t, s) {
				start = i
				break
			}
		}
		if start < 0 {
			break
		}
		ls := &jsScope{parent: s}
		for _, body := range st.bodies[start:] {
			for _, b := range body {
				if c, r := vm.exec(b, ls); c == ctlBreak {
					return ctlNone, nil
				} else if c != ctlNone {
					return c, r
				}
			}
		}
	case *jsTry:
		c, v, exc := vm.catch(func() (jsCtl, any) { return vm.execBlock(st.body, &jsScope{parent: s}) })
		if exc != nil && st.catch != nil {
			cs := &jsScope{parent: s}
			if st.param != "" {
				cs.declare(st.param, exc.v)
			}
			c, v, exc = vm.catch(func() (jsCtl, any) { return vm.execBlock(st.catch, cs) })
		}
		if st.finally != nil {
			if fc, fv := vm.execBlock(st.finally, &jsScope{parent: s}); fc != ctlNone {
				return fc, fv
			}
		}
		if exc != nil {
			panic(exc)
		}
		return c, v
	}
	return ctlNone, nil
}

// iterate lists what for…in (keys) or for…of (values) walks over.
func (vm *pacVM) iterate(o any, of bool) []any {
	var items []any
	switch o := o.(type) {
	case *jsArray:
		for i, v := range o.a {
			if of {
				items = append(items, v)
			} else {
				items = append(items, strconv.Itoa(i))
			}
		}
	case string:
		for i := range len(o) {
			if of {
				items = append(items, o[i:i+1])
			} else {
				items = append(items, strconv.Itoa(i))
			}
		}
	case *jsObject:
		if of {
			vm.throwf("TypeError", "object is not iterable")
		}
		for _, k := range o.keys {
			items = append(items, k)
		}
	default:
		if of {
			vm.throwf("TypeError", "%s is not iterable", jsToString(o))
		}
	}
	return items
}

// ---------------------------------------------------------------- expressions

func (vm *pacVM) eval(e jsExpr, s *jsScope) any {
	switch e := e.(type) {
	case *jsLit:
		return e.v
	case *jsIdent:
		v, ok := vm.lookup(e.name, s)
		if !ok {
			vm.throwf("ReferenceError", "%s is not defined", e.name)
		}
		return v
	case *jsRegexLit:
		if e.err != nil {
			vm.throwf("SyntaxError", "unsupported regular expression /%s/: %v", e.src, e.err)
		}
		return &jsRegexp{e.re, e.src, e.flags}
	case *jsArrayLit:
		a := make([]any, len(e.elems))
		for i, x := range e.elems {
			a[i] = vm.eval(x, s)
		}
		return &jsArray{a}
	case *jsObjectLit:
		o := &jsObject{}
		for i, k := range e.keys {
			o.set(k, vm.eval(e.vals[i], s))
		}
		return o
	case *jsFuncLit:
		c := &jsClosure{e.f, s}
		if e.f.name != "" { // a named function expression can call itself
			c.env = &jsScope{parent: s}
			c.env.declare(e.f.name, c)
		}
		return c
	case *jsUnary:
		return vm.unary(e, s)
	case *jsUpdate:
		old := jsToNumber(vm.eval(e.x, s))
		v := old + 1
		if e.op == "--" {
			v = old - 1
		}
		vm.store(e.x, v, s)
		if e.prefix {
			return v
		}
		return old
	case *jsBinary:
		return vm.binary(e.op, vm.eval(e.l, s), vm.eval(e.r, s))
	case *jsLogic:
		l := vm.eval(e.l, s)
		if e.op == "&&" && !jsTruthy(l) || e.op == "||" && jsTruthy(l) || e.op == "??" && l != nil && l != jsUndefined {
			return l
		}
		return vm.eval(e.r, s)
	case *jsCond:
		if jsTruthy(vm.eval(e.c, s)) {
			return vm.eval(e.a, s)
		}
		return vm.eval(e.b, s)
	case *jsAssign:
		var v any
		if e.op == "" {
			v = vm.eval(e.v, s)
		} else {
			v = vm.binary(e.op, vm.eval(e.target, s), vm.eval(e.v, s))
		}
		vm.store(e.target, v, s)
		return v
	case *jsSeq:
		var v any
		for _, x := range e.list {
			v = vm.eval(x, s)
		}
		return v
	case *jsMember:
		return vm.getProp(vm.eval(e.obj, s), vm.key(e, s))
	case *jsCall:
		fn := vm.eval(e.fn, s)
		args := make([]any, len(e.args))
		for i, a := range e.args {
			args[i] = vm.eval(a, s)
		}
		if e.isNew {
			vm.step()
			if f, ok := fn.(*jsNative); ok && f.ctor != nil {
				return f.ctor(vm, args)
			}
			vm.throwf("TypeError", "new %s is not supported", jsCallee(e.fn))
		}
		return vm.call(fn, args, jsCallee(e.fn))
	}
	panic(fmt.Sprintf("pac: unknown expression %T", e))
}

func (vm *pacVM) key(m *jsMember, s *jsScope) any {
	if m.key == nil {
		return m.name
	}
	return vm.eval(m.key, s)
}

func (vm *pacVM) store(target jsExpr, v any, s *jsScope) {
	switch t := target.(type) {
	case *jsIdent:
		vm.assign(t.name, v, s)
	case *jsMember:
		vm.setProp(vm.eval(t.obj, s), vm.key(t, s), v)
	}
}

// jsCallee names what's being called, for error messages.
func jsCallee(e jsExpr) string {
	switch e := e.(type) {
	case *jsIdent:
		return e.name
	case *jsMember:
		if e.key == nil {
			return jsCallee(e.obj) + "." + e.name
		}
		return jsCallee(e.obj) + "[…]"
	}
	return "expression"
}

func (vm *pacVM) unary(e *jsUnary, s *jsScope) any {
	switch e.op {
	case "typeof":
		if id, ok := e.x.(*jsIdent); ok { // typeof of an undeclared name is fine
			v, found := vm.lookup(id.name, s)
			if !found {
				return "undefined"
			}
			return jsTypeof(v)
		}
		return jsTypeof(vm.eval(e.x, s))
	case "delete":
		if m, ok := e.x.(*jsMember); ok {
			if o, ok := vm.eval(m.obj, s).(*jsObject); ok {
				o.delete(jsToString(vm.key(m, s)))
			}
		}
		return true
	}
	v := vm.eval(e.x, s)
	switch e.op {
	case "!":
		return !jsTruthy(v)
	case "-":
		return -jsToNumber(v)
	case "+":
		return jsToNumber(v)
	case "~":
		return float64(^jsToInt32(v))
	}
	return jsUndefined // void
}

func (vm *pacVM) binary(op string, a, b any) any {
	switch op {
	case "+":
		a, b = jsPrimitive(a, false), jsPrimitive(b, false)
		_, as := a.(string)
		_, bs := b.(string)
		if as || bs {
			x, y := jsToString(a), jsToString(b)
			jsCheckLen(len(x) + len(y))
			return x + y
		}
		return jsToNumber(a) + jsToNumber(b)
	case "-":
		return jsToNumber(a) - jsToNumber(b)
	case "*":
		return jsToNumber(a) * jsToNumber(b)
	case "/":
		return jsToNumber(a) / jsToNumber(b)
	case "%":
		return math.Mod(jsToNumber(a), jsToNumber(b))
	case "**":
		return math.Pow(jsToNumber(a), jsToNumber(b))
	case "==":
		return jsLooseEq(a, b)
	case "!=":
		return !jsLooseEq(a, b)
	case "===":
		return a == b
	case "!==":
		return a != b
	case "<", ">", "<=", ">=":
		return jsCompare(op, a, b)
	case "&":
		return float64(jsToInt32(a) & jsToInt32(b))
	case "|":
		return float64(jsToInt32(a) | jsToInt32(b))
	case "^":
		return float64(jsToInt32(a) ^ jsToInt32(b))
	case "<<":
		return float64(jsToInt32(a) << (jsToUint32(b) & 31))
	case ">>":
		return float64(jsToInt32(a) >> (jsToUint32(b) & 31))
	case ">>>":
		return float64(jsToUint32(a) >> (jsToUint32(b) & 31))
	}
	panic("pac: unknown operator " + op)
}

func (vm *pacVM) call(fn any, args []any, name string) any {
	vm.step()
	switch f := fn.(type) {
	case *jsNative:
		if f.call == nil {
			vm.throwf("TypeError", "%s needs new", name)
		}
		return f.call(vm, args)
	case *jsClosure:
		if vm.depth++; vm.depth > pacMaxDepth {
			panic(pacAbort{errPACDepth})
		}
		defer func() { vm.depth-- }()
		fn := f.f
		fs := &jsScope{vars: make(map[string]any, len(fn.params)+len(fn.vars)+1), parent: f.env}
		if fn.usesArgs && !fn.arrow {
			fs.vars["arguments"] = &jsArray{append([]any(nil), args...)}
		}
		for i, p := range fn.params {
			fs.vars[p] = jsArg(args, i)
		}
		for _, v := range fn.vars {
			if _, ok := fs.vars[v]; !ok {
				fs.vars[v] = jsUndefined
			}
		}
		if fn.body == nil {
			return vm.eval(fn.expr, fs)
		}
		if c, v := vm.execBlock(fn.body, fs); c == ctlReturn {
			return v
		}
		return jsUndefined
	}
	vm.throwf("TypeError", "%s is not a function", name)
	return nil
}

func jsArg(a []any, i int) any {
	if i < len(a) {
		return a[i]
	}
	return jsUndefined
}

// ---------------------------------------------------------------- properties

func (vm *pacVM) getProp(o, key any) any {
	switch o := o.(type) {
	case nil, jsUndef:
		vm.throwf("TypeError", "cannot read property %q of %s", jsToString(key), jsToString(o))
	case string:
		if i, ok := jsIndex(key); ok {
			if i < len(o) {
				return o[i : i+1]
			}
			return jsUndefined
		}
		k := jsToString(key)
		if k == "length" {
			return float64(len(o))
		}
		if f := jsStringMethod(o, k); f != nil {
			return f
		}
	case *jsArray:
		if i, ok := jsIndex(key); ok {
			if i < len(o.a) {
				return o.a[i]
			}
			return jsUndefined
		}
		k := jsToString(key)
		if k == "length" {
			return float64(len(o.a))
		}
		if f := jsArrayMethod(o, k); f != nil {
			return f
		}
	case *jsObject:
		k := jsToString(key)
		if v, ok := o.m[k]; ok {
			return v
		}
		if k == "hasOwnProperty" {
			return &jsNative{name: k, call: func(_ *pacVM, a []any) any {
				_, ok := o.m[jsToString(jsArg(a, 0))]
				return ok
			}}
		}
	case *jsNS:
		if v, ok := o.m[jsToString(key)]; ok {
			return v
		}
	case *jsRegexp:
		return jsRegexpProp(o, jsToString(key))
	case *jsDate:
		return jsDateProp(o, jsToString(key))
	case float64:
		return jsNumberProp(o, jsToString(key))
	case bool:
		if jsToString(key) == "toString" {
			return &jsNative{name: "toString", call: func(*pacVM, []any) any { return strconv.FormatBool(o) }}
		}
	}
	return jsUndefined
}

func (vm *pacVM) setProp(o, key, v any) {
	switch o := o.(type) {
	case nil, jsUndef:
		vm.throwf("TypeError", "cannot set property %q of %s", jsToString(key), jsToString(o))
	case *jsArray:
		if i, ok := jsIndex(key); ok {
			jsCheckArray(i + 1)
			for len(o.a) <= i {
				o.a = append(o.a, jsUndefined)
			}
			o.a[i] = v
		} else if jsToString(key) == "length" {
			if n := jsToNumber(v); n >= 0 && n <= pacMaxArray {
				for len(o.a) < int(n) {
					o.a = append(o.a, jsUndefined)
				}
				o.a = o.a[:int(n)]
			}
		}
	case *jsObject:
		o.set(jsToString(key), v)
	}
	// other values ignore property writes, as in non-strict JavaScript
}

func (o *jsObject) set(k string, v any) {
	if o.m == nil {
		o.m = map[string]any{}
	}
	if _, ok := o.m[k]; !ok {
		jsCheckArray(len(o.keys) + 1)
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

func (o *jsObject) delete(k string) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// jsIndex reports whether a property key is an array index.
func jsIndex(k any) (int, bool) {
	switch k := k.(type) {
	case float64:
		if k >= 0 && k < 1<<31 && k == math.Trunc(k) {
			return int(k), true
		}
	case string:
		if n, err := strconv.Atoi(k); err == nil && n >= 0 && strconv.Itoa(n) == k {
			return n, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------- conversions

func jsTruthy(v any) bool {
	switch v := v.(type) {
	case nil, jsUndef:
		return false
	case bool:
		return v
	case float64:
		return v != 0 && !math.IsNaN(v)
	case string:
		return v != ""
	}
	return true
}

func jsTypeof(v any) string {
	switch v.(type) {
	case jsUndef:
		return "undefined"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case *jsClosure, *jsNative:
		return "function"
	}
	return "object"
}

func jsIsObject(v any) bool {
	switch v.(type) {
	case nil, jsUndef, bool, float64, string:
		return false
	}
	return true
}

// jsPrimitive turns objects into strings (or, for dates compared as numbers, numbers).
func jsPrimitive(v any, number bool) any {
	if d, ok := v.(*jsDate); ok && number {
		return float64(d.t.UnixMilli())
	}
	if jsIsObject(v) {
		return jsToString(v)
	}
	return v
}

func jsLooseEq(a, b any) bool {
	an, bn := a == nil || a == jsUndefined, b == nil || b == jsUndefined
	if an || bn {
		return an && bn
	}
	if a == b {
		return true
	}
	if jsIsObject(a) && jsIsObject(b) {
		return false
	}
	a, b = jsPrimitive(a, false), jsPrimitive(b, false)
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return x == y
		}
	}
	return jsToNumber(a) == jsToNumber(b)
}

func jsCompare(op string, a, b any) bool {
	a, b = jsPrimitive(a, true), jsPrimitive(b, true)
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			switch op {
			case "<":
				return x < y
			case ">":
				return x > y
			case "<=":
				return x <= y
			}
			return x >= y
		}
	}
	x, y := jsToNumber(a), jsToNumber(b)
	switch op {
	case "<":
		return x < y
	case ">":
		return x > y
	case "<=":
		return x <= y
	}
	return x >= y
}

func jsToNumber(v any) float64 {
	switch v := v.(type) {
	case float64:
		return v
	case bool:
		if v {
			return 1
		}
		return 0
	case nil:
		return 0
	case string:
		return jsParseNumber(v)
	case *jsDate:
		return float64(v.t.UnixMilli())
	case *jsArray:
		return jsParseNumber(jsToString(v))
	}
	return math.NaN()
}

func jsParseNumber(s string) float64 {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return 0
	case len(s) > 2 && s[0] == '0' && s[1]|0x20 == 'x':
		if v, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
			return float64(v)
		}
		return math.NaN()
	case s == "Infinity" || s == "+Infinity":
		return math.Inf(1)
	case s == "-Infinity":
		return math.Inf(-1)
	case strings.ContainsAny(s, "_xXpPiInN"): // forms Go accepts but JavaScript doesn't ("inf", "0x1p3", "1_0")
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return f
}

func jsToUint32(v any) uint32 {
	f := jsToNumber(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return uint32(int64(math.Mod(math.Trunc(f), 1<<32)))
}

func jsToInt32(v any) int32 { return int32(jsToUint32(v)) }

// jsInt converts an index-like argument (NaN is 0), kept within a range int can hold anywhere.
func jsInt(v any) int {
	f := jsToNumber(v)
	switch {
	case math.IsNaN(f):
		return 0
	case f > 1<<30:
		return 1 << 30
	case f < -1<<30:
		return -1 << 30
	}
	return int(f)
}

func jsToString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return jsNumStr(v)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "null"
	case jsUndef:
		return "undefined"
	case *jsArray:
		return jsJoin(v, ",", 0)
	case *jsObject:
		name, ok := v.m["name"].(string)
		if msg, ok2 := v.m["message"].(string); ok && ok2 { // an error
			return name + ": " + msg
		}
		return "[object Object]"
	case *jsRegexp:
		return "/" + v.src + "/" + v.flags
	case *jsDate:
		return v.t.Format("Mon Jan 02 2006 15:04:05 GMT-0700")
	case *jsClosure, *jsNative:
		return "function"
	}
	return "[object Object]"
}

// jsNumStr formats a number the way JavaScript does.
func jsNumStr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	if a := math.Abs(f); a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	m, exp, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	return m + "e" + exp[:1] + strings.TrimLeft(exp[1:], "0") // e+21, not e+021
}

func jsJoin(a *jsArray, sep string, depth int) string {
	if depth > 8 { // arrays that contain themselves
		return ""
	}
	var b strings.Builder
	for i, x := range a.a {
		if i > 0 {
			b.WriteString(sep)
		}
		switch x := x.(type) {
		case nil, jsUndef:
		case *jsArray:
			b.WriteString(jsJoin(x, ",", depth+1))
		default:
			b.WriteString(jsToString(x))
		}
		jsCheckLen(b.Len())
	}
	return b.String()
}
