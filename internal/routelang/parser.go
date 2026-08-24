package routelang

import (
	"fmt"
	"strings"
)

// ---- AST ----

type astProg struct {
	branches []astBranch // when 分支，按声明序
	fallback astBranch   // 无条件兜底（cond 为 nil）
}

type astBranch struct {
	cond  astExpr // nil 表示无条件
	chain astChain
	line  int
	col   int
}

type astExpr interface{ exprPos() (int, int) }

type astNum struct {
	v         float64
	line, col int
}
type astStr struct {
	v         string
	line, col int
}
type astVar struct {
	name      string
	line, col int
}
type astUnary struct {
	op        string // "!"
	x         astExpr
	line, col int
}
type astBin struct {
	op        string // && || <= >= == != < >
	l, r      astExpr
	line, col int
}
type astCall struct {
	name      string
	args      []astExpr
	line, col int
}

func (n *astNum) exprPos() (int, int)   { return n.line, n.col }
func (n *astStr) exprPos() (int, int)   { return n.line, n.col }
func (n *astVar) exprPos() (int, int)   { return n.line, n.col }
func (n *astUnary) exprPos() (int, int) { return n.line, n.col }
func (n *astBin) exprPos() (int, int)   { return n.line, n.col }
func (n *astCall) exprPos() (int, int)  { return n.line, n.col }

type astChain interface {
	chainPos() (int, int)
}

type chainStr struct {
	v         string
	line, col int
}
type chainPriority struct {
	items     []string
	line, col int
}
type chainWeighted struct {
	entries   []weightedEntry // 声明序；weight 已在编译期确认为正数
	line, col int
}

type weightedEntry struct {
	model  string
	weight float64
}

// astList 是字符串数组字面量（仅用于 ai_judge 实参）。
type astList struct {
	items     []string
	line, col int
}

func (n *astList) exprPos() (int, int) { return n.line, n.col }

func (n *chainStr) chainPos() (int, int)      { return n.line, n.col }
func (n *chainPriority) chainPos() (int, int) { return n.line, n.col }
func (n *chainWeighted) chainPos() (int, int) { return n.line, n.col }

// ---- Lexer ----

type tokKind int

const (
	tkEOF tokKind = iota
	tkIdent
	tkNumber
	tkString
	tkArrow // ->
	tkOp    // 单/双字符符号：<= >= == != < > ! && || ( ) [ ] { } : ,
)

type token struct {
	kind tokKind
	text string
	num  float64
	line int
	col  int
}

func syntaxErrorf(line, col int, format string, args ...any) error {
	return &SyntaxError{Msg: fmt.Sprintf(format, args...), Line: line, Col: col}
}

// lex 把脚本切成 token。注释以 # 起始到行尾。
func lex(src string) ([]token, error) {
	var toks []token
	line, col := 1, 1
	i := 0
	n := len(src)
	adv := func(k int) { i += k; col += k }
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			adv(1)
			line++
			col = 1
			continue
		case c == ' ' || c == '\t' || c == '\r':
			adv(1)
			continue
		case c == '#':
			for i < n && src[i] != '\n' {
				adv(1)
			}
			continue
		case c == '-' && i+1 < n && src[i+1] == '>':
			toks = append(toks, token{kind: tkArrow, text: "->", line: line, col: col})
			adv(2)
			continue
		case c == '"':
			sl, sc := line, col
			adv(1)
			var sb strings.Builder
			closed := false
			for i < n {
				ch := src[i]
				if ch == '\n' {
					break
				}
				if ch == '\\' {
					if i+1 >= n {
						break
					}
					esc := src[i+1]
					switch esc {
					case '"', '\\':
						sb.WriteByte(esc)
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					default:
						sb.WriteByte('\\')
						sb.WriteByte(esc)
					}
					adv(2)
					continue
				}
				if ch == '"' {
					closed = true
					adv(1)
					break
				}
				sb.WriteByte(ch)
				adv(1)
			}
			if !closed {
				return nil, syntaxErrorf(sl, sc, "字符串缺少收尾引号")
			}
			toks = append(toks, token{kind: tkString, text: sb.String(), line: sl, col: sc})
			continue
		case c >= '0' && c <= '9':
			sl, sc := line, col
			j := i
			for j < n && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			text := src[i:j]
			var num float64
			if _, err := fmt.Sscanf(text, "%g", &num); err != nil {
				return nil, syntaxErrorf(sl, sc, "非法数字 %q", text)
			}
			toks = append(toks, token{kind: tkNumber, text: text, num: num, line: sl, col: sc})
			adv(j - i)
			continue
		case isIdentStart(c):
			sl, sc := line, col
			j := i
			for j < n && isIdentPart(src[j]) {
				j++
			}
			toks = append(toks, token{kind: tkIdent, text: src[i:j], line: sl, col: sc})
			adv(j - i)
			continue
		default:
			two := ""
			if i+1 < n {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "==", "!=", "&&", "||":
				toks = append(toks, token{kind: tkOp, text: two, line: line, col: col})
				adv(2)
				continue
			}
			if strings.ContainsRune("<>!()[]{}:,", rune(c)) {
				toks = append(toks, token{kind: tkOp, text: string(c), line: line, col: col})
				adv(1)
				continue
			}
			return nil, syntaxErrorf(line, col, "无法识别的字符 %q", string(c))
		}
	}
	toks = append(toks, token{kind: tkEOF, line: line, col: col})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// ---- Parser ----

type parser struct {
	toks []token
	pos  int
	prog *Program
}

func parseProgram(src string) (*Program, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, prog: &Program{}}
	root, usesAI, refs, err := p.parseFile()
	if err != nil {
		return nil, err
	}
	p.prog.root = root
	p.prog.usesAI = usesAI
	p.prog.refs = refs
	return p.prog, nil
}

func (p *parser) peek() token   { return p.toks[p.pos] }
func (p *parser) next() token   { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) atArrow() bool { return p.peek().kind == tkArrow }
func (p *parser) atWhen() bool {
	t := p.peek()
	return t.kind == tkIdent && t.text == "when"
}

func (p *parser) parseFile() (astProg, bool, []string, error) {
	var root astProg
	usesAI := false
	refsSeen := map[string]bool{}
	var refs []string
	sawFallback := false
	for {
		t := p.peek()
		if t.kind == tkEOF {
			break
		}
		if sawFallback {
			return root, false, nil, syntaxErrorf(t.line, t.col, "兜底分支必须是最后一条")
		}
		switch {
		case p.atWhen():
			b, uai, rrefs, err := p.parseWhenBranch(refsSeen)
			if err != nil {
				return root, false, nil, err
			}
			usesAI = usesAI || uai
			refs = appendUnique(refs, rrefs)
			root.branches = append(root.branches, b)
		case p.atArrow():
			b, uai, rrefs, err := p.parseFallbackBranch(refsSeen)
			if err != nil {
				return root, false, nil, err
			}
			usesAI = usesAI || uai
			refs = appendUnique(refs, rrefs)
			root.fallback = b
			sawFallback = true
		default:
			return root, false, nil, syntaxErrorf(t.line, t.col, "期望 when 或 -> ，得到 %q", tokText(t))
		}
	}
	if !sawFallback {
		last := p.toks[len(p.toks)-1]
		return root, false, nil, syntaxErrorf(last.line, last.col, "缺少无条件兜底分支（-> ...）")
	}
	return root, usesAI, refs, nil
}

func appendUnique(list []string, items []string) []string {
	for _, it := range items {
		dup := false
		for _, v := range list {
			if normalizeKey(v) == normalizeKey(it) {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, it)
		}
	}
	return list
}

func tokText(t token) string {
	if t.kind == tkEOF {
		return "文件末尾"
	}
	return t.text
}

func (p *parser) parseWhenBranch(refsSeen map[string]bool) (astBranch, bool, []string, error) {
	w := p.next() // when
	cond, uai, err := p.parseExpr()
	if err != nil {
		return astBranch{}, false, nil, err
	}
	if !p.atArrow() {
		t := p.peek()
		return astBranch{}, false, nil, syntaxErrorf(t.line, t.col, "条件后应为 -> ，得到 %q", tokText(t))
	}
	p.next()
	chain, cai, rrefs, err := p.parseChain(refsSeen)
	if err != nil {
		return astBranch{}, false, nil, err
	}
	return astBranch{cond: cond, chain: chain, line: w.line, col: w.col}, uai || cai, rrefs, nil
}

func (p *parser) parseFallbackBranch(refsSeen map[string]bool) (astBranch, bool, []string, error) {
	a := p.next() // ->
	chain, cai, rrefs, err := p.parseChain(refsSeen)
	if err != nil {
		return astBranch{}, false, nil, err
	}
	return astBranch{chain: chain, line: a.line, col: a.col}, cai, rrefs, nil
}

// parseChain 解析候选链表达式：字符串字面量 / priority[...] / weighted{...}。
// 函数调用形式只保留给 ai_judge 出现在条件里；链上出现即报错。
func (p *parser) parseChain(refsSeen map[string]bool) (astChain, bool, []string, error) {
	t := p.peek()
	switch {
	case t.kind == tkString:
		p.next()
		markRef(refsSeen, t.text)
		return &chainStr{v: t.text, line: t.line, col: t.col}, false, []string{t.text}, nil
	case t.kind == tkIdent && t.text == "priority":
		p.next()
		if o := p.peek(); !(o.kind == tkOp && o.text == "[") {
			return nil, false, nil, syntaxErrorf(o.line, o.col, "priority 后应为 [")
		}
		p.next()
		var items []string
		if c := p.peek(); c.kind == tkOp && c.text == "]" {
			return nil, false, nil, syntaxErrorf(c.line, c.col, "priority 列表不能为空")
		}
		for {
			s := p.peek()
			if s.kind != tkString {
				return nil, false, nil, syntaxErrorf(s.line, s.col, "priority 列表元素应为字符串，得到 %q", tokText(s))
			}
			p.next()
			markRef(refsSeen, s.text)
			items = append(items, s.text)
			c := p.peek()
			if c.kind == tkOp && c.text == "," {
				p.next()
				continue
			}
			break
		}
		if c := p.peek(); !(c.kind == tkOp && c.text == "]") {
			return nil, false, nil, syntaxErrorf(c.line, c.col, "priority 列表应以 ] 结束，得到 %q", tokText(c))
		}
		p.next()
		if len(items) == 0 {
			return nil, false, nil, syntaxErrorf(t.line, t.col, "priority 列表不能为空")
		}
		return &chainPriority{items: items, line: t.line, col: t.col}, false, items, nil
	case t.kind == tkIdent && t.text == "weighted":
		p.next()
		if o := p.peek(); !(o.kind == tkOp && o.text == "{") {
			return nil, false, nil, syntaxErrorf(o.line, o.col, "weighted 后应为 {")
		}
		p.next()
		var entries []weightedEntry
		seen := map[string]bool{}
		for {
			s := p.peek()
			if s.kind != tkString {
				return nil, false, nil, syntaxErrorf(s.line, s.col, "weighted 键应为字符串模型名，得到 %q", tokText(s))
			}
			p.next()
			c := p.peek()
			if !(c.kind == tkOp && c.text == ":") {
				return nil, false, nil, syntaxErrorf(c.line, c.col, "weighted 键后应为 :")
			}
			p.next()
			w := p.peek()
			if w.kind != tkNumber {
				return nil, false, nil, syntaxErrorf(w.line, w.col, "weighted 权重应为数字，得到 %q", tokText(w))
			}
			p.next()
			if w.num <= 0 {
				return nil, false, nil, syntaxErrorf(w.line, w.col, "weighted 权重必须为正数")
			}
			key := normalizeKey(s.text)
			if seen[key] {
				return nil, false, nil, syntaxErrorf(s.line, s.col, "weighted 中重复的模型 %q", s.text)
			}
			seen[key] = true
			markRef(refsSeen, s.text)
			entries = append(entries, weightedEntry{model: s.text, weight: w.num})
			cc := p.peek()
			if cc.kind == tkOp && cc.text == "," {
				p.next()
				continue
			}
			break
		}
		if c := p.peek(); !(c.kind == tkOp && c.text == "}") {
			return nil, false, nil, syntaxErrorf(c.line, c.col, "weighted 应以 } 结束，得到 %q", tokText(c))
		}
		p.next()
		if len(entries) == 0 {
			return nil, false, nil, syntaxErrorf(t.line, t.col, "weighted 不能为空")
		}
		var refs []string
		for _, e := range entries {
			refs = append(refs, e.model)
		}
		return &chainWeighted{entries: entries, line: t.line, col: t.col}, false, refs, nil
	default:
		return nil, false, nil, syntaxErrorf(t.line, t.col,
			"候选链应为字符串、priority [...] 或 weighted {...}，得到 %q", tokText(t))
	}
}

func markRef(seen map[string]bool, model string) { _ = seen[normalizeKey(model)] }

// parseExpr 解析布尔/算术表达式（优先级：|| < && < 比较 < 一元!）。
func (p *parser) parseExpr() (astExpr, bool, error) {
	return p.parseOr()
}

func (p *parser) parseOr() (astExpr, bool, error) {
	left, uai, err := p.parseAnd()
	if err != nil {
		return nil, false, err
	}
	for {
		t := p.peek()
		if t.kind == tkOp && t.text == "||" {
			p.next()
			right, u2, err := p.parseAnd()
			if err != nil {
				return nil, false, err
			}
			uai = uai || u2
			left = &astBin{op: "||", l: left, r: right, line: t.line, col: t.col}
			continue
		}
		return left, uai, nil
	}
}

func (p *parser) parseAnd() (astExpr, bool, error) {
	left, uai, err := p.parseCompare()
	if err != nil {
		return nil, false, err
	}
	for {
		t := p.peek()
		if t.kind == tkOp && t.text == "&&" {
			p.next()
			right, u2, err := p.parseCompare()
			if err != nil {
				return nil, false, err
			}
			uai = uai || u2
			left = &astBin{op: "&&", l: left, r: right, line: t.line, col: t.col}
			continue
		}
		return left, uai, nil
	}
}

var compareOps = map[string]bool{"<=": true, ">=": true, "==": true, "!=": true, "<": true, ">": true}

func (p *parser) parseCompare() (astExpr, bool, error) {
	left, uai, err := p.parseUnary()
	if err != nil {
		return nil, false, err
	}
	t := p.peek()
	if t.kind == tkOp && compareOps[t.text] {
		p.next()
		right, u2, err := p.parseUnary()
		if err != nil {
			return nil, false, err
		}
		return &astBin{op: t.text, l: left, r: right, line: t.line, col: t.col}, uai || u2, nil
	}
	return left, uai, nil
}

func (p *parser) parseUnary() (astExpr, bool, error) {
	t := p.peek()
	if t.kind == tkOp && t.text == "!" {
		p.next()
		x, uai, err := p.parseUnary()
		if err != nil {
			return nil, false, err
		}
		return &astUnary{op: "!", x: x, line: t.line, col: t.col}, uai, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (astExpr, bool, error) {
	t := p.peek()
	switch {
	case t.kind == tkNumber:
		p.next()
		return &astNum{v: t.num, line: t.line, col: t.col}, false, nil
	case t.kind == tkString:
		p.next()
		return &astStr{v: t.text, line: t.line, col: t.col}, false, nil
	case t.kind == tkOp && t.text == "(":
		p.next()
		x, uai, err := p.parseExpr()
		if err != nil {
			return nil, false, err
		}
		if c := p.peek(); !(c.kind == tkOp && c.text == ")") {
			return nil, false, syntaxErrorf(c.line, c.col, "应为 )，得到 %q", tokText(c))
		}
		p.next()
		return x, uai, nil
	case t.kind == tkOp && t.text == "[":
		// 字符串数组字面量：["a", "b"]（仅用于 ai_judge 实参）。
		sl, sc := t.line, t.col
		p.next()
		var items []string
		for {
			s := p.peek()
			if s.kind != tkString {
				return nil, false, syntaxErrorf(s.line, s.col, "数组元素应为字符串，得到 %q", tokText(s))
			}
			p.next()
			items = append(items, s.text)
			c := p.peek()
			if c.kind == tkOp && c.text == "," {
				p.next()
				continue
			}
			break
		}
		if c := p.peek(); !(c.kind == tkOp && c.text == "]") {
			return nil, false, syntaxErrorf(c.line, c.col, "数组应以 ] 结束，得到 %q", tokText(c))
		}
		p.next()
		if len(items) == 0 {
			return nil, false, syntaxErrorf(sl, sc, "数组不能为空")
		}
		return &astList{items: items, line: sl, col: sc}, false, nil
	case t.kind == tkIdent:
		p.next()
		if o := p.peek(); o.kind == tkOp && o.text == "(" {
			p.next()
			var args []astExpr
			uai := false
			if o2 := p.peek(); !(o2.kind == tkOp && o2.text == ")") {
				for {
					a, u2, err := p.parseExpr()
					if err != nil {
						return nil, false, err
					}
					uai = uai || u2
					args = append(args, a)
					c := p.peek()
					if c.kind == tkOp && c.text == "," {
						p.next()
						continue
					}
					break
				}
			}
			if c := p.peek(); !(c.kind == tkOp && c.text == ")") {
				return nil, false, syntaxErrorf(c.line, c.col, "函数调用应以 ) 结束，得到 %q", tokText(c))
			}
			p.next()
			if t.text != "ai_judge" {
				return nil, false, syntaxErrorf(t.line, t.col,
					"未知函数 %q（本期仅内置 ai_judge）", t.text)
			}
			uai = true
			return &astCall{name: t.text, args: args, line: t.line, col: t.col}, uai, nil
		}
		return &astVar{name: t.text, line: t.line, col: t.col}, false, nil
	default:
		return nil, false, syntaxErrorf(t.line, t.col, "无法识别的表达式记号 %q", tokText(t))
	}
}
