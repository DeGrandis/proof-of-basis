// Command btctax-demo serves the public demo of btctax: the same app,
// with invented data, a private copy per visitor, no login, a simulated
// node, and the assistant on Claude in Amazon Bedrock (with the machine's
// AWS role) within a daily allowance.
//
// It is a separate program so the app people run never carries the demo
// or the AWS libraries; both are built from the same code at the same
// commit, so the demo is always the app as it is.
package main

import (
	_ "time/tzdata"
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/DeGrandis/proof-of-basis/internal/app"
	"github.com/DeGrandis/proof-of-basis/internal/claude"
	"github.com/DeGrandis/proof-of-basis/internal/sandbox"
	"github.com/DeGrandis/proof-of-basis/web"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return d
}

func main() {
	addr := flag.String("addr", env("BTCTAX_ADDR", "127.0.0.1:8080"), "listen address")
	data := flag.String("data", env("BTCTAX_DATA", "./data"), "working directory for visitors' copies")
	flag.Parse()

	ctx := context.Background()
	var ai app.ChatModel
	region := env("BTCTAX_BEDROCK_REGION", env("AWS_REGION", "us-east-1"))
	model := env("BTCTAX_BEDROCK_MODEL", "anthropic.claude-haiku-5-5")
	if env("BTCTAX_DEMO_AI", "true") == "true" {
		if c, err := claude.New(ctx, region, model); err != nil {
			log.Printf("assistant off: %v", err)
		} else {
			ai = c
		}
	}
	hub, err := sandbox.New(sandbox.Config{
		Dir: filepath.Join(*data, "demo"), Static: web.Static(), AI: ai,
		MaxCopies:    envInt("BTCTAX_DEMO_MAX_COPIES", 100),
		IdleTTL:      time.Duration(envInt("BTCTAX_DEMO_IDLE_HOURS", 3)) * time.Hour,
		AIPerVisitor: envInt("BTCTAX_DEMO_AI_PER_VISITOR", 20),
		AIPerDay:     envInt("BTCTAX_DEMO_AI_PER_DAY", 300),
	})
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{Addr: *addr, Handler: app.LogRequests(hub), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("btctax demo listening on http://%s (assistant: %v, model %s in %s)", *addr, ai != nil, model, region)
	log.Fatal(hs.ListenAndServe())
}
