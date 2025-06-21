// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

var logInfof = log.Printf
var logDebugf = log.Printf

func main() {
	ctx := context.Background()

	config, err := parseConfig(ctx)
	if err != nil {
		log.Fatalf("Error: unable to parse config: %v", err)
	}

	// Open the database
	db, err := sql.Open("sqlite3", "file:"+config.dbPath+"?mode=ro&_foreign_keys=true")
	if err != nil {
		log.Fatalf("Error: unable to open database %q: %v", config.dbPath, err)
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Fatalf("Error: unable to begin transaction: %v", err) //nolint:gocritic
	}
	defer tx.Rollback() //nolint:errcheck

	linkCommandID, mainPackage, err := getLinkCommandID(ctx, tx, config.binaryName, config.buildTags)
	if err != nil {
		log.Fatalf("Error: unable to get link command ID: %v", err)
	}

	importcfgFileName, err := getImportcfg(ctx, tx, linkCommandID)
	if err != nil {
		log.Fatalf("Error: unable to get importcfg: %v", err)
	}

	binaryFile, err := os.CreateTemp("", config.binaryName)
	if err != nil {
		log.Fatalf("Error: unable to create binary file: %v", err)
	}

	args, err := getLinkerCommandArgs(ctx, tx, linkCommandID, mainPackage, binaryFile.Name(), importcfgFileName)
	if err != nil {
		log.Fatalf("Error: unable to get link command args: %v", err)
	}

	// Invoke the linker
	logInfof("Link command: %s %s", config.linker, strings.Join(args, " "))
	out, err := exec.CommandContext(ctx, config.linker, args...).Output() //nolint:gosec
	logInfof("%s", out)
	if err != nil {
		if err, ok := err.(*exec.ExitError); ok {
			log.Print(string(err.Stderr))
			os.Exit(err.ExitCode())
		}
		log.Fatalf("Error: linker command failed: %v", err)
	}

	if err := os.Remove(importcfgFileName); err != nil {
		log.Fatalf("Error: unable to remove importcfg file: %v", err)
	}

	logInfof("Exec: %s %s", binaryFile.Name(), config.args)
	if err := syscall.Exec(binaryFile.Name(), append([]string{config.binaryName}, config.args...), os.Environ()); err != nil { //nolint:gosec
		log.Fatalf("Error: exec failed: %v", err)
	}
}

type Config struct {
	dbPath     string
	linker     string
	binaryName string
	buildTags  []string
	args       []string
}

func parseConfig(_ context.Context) (config Config, err error) {
	logLevel := flag.Uint("log-level", 0, "Log level (0 = silent, 1 = info, 2 = debug)")
	flag.StringVar(&config.dbPath, "db", "link.db", "Path to the sqlite DB")
	flag.StringVar(&config.linker, "link", "", "File path to the linker executable (Should be \"$(go env GOTOOLDIR)/link\")")
	tags := flag.String("tags", "", "Build tags to use")
	flag.Parse()
	if len(flag.Args()) < 1 {
		fmt.Fprintln(os.Stderr, "Need an executable name")
		flag.Usage()
		os.Exit(2)
	}

	config.binaryName = flag.Arg(0)
	config.args = flag.Args()[1:]
	if *tags != "" {
		config.buildTags = strings.Split(*tags, ",")
		slices.Sort(config.buildTags)
	}

	switch {
	case *logLevel < 1:
		logInfof = func(string, ...any) {}
		fallthrough
	case *logLevel < 2:
		logDebugf = func(string, ...any) {}
	}

	return
}

func getLinkCommandID(ctx context.Context, tx *sql.Tx, binaryName string, buildTags []string) (linkCommandID int, mainPackage string, err error) {
	buildTagsJSON, err := json.Marshal(buildTags)
	if err != nil {
		return 0, "", fmt.Errorf("unable to marshal build tags: %w", err)
	}

	row := tx.QueryRowContext(ctx, `
SELECT link_command_id, package_file.file
FROM link_command
NATURAL JOIN build_tags
LEFT JOIN package_file ON link_command.main_package_id = package_file.package_file_id
WHERE binary_name = ? AND tags = jsonb(?);`,
		binaryName, buildTagsJSON)
	if err := row.Scan(&linkCommandID, &mainPackage); err != nil {
		if err == sql.ErrNoRows {
			fmt.Fprintf(os.Stderr, "No link command found for %q with build tags %q\n", binaryName, buildTags)
			os.Exit(1)
		}
		return 0, "", fmt.Errorf("unable to query link command ID: %w", err)
	}

	return
}

func getImportcfg(ctx context.Context, tx *sql.Tx, linkCommandID int) (importcfgFileName string, err error) {
	importcfgFile, err := os.CreateTemp("", "importcfg.link")
	if err != nil {
		return "", fmt.Errorf("unable to create importcfg file: %w", err)
	}
	defer func() {
		if err2 := importcfgFile.Close(); err2 != nil {
			err = errors.Join(err, fmt.Errorf("unable to close importcfg file: %w", err2))
		}
	}()
	importcfgFileName = importcfgFile.Name()

	rows, err := tx.QueryContext(ctx, `
SELECT 'packagefile ' || package || '=' || file
FROM package_file
NATURAL JOIN link_command_package_file
WHERE link_command_id = ?
UNION
SELECT line
FROM importcfg_additional_lines
WHERE link_command_id = ?;`,
		linkCommandID, linkCommandID)
	if err != nil {
		return "", fmt.Errorf("unable to query importcfg: %w", err)
	}
	defer func() {
		if err2 := rows.Close(); err2 != nil {
			err = errors.Join(err, fmt.Errorf("unable to close importcfg rows: %w", err2))
		}
	}()

	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("unable to scan importcfg line: %w", err)
		}
		logDebugf("%s --- %s", importcfgFile.Name(), line)
		if _, err := fmt.Fprintln(importcfgFile, line); err != nil {
			return "", fmt.Errorf("unable to write importcfg line: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("error reading importcfg rows: %w", err)
	}

	return
}

func getLinkerCommandArgs(ctx context.Context, tx *sql.Tx, linkCommandID int, mainPackage, binaryFileName, importcfgFileName string) (args []string, err error) {
	rows, err := tx.QueryContext(ctx, `
SELECT arg
FROM link_command_args
WHERE link_command_id = ?
ORDER BY pos;`,
		linkCommandID)
	if err != nil {
		return nil, fmt.Errorf("unable to query link command args: %w", err)
	}
	defer func() {
		if err2 := rows.Close(); err2 != nil {
			err = errors.Join(err, fmt.Errorf("unable to close link command args rows: %w", err2))

		}
	}()

	var prevArg string
	for rows.Next() {
		var arg string
		if err := rows.Scan(&arg); err != nil {
			return nil, fmt.Errorf("unable to scan link command arg: %w", err)
		}

		if arg == "PLACEHOLDER" {
			switch prevArg {
			case "-o":
				arg = binaryFileName
			case "-importcfg":
				arg = importcfgFileName
			}
		}

		if arg == "MAIN PACKAGE" {
			arg = mainPackage
		}

		args = append(args, arg)
		prevArg = arg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error reading link command rows: %w", err)
	}

	return
}
