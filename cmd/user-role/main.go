// Command user-role lists users and updates a user's role in the datastore.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"sightingmap/internal/db"
	"sightingmap/internal/store"
)

func main() {
	log.SetFlags(0)

	var dbPath string
	flag.StringVar(&dbPath, "db", env("DB_PATH", "sightingmap.db"), "SQLite database path")
	flag.Parse()

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", dbPath, err)
	}
	defer sqlDB.Close()
	st := store.New(sqlDB)

	if flag.NArg() == 0 {
		printUsage()
		handleRoles(st)
		return
	}

	cmd := flag.Arg(0)
	switch cmd {
	case "list":
		handleList(st)
	case "set":
		handleSet(st, flag.Args()[1:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
	go run ./cmd/user-role list [-db path]
	go run ./cmd/user-role set [-db path] <opaque_user_id> <role>

Commands:
	list   print every opaque user id and its role and reg'n date and last seen
	set    assign a role to the specified opaque user id
`)
	os.Exit(2)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `usage:
	go run ./cmd/user-role list [-db path]
	go run ./cmd/user-role set [-db path] <opaque_user_id> <role>

Commands:
	list   print every opaque user id and its current role
	set    assign a role to the specified opaque user id

`)
}

func handleRoles(st *store.Store) {
	fmt.Fprintln(os.Stderr, "Known roles:")
	for _, role := range store.KnownRoles() {
		fmt.Fprintf(os.Stderr, "  %s\n", role)
	}
}

func handleList(st *store.Store) {
	users, err := st.ListUsers()
	if err != nil {
		log.Fatalf("list users: %v", err)
	}
	if len(users) == 0 {
		fmt.Println("no users found")
		return
	}
	fmt.Println("opaque_id\trole\tregistered_at\tlast_seen_at\tcalled")
	for _, u := range users {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", u.OpaqueID, u.Role, u.RegisteredAt, u.LastSeenAt, u.Called)
	}
}

func handleSet(st *store.Store, args []string) {
	if len(args) != 2 {
		usage()
	}
	opaqueID := args[0]
	role := args[1]
	if err := st.SetUserRole(opaqueID, role); err != nil {
		if err == store.ErrNotFound {
			log.Fatalf("user %q not found", opaqueID)
		}
		log.Fatalf("set role: %v", err)
	}
	fmt.Printf("set %s -> %s\n", opaqueID, role)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
