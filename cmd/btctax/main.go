// Command btctax serves the self-hosted Bitcoin tax calculator.
package main

import (
	_ "time/tzdata" // time zones for the tax calendar, even in a minimal image
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/app"
	"github.com/DeGrandis/proof-of-basis/internal/demo"
	"github.com/DeGrandis/proof-of-basis/internal/prices"
	"github.com/DeGrandis/proof-of-basis/internal/store"
	"github.com/DeGrandis/proof-of-basis/web"
)

// version is the release tag, set at build time (-ldflags -X main.version=…).
var version = "dev"

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
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		resetPassword(os.Args[2:])
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
	// Parser fixes reach files imported before them.
	if n, err := st.Reparse(); err != nil {
		log.Printf("re-reading imports: %v", err)
	} else if n > 0 {
		log.Printf("re-read imports: %d rows updated", n)
	}

	app.Version = version
	srv := app.New(st, web.Static())
	// An app platform (Umbrel, StartOS) passes its node's services here.
	nodeSet, notes := app.NodeDefaults(os.Getenv)
	for _, n := range notes {
		log.Print(n)
	}
	if len(nodeSet) > 0 {
		srv.UseNodeServices(nodeSet)
		log.Printf("using the node's services as defaults: %d settings", len(nodeSet))
	}
	if err := srv.ApplyEnvPassword(os.Getenv("BTCTAX_PASSWORD")); err != nil {
		log.Fatal(err)
	}
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
	from := fs.String("from", "2010-07-17", "first day (YYYY-MM-DD), or last for the file's last day on (a quick top-up)")
	out := fs.String("o", "internal/prices/data/btcusd-daily.csv", "output CSV; existing rows are kept unless refetched")
	fs.Parse(args)
	s, ok := prices.Get(*src)
	if !ok {
		log.Fatalf("unknown source %q (have %s)", *src, sourceIDs())
	}
	merged := map[string]int64{}
	if f, err := os.Open(*out); err == nil {
		merged, err = prices.ReadCSV(f)
		f.Close()
		if err != nil {
			log.Fatal(err)
		}
	}
	if *from == "last" {
		// Refetch the last day too: its close may have been taken mid-day.
		*from = "2010-07-17"
		for d := range merged {
			if d > *from {
				*from = d
			}
		}
	}
	start, err := time.Parse("2006-01-02", *from)
	if err != nil {
		log.Fatal(err)
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
	scale := fs.Int64("scale", 1, "divide every amount by this (10: about 0.12 BTC from the same small buys)")
	fs.Parse(args)
	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(filepath.Join(*data, "btctax.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if err := demo.SeedScaled(st, *seed, *scale); err != nil {
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

// resetPassword clears the login password and signs out every session.
// The next visit to the app asks for a new password. Nothing else changes.
func resetPassword(args []string) {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	data := fs.String("data", env("BTCTAX_DATA", "./data"), "data directory")
	fs.Parse(args)
	st, err := store.Open(filepath.Join(*data, "btctax.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if err := st.SetPasswordHash(""); err != nil {
		log.Fatal(err)
	}
	fmt.Println("Password cleared and everyone signed out. Open the app to set a new password.")
	if os.Getenv("BTCTAX_PASSWORD") != "" {
		fmt.Println("Note: BTCTAX_PASSWORD is set, so the app will use it again the next time it starts.")
	}
}
