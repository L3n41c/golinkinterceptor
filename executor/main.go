package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
)

const dbPath = "link.db"

func main() {
	link := flag.String("link", "", "File path to the linker executable (Should be \"$(go env GOTOOLDIR)/link\")")
	tags := flag.String("tags", "", "Build tags to use")
	flag.Parse()
	if len(flag.Args()) < 1 {
		fmt.Fprintln(os.Stderr, "Need an executable name")
		flag.Usage()
		os.Exit(2)
	}

	binaryName := flag.Arg(0)
	buildTags := strings.Split(*tags, ",")
	slices.Sort(buildTags)
	buildTagsJSON, err := json.Marshal(buildTags)
	if err != nil {
		log.Fatalf("unable to marshal build tags: %v", err)
	}

	ctx := context.Background()

	// Open the database
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_foreign_keys=true")
	if err != nil {
		log.Fatalf("unable to open database %q: %v", dbPath, err)
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Fatalf("unable to begin transaction: %v", err)
	}
	defer tx.Rollback()

	// Get the link command ID
	row := tx.QueryRowContext(ctx, `
SELECT link_command_id
FROM link_command NATURAL JOIN build_tags
WHERE binary_name = ? AND tags = jsonb(?);`,
		binaryName, buildTagsJSON)
	var linkCommandID int
	if err := row.Scan(&linkCommandID); err != nil {
		if err == sql.ErrNoRows {
			fmt.Fprintf(os.Stderr, "No link command found for %q with build tags %q\n", binaryName, buildTags)
			os.Exit(1)
		} else {
			log.Fatalf("unable to query link command ID: %v", err)
		}
	}

	// Get the importcfg
	rows, err := tx.QueryContext(ctx, `
SELECT 'packagefile ' || package || '=' || file
FROM package_file NATURAL JOIN link_command_package_file
WHERE link_command_id = ?
UNION
SELECT line
FROM importcfg_additional_lines
WHERE link_command_id = ?;`,
		linkCommandID, linkCommandID)
	if err != nil {
		log.Fatalf("unable to query importcfg: %v", err)
	}
	defer rows.Close()

	importcfgFile, err := os.CreateTemp("", "importcfg.link")
	if err != nil {
		log.Fatalf("unable to create importcfg file: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			log.Fatalf("unable to scan importcfg line: %v", err)
		}
		log.Printf("%s --- %s\n", importcfgFile.Name(), line)
		if _, err := fmt.Fprintln(importcfgFile, line); err != nil {
			log.Fatalf("unable to write importcfg line: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("error reading importcfg rows: %v", err)
	}
	if err := importcfgFile.Close(); err != nil {
		log.Fatalf("unable to close importcfg file: %v", err)
	}

	// Get the linker command
	binaryFile, err := os.CreateTemp("", binaryName)
	if err != nil {
		log.Fatalf("unable to create binary file: %v", err)
	}

	rows, err = tx.QueryContext(ctx, `
SELECT arg
FROM link_command_args
WHERE link_command_id = ?
ORDER BY pos;`,
		linkCommandID)
	if err != nil {
		log.Fatalf("unable to query link command args: %v", err)
	}
	defer rows.Close()
	args := []string{}
	var prevArg string
	for rows.Next() {
		var arg string
		if err := rows.Scan(&arg); err != nil {
			log.Fatalf("unable to scan link command arg: %v", err)
		}

		if arg == "PLACEHOLDER" {
			switch prevArg {
			case "-o":
				arg = binaryFile.Name()
			case "-importcfg":
				arg = importcfgFile.Name()
			}
		}

		args = append(args, arg)
		prevArg = arg
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("error reading link command rows: %v", err)
	}

	log.Printf("Link command: %s %s\n", *link, strings.Join(args, " "))
	out, err := exec.CommandContext(ctx, *link, args...).Output()
	log.Print(string(out))
	if err != nil {
		if err, ok := err.(*exec.ExitError); ok {
			log.Print(string(err.Stderr))
			os.Exit(err.ExitCode())
		}
		log.Fatalf("linker command failed: %v", err)
	}

	if os.Remove(importcfgFile.Name()) != nil {
		log.Fatalf("unable to remove importcfg file: %v", err)
	}

	log.Printf("Exec: %s %s\n", binaryFile.Name(), flag.Args()[1:])
	if err := syscall.Exec(binaryFile.Name(), flag.Args()[1:], os.Environ()); err != nil {
		log.Fatalf("exec failed: %v", err)
	}
}
