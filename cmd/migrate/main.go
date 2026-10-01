// Command migrate applies ./migrations/*.sql to a SQL database.
//
// Usage: go run ./cmd/migrate -dsn 'postgres://...' [-driver postgres] [-dir migrations]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/smallnest/matex/pkg/core/db"
)

func main() {
	driver := flag.String("driver", "postgres", "postgres | mysql | sqlite")
	dsn := flag.String("dsn", "", "database DSN (required)")
	dir := flag.String("dir", "migrations", "migrations directory")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "migrate: -dsn is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d, err := db.Open(ctx, db.Config{Driver: db.Driver(*driver), DSN: *dsn})
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	defer func() { _ = d.Close() }()

	if err := d.ApplyMigrations(ctx, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	fmt.Println("migrations applied")
}
