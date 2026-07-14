// crawler is the CLI entry for the jind-ai plugin registry. All logic lives
// in internal/crawl.Run; this file only reads flags, wires the HTTP client,
// and pipes errors to stderr.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/takaaki-s/jind-ai-plugin-registry/internal/crawl"
	"github.com/takaaki-s/jind-ai-plugin-registry/internal/github"
)

func main() {
	prevPath := flag.String("prev", "public/prev.json", "path to previous registry.json (state input)")
	outPath := flag.String("out", "public/registry.json", "path to write the new registry.json")
	topic := flag.String("topic", crawl.DefaultTopic, "GitHub topic to search for")
	flag.Parse()

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "crawler: GITHUB_TOKEN env var is required")
		os.Exit(2)
	}

	client, err := github.NewHTTPClient(github.Config{Token: token})
	if err != nil {
		fmt.Fprintf(os.Stderr, "crawler: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := crawl.Run(ctx, crawl.Options{
		Client:   client,
		PrevPath: *prevPath,
		OutPath:  *outPath,
		Topic:    *topic,
		Logger:   log.New(os.Stderr, "", log.LstdFlags),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "crawler: %v\n", err)
		os.Exit(1)
	}
}
