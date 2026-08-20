//go:build ignore

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func main() {
	dir, err := os.MkdirTemp("", "cpa-usage-manager-smoke-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	c := config.Default()
	c.DataDir = dir
	c.DatabaseFile = "smoke.db"
	if err := c.EnsureDataDir(); err != nil {
		panic(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, c.DatabaseFile), OwnerID: "smoke"})
	if err != nil {
		panic(err)
	}
	defer st.Close()
	ps, err := service.LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		panic(err)
	}
	svc := service.New(st, c, ps)
	i, err := svc.IssueKey(ctx, service.IssueRequest{})
	if err != nil {
		panic(err)
	}
	if _, err = svc.Authenticate(ctx, i.Key); err != nil {
		panic(err)
	}
	f, err := svc.UsageSummary(ctx, service.UsageFilter{})
	if err != nil {
		panic(err)
	}
	fmt.Printf("smoke ok: kid=%s requests=%d\n", i.KID, f.Requests)
}
