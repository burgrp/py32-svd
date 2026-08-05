package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tinygo-org/py32-svd/internal/updater"
)

func main() {
	config := flag.String("config", "packs.json", "path to the pack configuration")
	output := flag.String("output", "svd", "directory to replace with generated SVD files")
	flag.Parse()

	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := updater.Run(ctx, updater.Options{
		ConfigPath: *config,
		OutputDir:  *output,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "update failed:", err)
		os.Exit(1)
	}
}
