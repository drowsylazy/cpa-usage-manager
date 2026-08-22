//go:build ignore

// migcheck.go 验证 v1 → v2 迁移在真实旧库上的行为（开发用，不参与构建）。
//
//	CPA_DEV_DATA_DIR=/tmp/migtest go run scripts/migcheck.go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func main() {
	c := config.Default()
	if v := os.Getenv("CPA_DEV_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	path := filepath.Join(c.DataDir, c.DatabaseFile)
	fmt.Println("库:", path)

	st, err := store.Open(context.Background(), store.Options{Path: path, OwnerID: "migcheck"})
	if err != nil {
		log.Fatalf("打开（含迁移）失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	v, err := st.CurrentSchemaVersion(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("迁移后 schema 版本: %d（期望 %d）\n", v, store.SchemaVersion)

	// 列结构：token 列应存在，两个死价格列应消失
	var cols []string
	if err := st.Read(ctx, func(q store.Querier) error {
		rows, e := q.QueryContext(ctx, `SELECT name FROM pragma_table_info('plugin_keys')`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if e := rows.Scan(&n); e != nil {
				return e
			}
			cols = append(cols, n)
		}
		return rows.Err()
	}); err != nil {
		log.Fatal(err)
	}
	has := func(list []string, want string) bool {
		for _, x := range list {
			if x == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"token_limit", "daily_token_limit", "weekly_token_limit",
		"monthly_token_limit", "tokens_used", "daily_tokens_used", "weekly_tokens_used", "monthly_tokens_used"} {
		fmt.Printf("  plugin_keys.%-20s %v\n", want, has(cols, want))
	}

	var pcols []string
	if err := st.Read(ctx, func(q store.Querier) error {
		rows, e := q.QueryContext(ctx, `SELECT name FROM pragma_table_info('pricing_rules')`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if e := rows.Scan(&n); e != nil {
				return e
			}
			pcols = append(pcols, n)
		}
		return rows.Err()
	}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("  pricing_rules.price_reasoning 已删除: %v\n", !has(pcols, "price_reasoning"))
	fmt.Printf("  pricing_rules.price_cached    已删除: %v\n", !has(pcols, "price_cached"))

	// 数据完好：既有 Key 与规则不能丢，token 列应为 NULL（不限）
	s, err := st.Stats(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("数据: keys=%d rules=%d requests=%d rollups=%d\n", s.Keys, s.PricingRules, s.Requests, s.Rollups)

	keys, _, err := st.ListKeys(ctx, store.KeyFilter{Limit: 5})
	if err != nil {
		log.Fatal(err)
	}
	for _, k := range keys {
		fmt.Printf("  %s label=%-14q token_limit=%v tokens_used=%d quota=%v\n",
			k.KID, k.Label, k.TokenLimit, k.TokensUsed, k.QuotaMicroUSD)
	}
	rules, err := st.ListPricingRules(ctx, false)
	if err != nil {
		log.Fatal(err)
	}
	if len(rules) > 0 {
		r := rules[0]
		fmt.Printf("  规则 #%d %s:%s in=%d out=%d cr=%d cw=%d（读取正常）\n",
			r.ID, r.MatchKind, r.Pattern, r.PriceInput, r.PriceOutput, r.PriceCacheRead, r.PriceCacheCreation)
	}
	fmt.Println("迁移检查完成")
}
