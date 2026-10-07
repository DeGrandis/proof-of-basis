// Command btctax serves the self-hosted Bitcoin tax calculator.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"btctax/internal/app"
	"btctax/internal/demo"
	"btctax/internal/prices"
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
	if len(os.Args) > 2 && os.Args[1] == "prices" && os.Args[2] == "fetch" {
		fetchPrices(os.Args[3:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "demo" {
		seedDemo(os.Args[2:])
		return
	}
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

// fetchPrices downloads daily closes and writes them as CSV. It is used when
// building the image so the price history ships inside the binary.
func fetchPrices(args []string) {
	fs := flag.NewFlagSet("prices fetch", flag.ExitOnError)
	src := fs.String("source", "yahoo", "price source: "+sourceIDs())
	from := fs.String("from", "2010-07-17", "first day (YYYY-MM-DD)")
	out := fs.String("o", "internal/prices/data/btcusd-daily.csv", "output CSV; existing rows are kept unless refetched")
	fs.Parse(args)
	s, ok := prices.Get(*src)
	if !ok {
		log.Fatalf("unknown source %q (have %s)", *src, sourceIDs())
	}
	start, err := time.Parse("2006-01-02", *from)
	if err != nil {
		log.Fatal(err)
	}
	merged := map[string]int64{}
	if f, err := os.Open(*out); err == nil {
		merged, err = prices.ReadCSV(f)
		f.Close()
		if err != nil {
			log.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	got, err := s.Fetch(ctx, start, time.Now())
	if err != nil {
		log.Fatalf("fetch from %s: %v", s.Host(), err)
	}
	for d, c := range got {
		merged[d] = c
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	if err := prices.WriteCSV(f, merged); err != nil {
		log.Fatal(err)
	}
	f.Close()
	fmt.Printf("%s: %d days fetched, %d in %s\n", s.Name(), len(got), len(merged), *out)
}

// seedDemo fills an empty data directory with invented history for trying
// the app. It refuses a database that already has accounts.
func seedDemo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	data := fs.String("data", env("BTCTAX_DATA", "./data"), "data directory (must be empty of accounts)")
	seed := fs.Int64("seed", 1, "random seed")
	fs.Parse(args)
	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(filepath.Join(*data, "btctax.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if err := demo.Seed(st, *seed); err != nil {
		log.Fatal(err)
	}
	fmt.Println("demo data written to", *data)
}

func sourceIDs() string {
	var ids []string
	for _, s := range prices.All() {
		ids = append(ids, s.ID())
	}
	return strings.Join(ids, ", ")
}
