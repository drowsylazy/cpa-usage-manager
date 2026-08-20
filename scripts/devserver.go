//go:build ignore

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/httpapi"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func main() {
	c := config.Default()
	if v := os.Getenv("CPA_DEV_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if err := c.EnsureDataDir(); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "devserver"})
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	ps, err := service.LoadPeppers(c, os.LookupEnv)
	if err != nil {
		log.Fatal(err)
	}
	api := httpapi.New(service.New(st, c, ps), st, "dev-secret")
	log.Println("http://127.0.0.1:18080/console")
	log.Fatal(http.ListenAndServe("127.0.0.1:18080", api.Handler()))
}
