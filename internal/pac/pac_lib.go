package pac

import (
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The slice of JavaScript's standard library PAC scripts lean on: string and array
// methods, simple regular expressions, Math, Date, parseInt and friends.

func jsFn(name string, f func(vm *pacVM, a []any) any) *jsNative {
	return &jsNative{name: name, call: f}
}

// jsClamp is an index argument clamped to 0..n; undefined gives def.
func jsClamp(v any, n, def int) int {
	if v == jsUndefined {
		return def
	}
	return min(max(jsInt(v), 0), n)
}

// jsRel is a relative index argument (negative counts from the end) clamped to 0..n.
func jsRel(v any, n, def int) int {
	if v == jsUndefined {
		return def
	}
	i := jsInt(v)
	if i < 0 {
		i += n
	}
	return min(max(i, 0), n)
}

func jsStringMethod(s, name string) *jsNative {
	var f func(vm *pacVM, a []any) any
	n := len(s)
	switch name {
	case "toLowerCase", "toLocaleLowerCase":
		f = func(*pacVM, []any) any { return strings.ToLower(s) }
	case "toUpperCase", "toLocaleUpperCase":
		f = func(*pacVM, []any) any { return strings.ToUpper(s) }
	case "trim":
		f = func(*pacVM, []any) any { return strings.TrimSpace(s) }
	case "trimStart", "trimLeft":
		f = func(*pacVM, []any) any { return strings.TrimLeftFunc(s, unicode.IsSpace) }
	case "trimEnd", "trimRight":
		f = func(*pacVM, []any) any { return strings.TrimRightFunc(s, unicode.IsSpace) }
	case "toString", "valueOf":
		f = func(*pacVM, []any) any { return s }
	case "charAt":
		f = func(_ *pacVM, a []any) any {
			if i := jsInt(jsArg(a, 0)); i >= 0 && i < n {
				return s[i : i+1]
			}
			return ""
		}
	case "charCodeAt":
		f = func(_ *pacVM, a []any) any {
			if i := jsInt(jsArg(a, 0)); i >= 0 && i < n {
				return float64(s[i])
			}
			return math.NaN()
		}
	case "indexOf", "includes":
		f = func(_ *pacVM, a []any) any {
			from := jsClamp(jsArg(a, 1), n, 0)
			i := strings.Index(s[from:], jsToString(jsArg(a, 0)))
			if name == "includes" {
				return i >= 0
			}
			if i < 0 {
				return -1.0
			}
			return float64(i + from)
		}
	case "lastIndexOf":
		f = func(_ *pacVM, a []any) any {
			sub, end := jsToString(jsArg(a, 0)), n
			if p := jsArg(a, 1); p != jsUndefined && !math.IsNaN(jsToNumber(p)) {
				end = min(jsClamp(p, n, n)+len(sub), n)
			}
			return float64(strings.LastIndex(s[:end], sub))
		}
	case "startsWith":
		f = func(_ *pacVM, a []any) any {
			return strings.HasPrefix(s[jsClamp(jsArg(a, 1), n, 0):], jsToString(jsArg(a, 0)))
		}
	case "endsWith":
		f = func(_ *pacVM, a []any) any {
			return strings.HasSuffix(s[:jsClamp(jsArg(a, 1), n, n)], jsToString(jsArg(a, 0)))
		}
	case "substring":
		f = func(_ *pacVM, a []any) any {
			i, j := jsClamp(jsArg(a, 0), n, 0), jsClamp(jsArg(a, 1), n, n)
			return s[min(i, j):max(i, j)]
		}
	case "substr":
		f = func(_ *pacVM, a []any) any {
			i := jsRel(jsArg(a, 0), n, 0)
			return s[i : i+jsClamp(jsArg(a, 1), n-i, n-i)]
		}
	case "slice":
		f = func(_ *pacVM, a []any) any {
			i, j := jsRel(jsArg(a, 0), n, 0), jsRel(jsArg(a, 1), n, n)
			if i >= j {
				return ""
			}
			return s[i:j]
		}
	case "split":
		f = func(_ *pacVM, a []any) any {
			var parts []string
			switch sep := jsArg(a, 0).(type) {
			case jsUndef:
				parts = []string{s}
			case *jsRegexp:
				parts = sep.re.Split(s, -1)
			default:
				parts = strings.Split(s, jsToString(sep))
			}
			if lim := jsArg(a, 1); lim != jsUndefined {
				parts = parts[:jsClamp(lim, len(parts), len(parts))]
			}
			jsCheckArray(len(parts))
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return &jsArray{out}
		}
	case "concat":
		f = func(_ *pacVM, a []any) any {
			var b strings.Builder
			b.WriteString(s)
			for _, x := range a {
				b.WriteString(jsToString(x))
				jsCheckLen(b.Len())
			}
			return b.String()
		}
	case "repeat":
		f = func(vm *pacVM, a []any) any {
			k := jsInt(jsArg(a, 0))
			if k < 0 {
				vm.throwf("RangeError", "invalid count")
			}
			if n > 0 {
				jsCheckLen(n * min(k, pacMaxStr+1))
			}
			return strings.Repeat(s, k)
		}
	case "padStart", "padEnd":
		f = func(_ *pacVM, a []any) any {
			width, pad := jsInt(jsArg(a, 0)), " "
			if p := jsArg(a, 1); p != jsUndefined {
				pad = jsToString(p)
			}
			if width <= n || pad == "" {
				return s
			}
			jsCheckLen(width)
			fill := strings.Repeat(pad, (width-n)/len(pad)+1)[:width-n]
			if name == "padStart" {
				return fill + s
			}
			return s + fill
		}
	case "replace", "replaceAll":
		f = func(vm *pacVM, a []any) any { return vm.replace(s, jsArg(a, 0), jsArg(a, 1), name == "replaceAll") }
	case "match":
		f = func(vm *pacVM, a []any) any {
			re := vm.toRegexp(jsArg(a, 0))
			if !strings.Contains(re.flags, "g") {
				return jsExec(re, s)
			}
			all := re.re.FindAllString(s, -1)
			if all == nil {
				return nil
			}
			jsCheckArray(len(all))
			out := make([]any, len(all))
			for i, m := range all {
				out[i] = m
			}
			return &jsArray{out}
		}
	case "search":
		f = func(vm *pacVM, a []any) any {
			if loc := vm.toRegexp(jsArg(a, 0)).re.FindStringIndex(s); loc != nil {
				return float64(loc[0])
			}
			return -1.0
		}
	default:
		return nil
	}
	return jsFn(name, f)
}

// replace is String.prototype.replace (and replaceAll): a string pattern matches
// literally, a regex one as a regex; the replacement is a "$1"-style template or a function.
func (vm *pacVM) replace(s string, pat, repl any, all bool) string {
	var locs [][]int
	if re, ok := pat.(*jsRegexp); ok {
		if all || strings.Contains(re.flags, "g") {
			locs = re.re.FindAllStringSubmatchIndex(s, -1)
		} else if l := re.re.FindStringSubmatchIndex(s); l != nil {
			locs = [][]int{l}
		}
	} else {
		p := jsToString(pat)
		for from := 0; from <= len(s); {
			i := strings.Index(s[from:], p)
			if i < 0 {
				break
			}
			locs = append(locs, []int{from + i, from + i + len(p)})
			if !all {
				break
			}
			from += i + max(len(p), 1)
		}
	}
	fn := jsTypeof(repl) == "function"
	tmpl := jsToString(repl)
	var b strings.Builder
	last := 0
	for _, l := range locs {
		b.WriteString(s[last:l[0]])
		if fn {
			args := []any{s[l[0]:l[1]]}
			for g := 2; g+1 < len(l); g += 2 {
				if l[g] < 0 {
					args = append(args, jsUndefined)
				} else {
					args = append(args, s[l[g]:l[g+1]])
				}
			}
			b.WriteString(jsToString(vm.call(repl, append(args, float64(l[0]), s), "replacer")))
		} else {
			jsExpand(&b, tmpl, s, l)
		}
		jsCheckLen(b.Len())
		last = l[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// jsExpand writes a replacement template: $& is the match, $1…$99 groups, $` and $'
// the text before and after, $$ a dollar sign.
func jsExpand(b *strings.Builder, tmpl, s string, loc []int) {
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		if c != '$' || i+1 == len(tmpl) {
			b.WriteByte(c)
			continue
		}
		switch d := tmpl[i+1]; {
		case d == '$':
			b.WriteByte('$')
		case d == '&':
			b.WriteString(s[loc[0]:loc[1]])
		case d == '`':
			b.WriteString(s[:loc[0]])
		case d == '\'':
			b.WriteString(s[loc[1]:])
		case isDigitByte(d):
			g, w := int(d-'0'), 1
			if i+2 < len(tmpl) && isDigitByte(tmpl[i+2]) {
				if g2 := g*10 + int(tmpl[i+2]-'0'); 2*g2+1 < len(loc) {
					g, w = g2, 2
				}
			}
			if g == 0 || 2*g+1 >= len(loc) {
				b.WriteByte(c)
				continue
			}
			if loc[2*g] >= 0 {
				b.WriteString(s[loc[2*g]:loc[2*g+1]])
			}
			i += w - 1
		default:
			b.WriteByte(c)
			continue
		}
		i++
	}
}

func (vm *pacVM) toRegexp(v any) *jsRegexp {
	if re, ok := v.(*jsRegexp); ok {
		return re
	}
	src := ""
	if v != jsUndefined {
		src = jsToString(v)
	}
	re, err := jsCompileRegex(src, "")
	if err != nil {
		vm.throwf("SyntaxError", "unsupported regular expression %q: %v", src, err)
	}
	return &jsRegexp{re, src, ""}
}

// jsCompileRegex maps a JavaScript regex onto Go's regexp, which covers what PAC files
// use (classes, anchors, groups, alternation, repeats) but not lookaround or backreferences.
func jsCompileRegex(src, flags string) (*regexp.Regexp, error) {
	mode := ""
	for _, f := range flags {
		switch f {
		case 'i', 'm', 's':
			if !strings.ContainsRune(mode, f) {
				mode += string(f)
			}
		case 'g', 'u', 'y', 'd':
		default:
			return nil, fmt.Errorf("unknown flag %q", f)
		}
	}
	var b strings.Builder
	if mode != "" {
		b.WriteString("(?" + mode + ")")
	}
	for i := 0; i < len(src); i++ {
		if src[i] == '\\' && i+1 < len(src) {
			if src[i+1] == 'u' && i+6 <= len(src) { // JavaScript's \u00e9 is \x{00e9} in Go
				if _, err := strconv.ParseUint(src[i+2:i+6], 16, 16); err == nil {
					b.WriteString(`\x{` + src[i+2:i+6] + `}`)
					i += 5
					continue
				}
			}
			b.WriteString(src[i : i+2])
			i++
			continue
		}
		b.WriteByte(src[i])
	}
	return regexp.Compile(b.String())
}

func jsExec(re *jsRegexp, s string) any {
	m := re.re.FindStringSubmatchIndex(s)
	if m == nil {
		return nil
	}
	out := make([]any, len(m)/2)
	for g := range out {
		if m[2*g] < 0 {
			out[g] = jsUndefined
		} else {
			out[g] = s[m[2*g]:m[2*g+1]]
		}
	}
	return &jsArray{out}
}

func jsRegexpProp(re *jsRegexp, name string) any {
	switch name {
	case "test":
		return jsFn(name, func(_ *pacVM, a []any) any { return re.re.MatchString(jsToString(jsArg(a, 0))) })
	case "exec":
		return jsFn(name, func(_ *pacVM, a []any) any { return jsExec(re, jsToString(jsArg(a, 0))) })
	case "toString":
		return jsFn(name, func(*pacVM, []any) any { return jsToString(re) })
	case "source":
		return re.src
	case "flags":
		return re.flags
	case "global":
		return strings.Contains(re.flags, "g")
	case "ignoreCase":
		return strings.Contains(re.flags, "i")
	case "multiline":
		return strings.Contains(re.flags, "m")
	}
	return jsUndefined
}

func jsArrayMethod(arr *jsArray, name string) *jsNative {
	var f func(vm *pacVM, a []any) any
	switch name {
	case "push":
		f = func(_ *pacVM, a []any) any {
			jsCheckArray(len(arr.a) + len(a))
			arr.a = append(arr.a, a...)
			return float64(len(arr.a))
		}
	case "unshift":
		f = func(_ *pacVM, a []any) any {
			jsCheckArray(len(arr.a) + len(a))
			arr.a = append(append([]any(nil), a...), arr.a...)
			return float64(len(arr.a))
		}
	case "pop", "shift":
		f = func(*pacVM, []any) any {
			if len(arr.a) == 0 {
				return jsUndefined
			}
			if name == "shift" {
				v := arr.a[0]
				arr.a = arr.a[1:]
				return v
			}
			v := arr.a[len(arr.a)-1]
			arr.a = arr.a[:len(arr.a)-1]
			return v
		}
	case "indexOf", "lastIndexOf", "includes":
		f = func(_ *pacVM, a []any) any {
			v := jsArg(a, 0)
			nan := jsTypeof(v) == "number" && math.IsNaN(v.(float64))
			for k := range arr.a {
				i := k
				if name == "lastIndexOf" {
					i = len(arr.a) - 1 - k
				}
				if arr.a[i] == v || name == "includes" && nan && jsTypeof(arr.a[i]) == "number" && math.IsNaN(arr.a[i].(float64)) {
					if name == "includes" {
						return true
					}
					return float64(i)
				}
			}
			if name == "includes" {
				return false
			}
			return -1.0
		}
	case "join", "toString":
		f = func(_ *pacVM, a []any) any {
			sep := ","
			if s := jsArg(a, 0); name == "join" && s != jsUndefined {
				sep = jsToString(s)
			}
			return jsJoin(arr, sep, 0)
		}
	case "slice":
		f = func(_ *pacVM, a []any) any {
			n := len(arr.a)
			i, j := jsRel(jsArg(a, 0), n, 0), jsRel(jsArg(a, 1), n, n)
			return &jsArray{append([]any{}, arr.a[i:max(i, j)]...)}
		}
	case "concat":
		f = func(_ *pacVM, a []any) any {
			out := append([]any{}, arr.a...)
			for _, x := range a {
				if xa, ok := x.(*jsArray); ok {
					out = append(out, xa.a...)
				} else {
					out = append(out, x)
				}
				jsCheckArray(len(out))
			}
			return &jsArray{out}
		}
	case "reverse":
		f = func(*pacVM, []any) any {
			slices.Reverse(arr.a)
			return arr
		}
	case "sort":
		f = func(vm *pacVM, a []any) any {
			cmp, items := jsArg(a, 0), arr.a
			slices.SortStableFunc(items, func(x, y any) int {
				switch {
				case x == jsUndefined || y == jsUndefined: // undefined sorts last
					return jsBoolInt(x == jsUndefined) - jsBoolInt(y == jsUndefined)
				case cmp != jsUndefined:
					r := jsToNumber(vm.call(cmp, []any{x, y}, "comparator"))
					return jsBoolInt(r > 0) - jsBoolInt(r < 0)
				}
				return strings.Compare(jsToString(x), jsToString(y))
			})
			return arr
		}
	case "forEach", "map", "filter", "some", "every", "find", "findIndex":
		f = func(vm *pacVM, a []any) any {
			fn, out := jsArg(a, 0), []any{}
			for i, x := range append([]any(nil), arr.a...) {
				r := vm.call(fn, []any{x, float64(i), arr}, name)
				switch {
				case name == "map":
					out = append(out, r)
				case name == "filter" && jsTruthy(r):
					out = append(out, x)
				case name == "some" && jsTruthy(r):
					return true
				case name == "every" && !jsTruthy(r):
					return false
				case name == "find" && jsTruthy(r):
					return x
				case name == "findIndex" && jsTruthy(r):
					return float64(i)
				}
			}
			switch name {
			case "map", "filter":
				return &jsArray{out}
			case "some":
				return false
			case "every":
				return true
			case "findIndex":
				return -1.0
			}
			return jsUndefined
		}
	default:
		return nil
	}
	return jsFn(name, f)
}

func jsBoolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsNumberProp(x float64, name string) any {
	switch name {
	case "toString":
		return jsFn(name, func(_ *pacVM, a []any) any {
			if r := jsArg(a, 0); r != jsUndefined {
				if radix := jsInt(r); radix >= 2 && radix <= 36 && x == math.Trunc(x) && math.Abs(x) < 1<<53 {
					return strconv.FormatInt(int64(x), radix)
				}
			}
			return jsNumStr(x)
		})
	case "toFixed":
		return jsFn(name, func(_ *pacVM, a []any) any {
			return strconv.FormatFloat(x, 'f', min(max(jsInt(jsArg(a, 0)), 0), 100), 64)
		})
	case "valueOf":
		return jsFn(name, func(*pacVM, []any) any { return x })
	}
	return jsUndefined
}

func jsDateProp(d *jsDate, name string) any {
	t := d.t
	if strings.HasPrefix(name, "getUTC") {
		t, name = t.UTC(), "get"+name[6:]
	}
	var v any
	switch name {
	case "getTime", "valueOf":
		v = float64(t.UnixMilli())
	case "getFullYear":
		v = float64(t.Year())
	case "getYear":
		v = float64(t.Year() - 1900)
	case "getMonth":
		v = float64(t.Month() - 1)
	case "getDate":
		v = float64(t.Day())
	case "getDay":
		v = float64(t.Weekday())
	case "getHours":
		v = float64(t.Hour())
	case "getMinutes":
		v = float64(t.Minute())
	case "getSeconds":
		v = float64(t.Second())
	case "getMilliseconds":
		v = float64(t.Nanosecond() / 1e6)
	case "getTimezoneOffset":
		_, off := t.Zone()
		v = float64(-off / 60)
	case "toString", "toUTCString", "toISOString", "toLocaleString":
		v = jsToString(d)
	default:
		return jsUndefined
	}
	return jsFn(name, func(*pacVM, []any) any { return v })
}

func jsNewDate(vm *pacVM, a []any) any {
	switch len(a) {
	case 0:
		return &jsDate{vm.now}
	case 1:
		return &jsDate{time.UnixMilli(int64(jsToNumber(a[0]))).In(vm.now.Location())}
	}
	n := make([]int, 7)
	n[2] = 1
	for i := range min(len(a), 7) {
		n[i] = jsInt(a[i])
	}
	return &jsDate{time.Date(n[0], time.Month(n[1]+1), n[2], n[3], n[4], n[5], n[6]*1e6, vm.now.Location())}
}

func jsParseInt(_ *pacVM, a []any) any {
	s := strings.TrimSpace(jsToString(jsArg(a, 0)))
	radix := 0
	if r := jsArg(a, 1); r != jsUndefined {
		radix = jsInt(r)
	}
	neg := strings.HasPrefix(s, "-")
	if neg || strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if (radix == 0 || radix == 16) && len(s) > 1 && s[0] == '0' && s[1]|0x20 == 'x' {
		s, radix = s[2:], 16
	}
	if radix == 0 {
		radix = 10
	}
	if radix < 2 || radix > 36 {
		return math.NaN()
	}
	v, digits := 0.0, 0
	for _, c := range []byte(s) {
		d := 99
		switch {
		case isDigitByte(c):
			d = int(c - '0')
		case c|0x20 >= 'a' && c|0x20 <= 'z':
			d = int(c|0x20-'a') + 10
		}
		if d >= radix {
			break
		}
		v, digits = v*float64(radix)+float64(d), digits+1
	}
	if digits == 0 {
		return math.NaN()
	}
	if neg {
		v = -v
	}
	return v
}

// jsParseFloat reads the longest number at the start of a string.
func jsParseFloat(_ *pacVM, a []any) any {
	s := strings.TrimSpace(jsToString(jsArg(a, 0)))
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	if strings.HasPrefix(s[i:], "Infinity") {
		return jsParseNumber(s[:i+8])
	}
	digits := func() {
		for i < len(s) && isDigitByte(s[i]) {
			i++
		}
	}
	digits()
	if i < len(s) && s[i] == '.' {
		i++
		digits()
	}
	if i < len(s) && s[i]|0x20 == 'e' {
		j := i
		if i++; i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i == len(s) || !isDigitByte(s[i]) {
			i = j
		}
		digits()
	}
	if strings.Trim(s[:i], "+-.") == "" {
		return math.NaN()
	}
	return jsParseNumber(s[:i])
}

// jsGlobals are JavaScript's own globals; pac.go adds the PAC functions.
func jsGlobals() map[string]any {
	math1 := func(name string, f func(float64) float64) *jsNative {
		return jsFn(name, func(_ *pacVM, a []any) any { return f(jsToNumber(jsArg(a, 0))) })
	}
	minMax := func(name string, f func(a, b float64) float64, start float64) *jsNative {
		return jsFn(name, func(_ *pacVM, a []any) any {
			r := start
			for _, x := range a {
				r = f(r, jsToNumber(x))
			}
			return r
		})
	}
	noop := jsFn("log", func(*pacVM, []any) any { return jsUndefined })
	newArray := func(vm *pacVM, a []any) any {
		n, ok := jsArg(a, 0).(float64)
		if len(a) != 1 || !ok {
			return &jsArray{append([]any{}, a...)}
		}
		if n < 0 || n != math.Trunc(n) {
			vm.throwf("RangeError", "invalid array length")
		}
		jsCheckArray(int(min(n, pacMaxArray+1)))
		out := make([]any, int(n))
		for i := range out {
			out[i] = jsUndefined
		}
		return &jsArray{out}
	}
	newRegExp := func(vm *pacVM, a []any) any {
		if re, ok := jsArg(a, 0).(*jsRegexp); ok {
			return re
		}
		src, flags := jsToString(jsArg(a, 0)), ""
		if f := jsArg(a, 1); f != jsUndefined {
			flags = jsToString(f)
		}
		re, err := jsCompileRegex(src, flags)
		if err != nil {
			vm.throwf("SyntaxError", "unsupported regular expression %q: %v", src, err)
		}
		return &jsRegexp{re, src, flags}
	}
	newObject := func(*pacVM, []any) any { return &jsObject{} }
	return map[string]any{
		"undefined":  jsUndefined,
		"NaN":        math.NaN(),
		"Infinity":   math.Inf(1),
		"parseInt":   jsFn("parseInt", jsParseInt),
		"parseFloat": jsFn("parseFloat", jsParseFloat),
		"isNaN":      jsFn("isNaN", func(_ *pacVM, a []any) any { return math.IsNaN(jsToNumber(jsArg(a, 0))) }),
		"isFinite": jsFn("isFinite", func(_ *pacVM, a []any) any {
			f := jsToNumber(jsArg(a, 0))
			return !math.IsNaN(f) && !math.IsInf(f, 0)
		}),
		"String": jsFn("String", func(_ *pacVM, a []any) any {
			if len(a) == 0 {
				return ""
			}
			return jsToString(a[0])
		}),
		"Number": jsFn("Number", func(_ *pacVM, a []any) any {
			if len(a) == 0 {
				return 0.0
			}
			return jsToNumber(a[0])
		}),
		"Boolean": jsFn("Boolean", func(_ *pacVM, a []any) any { return jsTruthy(jsArg(a, 0)) }),
		"Array":   &jsNative{name: "Array", call: newArray, ctor: newArray},
		"Object":  &jsNative{name: "Object", call: newObject, ctor: newObject},
		"RegExp":  &jsNative{name: "RegExp", call: newRegExp, ctor: newRegExp},
		"Date": &jsNative{name: "Date", ctor: jsNewDate, call: func(vm *pacVM, _ []any) any {
			return jsToString(&jsDate{vm.now})
		}},
		"Math": &jsNS{map[string]any{
			"floor": math1("floor", math.Floor),
			"ceil":  math1("ceil", math.Ceil),
			"round": math1("round", func(x float64) float64 { return math.Floor(x + 0.5) }),
			"trunc": math1("trunc", math.Trunc),
			"abs":   math1("abs", math.Abs),
			"sqrt":  math1("sqrt", math.Sqrt),
			"log":   math1("log", math.Log),
			"min":   minMax("min", math.Min, math.Inf(1)),
			"max":   minMax("max", math.Max, math.Inf(-1)),
			"pow": jsFn("pow", func(_ *pacVM, a []any) any {
				return math.Pow(jsToNumber(jsArg(a, 0)), jsToNumber(jsArg(a, 1)))
			}),
			"random": jsFn("random", func(*pacVM, []any) any { return rand.Float64() }),
			"PI":     math.Pi,
		}},
		"console": &jsNS{map[string]any{"log": noop, "info": noop, "warn": noop, "error": noop, "debug": noop}},
	}
}
