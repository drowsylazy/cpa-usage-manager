package routelang

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
)

// evalCond 求值 when 条件。
func evalCond(ctx context.Context, x astExpr, env *Env) (bool, error) {
	v, err := evalExpr(ctx, x, env)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		line, col := x.exprPos()
		return false, fmt.Errorf("第 %d 行 %d 列: 条件必须为布尔值", line, col)
	}
	return b, nil
}

func evalExpr(ctx context.Context, x astExpr, env *Env) (any, error) {
	switch n := x.(type) {
	case *astNum:
		return n.v, nil
	case *astStr:
		return n.v, nil
	case *astBool:
		return n.v, nil
	case *astList:
		return n.items, nil
	case *astVar:
		if env == nil || env.Vars == nil {
			return nil, fmt.Errorf("第 %d 行 %d 列: 变量 %q 不可用（环境为空）", n.line, n.col, n.name)
		}
		v, ok := env.Vars[n.name]
		if !ok {
			return nil, fmt.Errorf("第 %d 行 %d 列: 未知变量 %q", n.line, n.col, n.name)
		}
		return v, nil
	case *astUnary:
		if n.op != "!" {
			return nil, fmt.Errorf("第 %d 行 %d 列: 未知一元运算符 %q", n.line, n.col, n.op)
		}
		v, err := evalExpr(ctx, n.x, env)
		if err != nil {
			return nil, err
		}
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("第 %d 行 %d 列: ! 要求布尔操作数", n.line, n.col)
		}
		return !b, nil
	case *astBin:
		return evalBin(ctx, n, env)
	case *astCall:
		return evalCall(ctx, n, env)
	}
	return nil, fmt.Errorf("routelang: 未支持的表达式节点 %T", x)
}

func evalBin(ctx context.Context, n *astBin, env *Env) (any, error) {
	// && / || 短路：ai_judge 这类高成本求值靠它跳过。
	if n.op == "&&" || n.op == "||" {
		l, err := evalExpr(ctx, n.l, env)
		if err != nil {
			return nil, err
		}
		lb, ok := l.(bool)
		if !ok {
			return nil, fmt.Errorf("第 %d 行 %d 列: %q 要求布尔操作数", n.line, n.col, n.op)
		}
		if n.op == "&&" && !lb {
			return false, nil
		}
		if n.op == "||" && lb {
			return true, nil
		}
		r, err := evalExpr(ctx, n.r, env)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, fmt.Errorf("第 %d 行 %d 列: %q 要求布尔操作数", n.line, n.col, n.op)
		}
		return rb, nil
	}

	l, err := evalExpr(ctx, n.l, env)
	if err != nil {
		return nil, err
	}
	r, err := evalExpr(ctx, n.r, env)
	if err != nil {
		return nil, err
	}

	if lf, lok := numericOf(l); lok {
		rf, rok := numericOf(r)
		if !rok {
			return nil, fmt.Errorf("第 %d 行 %d 列: 数值不能与 %T 比较", n.line, n.col, r)
		}
		return compareOrdered(n.op, lf, rf, n.line, n.col)
	}
	if ls, lok := l.(string); lok {
		rs, rok := r.(string)
		if !rok {
			return nil, fmt.Errorf("第 %d 行 %d 列: 字符串不能与 %T 比较", n.line, n.col, r)
		}
		if n.op == "==" {
			return ls == rs, nil
		}
		if n.op == "!=" {
			return ls != rs, nil
		}
		return nil, fmt.Errorf("第 %d 行 %d 列: 字符串仅支持 == 与 !=", n.line, n.col)
	}
	if lb, lok := l.(bool); lok {
		rb, rok := r.(bool)
		if !rok {
			return nil, fmt.Errorf("第 %d 行 %d 列: 布尔不能与 %T 比较", n.line, n.col, r)
		}
		if n.op == "==" {
			return lb == rb, nil
		}
		if n.op == "!=" {
			return lb != rb, nil
		}
		return nil, fmt.Errorf("第 %d 行 %d 列: 布尔仅支持 == 与 !=", n.line, n.col)
	}
	return nil, fmt.Errorf("第 %d 行 %d 列: 不支持的比较类型 %T", n.line, n.col, l)
}

func compareOrdered(op string, l, r float64, line, col int) (any, error) {
	switch op {
	case "<=":
		return l <= r, nil
	case ">=":
		return l >= r, nil
	case "<":
		return l < r, nil
	case ">":
		return l > r, nil
	case "==":
		return l == r, nil
	case "!=":
		return l != r, nil
	}
	return nil, fmt.Errorf("第 %d 行 %d 列: 未知比较运算符 %q", line, col, op)
}

func numericOf(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func evalCall(ctx context.Context, n *astCall, env *Env) (any, error) {
	// 编译期已保证 name == "ai_judge"。
	if len(n.args) != 1 {
		return nil, fmt.Errorf("第 %d 行 %d 列: ai_judge 需要且只需要一个数组实参", n.line, n.col)
	}
	listNode, ok := n.args[0].(*astList)
	if !ok {
		return nil, fmt.Errorf("第 %d 行 %d 列: ai_judge 实参应为字符串数组", n.line, n.col)
	}
	if env == nil || env.AI == nil {
		return nil, &AIError{Err: fmt.Errorf("judge 模型未配置")}
	}
	ans, err := env.AI(ctx, listNode.items)
	if err != nil {
		return nil, &AIError{Err: err}
	}
	for _, opt := range listNode.items {
		if normalizeKey(opt) == normalizeKey(ans) {
			return opt, nil
		}
	}
	return nil, &AIError{Err: fmt.Errorf("返回值 %q 不在选项中", strings2(ans))}
}

func strings2(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

// evalChain 求值候选链表达式，返回有序候选模型链。
func evalChain(c astChain, env *Env) ([]string, error) {
	switch n := c.(type) {
	case *chainStr:
		return []string{n.v}, nil
	case *chainPriority:
		out := make([]string, len(n.items))
		copy(out, n.items)
		return out, nil
	case *chainWeighted:
		total := 0.0
		for _, e := range n.entries {
			total += e.weight
		}
		// math/rand/v2 包级源：per-thread ChaCha8，无锁零分配。
		// 不用 math/rand.New(NewSource)——构造开销是单次抽样的 ~44 倍。
		pick := rand.Float64() * total
		var picked string
		acc := 0.0
		for _, e := range n.entries {
			acc += e.weight
			if pick <= acc {
				picked = e.model
				break
			}
		}
		if picked == "" {
			picked = n.entries[len(n.entries)-1].model
		}
		// 其余按权重降序跟随（同权重保持声明序），作为 failover 的次选链。
		type rest struct {
			model  string
			weight float64
			idx    int
		}
		others := make([]rest, 0, len(n.entries))
		for i, e := range n.entries {
			if e.model == picked {
				continue
			}
			others = append(others, rest{e.model, e.weight, i})
		}
		sort.SliceStable(others, func(i, j int) bool {
			if others[i].weight != others[j].weight {
				return others[i].weight > others[j].weight
			}
			return others[i].idx < others[j].idx
		})
		out := []string{picked}
		for _, o := range others {
			out = append(out, o.model)
		}
		return out, nil
	}
	return nil, fmt.Errorf("routelang: 未支持的候选链节点 %T", c)
}
