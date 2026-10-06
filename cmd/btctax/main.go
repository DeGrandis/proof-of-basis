// Command btctax serves the self-hosted Bitcoin tax calculator.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"btctax/internal/app"
	"btctax/internal/store"
	"btctax/web"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	addr := flag.String("addr", env("BTCTAX_ADDR", "127.0.0.1:8080"), "listen address")
	data := flag.String("data", env("BTCTAX_DATA", "./data"), "data directory")
	flag.Parse()

	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(filepath.Join(*data, "btctax.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	srv := app.New(st, web.Static(), os.Getenv("BTCTAX_PASSWORD"))
	hs := &http.Server{
		Addr:              *addr,
		Handler:           app.LogRequests(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("btctax listening on http://%s", *addr)
	log.Fatal(hs.ListenAndServe())
}
