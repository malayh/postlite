package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/benbjohnson/postlite"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	addr := flag.String("addr", envOr("POSTLITE_ADDR", ":5432"), "postgres protocol bind address")
	dataDir := flag.String("data-dir", os.Getenv("POSTLITE_DATA_DIR"), "directory of SQLite databases (a client's database name selects a file within it)")
	database := flag.String("database", os.Getenv("POSTLITE_DATABASE"), "path to a single SQLite file served to every connection (overrides -data-dir)")
	username := flag.String("username", envOr("POSTLITE_USER", "postgres"), "username clients must authenticate as when a password is set")
	password := flag.String("password", os.Getenv("POSTLITE_PASSWORD"), "password clients must supply (MD5 auth); empty disables authentication")
	flag.Parse()

	if *dataDir == "" && *database == "" {
		return fmt.Errorf("required: -data-dir PATH or -database FILE")
	}

	log.SetFlags(0)

	s := postlite.NewServer()
	s.Addr = *addr
	s.DataDir = *dataDir
	s.DatabasePath = *database
	s.Username = *username
	s.Password = *password
	if err := s.Open(); err != nil {
		return err
	}
	defer s.Close()

	if *password == "" {
		log.Printf("warning: no password set; accepting all connections (set POSTLITE_PASSWORD to require authentication)")
	} else {
		log.Printf("password authentication enabled for user %q", *username)
	}
	log.Printf("listening on %s", s.Addr)

	// Wait on signal before shutting down.
	<-ctx.Done()
	log.Printf("SIGINT received, shutting down")

	// Perform clean shutdown.
	if err := s.Close(); err != nil {
		return err
	}
	log.Printf("postlite shutdown complete")

	return nil
}

// envOr returns the value of the named environment variable, or def if it is unset
// or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
