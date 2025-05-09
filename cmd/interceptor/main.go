package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const dbPath = "link.db"

func main() {
	ctx := context.Background()

	config, err := parseConfig(ctx)
	if err != nil {
		log.Fatalf("Error: unable to parse config: %v", err)
	}

	// Build the program once to compile all packages in the cache
	out, err := exec.CommandContext(ctx, os.Args[1], os.Args[2:]...).CombinedOutput() //nolint:gosec
	fmt.Println(string(out))
	if err != nil {
		if err, ok := err.(*exec.ExitError); ok {
			os.Exit(err.ExitCode())
		}
		log.Fatalf("Error: unable to build program: %v", err)
	}

	// Re-build the program to get the link command
	err = os.Remove(config.binaryName)
	if err != nil {
		log.Fatalf("Error: unable to remove output file %s: %v", config.binaryName, err)
	}

	args := []string{os.Args[2], "-x"}
	args = append(args, os.Args[3:]...)
	out, err = exec.CommandContext(ctx, os.Args[1], args...).CombinedOutput() //nolint:gosec
	if err != nil {
		log.Fatalf("Error: unable to get link command: %v\n%s", err, out)
	}

	// Extract the link command from the `go build -x` output
	linkCommands, filesContent, err := parseGoBuildOutput(ctx, out)
	if err != nil {
		log.Fatalf("Error: unable to parse Go build output: %v", err)
	}

	err = writeToDB(ctx, config, linkCommands, filesContent)
	if err != nil {
		log.Fatalf("Error: unable to write to database: %v", err)
	}
}

type Config struct {
	binaryName string
	buildTags  []string
}

func parseConfig(_ context.Context) (config Config, err error) {
	if len(os.Args) < 3 || os.Args[1] != "go" || os.Args[2] != "build" {
		fmt.Printf("Usage: %s go build -o output [build flags] [packages]", os.Args[0])
		os.Exit(1)
	}

	for i, arg := range os.Args[:len(os.Args)-1] {
		switch arg {
		case "-o":
			config.binaryName = os.Args[i+1]
		case "-tags", "--tags":
			config.buildTags = strings.Split(os.Args[i+1], ",")
			slices.Sort(config.buildTags)
		}
	}
	if config.binaryName == "" {
		return Config{}, errors.New("Error: -o flag is required")
	}

	return
}

func getGoEnvVar(ctx context.Context) (goEnv map[string]string, err error) {
	out, err := exec.CommandContext(ctx, "go", "env", "-json").Output()
	if err != nil {
		if err, ok := err.(*exec.ExitError); ok {
			os.Stderr.Write(err.Stderr)
			os.Exit(err.ExitCode())
		}
		return nil, fmt.Errorf("unable to get Go environment: %w", err)
	}

	err = json.Unmarshal(out, &goEnv)

	return
}

func parseGoBuildOutput(ctx context.Context, out []byte) (linkCommands []string, filesContent map[string][]string, err error) {
	goEnv, err := getGoEnvVar(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get Go environment variables: %w", err)
	}

	envVarDefRe := regexp.MustCompile(`^(\w+)=(\S*)$`)
	envVarRe := regexp.MustCompile(`\$\w+`)
	startFileRe := regexp.MustCompile(`^cat > *(\S+) *<< 'EOF' *(?:#.*)?$`)
	endFileRe := regexp.MustCompile(`^EOF$`)
	linkCommandRe := regexp.MustCompile(`^.*` + regexp.QuoteMeta(goEnv["GOTOOLDIR"]+"/link") + ` (.*)$`)

	filesContent = make(map[string][]string)
	linkCommands = make([]string, 0, 1)

	currentFile := ""
	envVarMap := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := envVarRe.ReplaceAllStringFunc(scanner.Text(), func(s string) string {
			if val, ok := envVarMap[s[1:]]; ok {
				return val
			}
			return s
		})
		switch {
		case envVarDefRe.MatchString(line):
			if matches := envVarDefRe.FindStringSubmatch(line); matches != nil {
				envVarMap[matches[1]] = matches[2]
			}
			log.Printf("Environment variable --- %s", line)
		case endFileRe.MatchString(line):
			log.Printf("End of file %q     --- %s", currentFile, line)
			currentFile = ""
		case currentFile != "":
			log.Printf("Content of file %q --- %s", currentFile, line)
			filesContent[currentFile] = append(filesContent[currentFile], line)
		case startFileRe.MatchString(line):
			if matches := startFileRe.FindStringSubmatch(line); matches != nil {
				currentFile = matches[1]
			}
			log.Printf("Start of file %q   --- %s", currentFile, line)
		case linkCommandRe.MatchString(line):
			if matches := linkCommandRe.FindStringSubmatch(line); matches != nil {
				linkCommands = append(linkCommands, matches[1])
			}
			log.Printf("Link command found --- %s", line)
		default:
			log.Printf("Ignored line --- %s", line)
		}
	}

	return
}

func writeToDB(ctx context.Context, config Config, linkCommands []string, filesContent map[string][]string) (err error) {
	db, err := openOrCreateDB(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("unable to open or create database: %w", err)
	}
	defer func() {
		if err2 := db.Close(); err2 != nil {
			err = errors.Join(err, fmt.Errorf("unable to close database: %w", err2))
		}
	}()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("unable to begin transaction: %w", err)
	}
	defer func() {
		if err2 := tx.Commit(); err2 != nil {
			err = errors.Join(err, fmt.Errorf("unable to commit transaction: %w", err2))
		}
	}()

	buildTagsID, err := insertBuildTags(ctx, tx, config.buildTags)
	if err != nil {
		return fmt.Errorf("unable to insert build tags into database: %w", err)
	}

	for _, linkCommand := range linkCommands {
		linkCommandID, importcfg, err := insertLinkCommand(ctx, tx, config.binaryName, buildTagsID, linkCommand)
		if err != nil {
			return fmt.Errorf("unable to insert link command into database: %w", err)
		}

		for _, line := range filesContent[importcfg] {
			if strings.HasPrefix(line, "packagefile") {
				if err := insertPackageFile(ctx, tx, linkCommandID, line); err != nil {
					return fmt.Errorf("unable to insert package file into database: %w", err)
				}
			} else {
				if err := insertAdditionalLines(ctx, tx, linkCommandID, line); err != nil {
					return fmt.Errorf("unable to insert additional lines into database: %w", err)
				}
			}
		}

		err = updateLinkCommand(ctx, tx, linkCommandID)
		if err != nil {
			return fmt.Errorf("unable to update link command in database: %w", err)
		}
	}

	return nil
}

func openOrCreateDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc&_foreign_keys=true")
	if err != nil {
		return nil, fmt.Errorf("unable to open database %q: %w", dbPath, err)
	}

	for _, sqlStmt := range []string{
		`
CREATE TABLE IF NOT EXISTS link_command (
	link_command_id INTEGER PRIMARY KEY AUTOINCREMENT,
	binary_name     TEXT    NOT NULL,
	build_tags_id   INTEGER NOT NULL,
	main_package_id INTEGER,
	UNIQUE (binary_name, build_tags_id),
	FOREIGN KEY (build_tags_id) REFERENCES build_tags(build_tags_id),
	FOREIGN KEY (main_package_id) REFERENCES package_file(package_file_id)
);`,
		`
CREATE TABLE IF NOT EXISTS link_command_args (
	link_command_id INTEGER NOT NULL,
	pos             INTEGER NOT NULL,
	arg             TEXT    NOT NULL,
	PRIMARY KEY (link_command_id, pos),
	FOREIGN KEY (link_command_id) REFERENCES link_command(link_command_id)
);`,
		`
CREATE TABLE IF NOT EXISTS build_tags (
	build_tags_id INTEGER PRIMARY KEY AUTOINCREMENT,
	tags          JSONB NOT NULL UNIQUE
);`,
		`
CREATE TABLE IF NOT EXISTS package_file (
	package_file_id INTEGER PRIMARY KEY AUTOINCREMENT,
	package         TEXT    NOT NULL,
	file            TEXT    NOT NULL UNIQUE
);`,
		`
CREATE TABLE IF NOT EXISTS link_command_package_file (
	link_command_id INTEGER NOT NULL,
	package_file_id INTEGER NOT NULL,
	PRIMARY KEY (link_command_id, package_file_id),
	FOREIGN KEY (link_command_id) REFERENCES link_command(link_command_id),
	FOREIGN KEY (package_file_id) REFERENCES package_file(package_file_id)
);`,
		`
CREATE TABLE IF NOT EXISTS importcfg_additional_lines (
	link_command_id INTEGER NOT NULL,
	line            TEXT    NOT NULL,
	PRIMARY KEY (link_command_id, line),
	FOREIGN KEY (link_command_id) REFERENCES link_command(link_command_id)
);`,
	} {
		_, err = db.ExecContext(ctx, sqlStmt)
		if err != nil {
			return nil, fmt.Errorf("unable to create table: %w", err)
		}
	}

	return db, nil
}

func insertBuildTags(ctx context.Context, tx *sql.Tx, buildTags []string) (int64, error) {
	buildTagsJSON, err := json.Marshal(buildTags)
	if err != nil {
		return 0, fmt.Errorf("unable to marshal build tags: %w", err)
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO build_tags (tags) VALUES (jsonb(?)) ON CONFLICT DO NOTHING;`, buildTagsJSON)
	if err != nil {
		return 0, fmt.Errorf("unable to insert build tags: %w", err)
	}

	if rowsAffected, err := result.RowsAffected(); err == nil && rowsAffected == 1 {
		if lastInsertID, err := result.LastInsertId(); err == nil {
			return lastInsertID, nil
		}
	}

	row := tx.QueryRowContext(ctx, `SELECT build_tags_id FROM build_tags WHERE tags = jsonb(?);`, buildTagsJSON)
	var buildTagsID int64
	if err := row.Scan(&buildTagsID); err != nil {
		return 0, fmt.Errorf("unable to get build tags ID: %w", err)
	}

	return buildTagsID, nil
}

func insertLinkCommand(ctx context.Context, tx *sql.Tx, binaryName string, buildTagsID int64, linkCommand string) (int64, string, error) {

	result, err := tx.ExecContext(ctx, `INSERT INTO link_command (binary_name, build_tags_id) VALUES (?, ?);`, binaryName, buildTagsID)
	if err != nil {
		return 0, "", fmt.Errorf("unable to insert link command: %w", err)
	}

	var linkCommandID int64
	if rowsAffected, err := result.RowsAffected(); err == nil && rowsAffected == 1 {
		if lastInsertID, err := result.LastInsertId(); err == nil {
			linkCommandID = lastInsertID
		}
	} else {
		row := tx.QueryRowContext(ctx, `SELECT link_command_id FROM link_command WHERE binary_name = ? AND build_tags_id = ?;`, binaryName, buildTagsID)
		if err := row.Scan(&linkCommandID); err != nil {
			return 0, "", fmt.Errorf("unable to get link command ID: %w", err)
		}
	}

	// Split the link command into arguments
	// Broken if there’s quotes
	var importcfg string
	var prevArg string
	for i, arg := range strings.Split(linkCommand, " ") {
		switch prevArg {
		case "-o":
			arg = "PLACEHOLDER"
		case "-importcfg":
			importcfg = arg
			arg = "PLACEHOLDER"
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO link_command_args (link_command_id, pos, arg) VALUES (?, ?, ?);`, linkCommandID, i, arg); err != nil {
			return 0, "", fmt.Errorf("unable to insert link command argument: %w", err)
		}
		prevArg = arg
	}

	return linkCommandID, importcfg, nil
}

func insertPackageFile(ctx context.Context, tx *sql.Tx, linkCommandID int64, line string) error {
	directive, argument, ok := strings.Cut(line, " ")
	if !ok || directive != "packagefile" {
		return fmt.Errorf("invalid line: %s", line)
	}

	packageName, file, ok := strings.Cut(argument, "=")
	if !ok {
		return fmt.Errorf("invalid line: %s", line)
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO package_file (package, file) VALUES (?, ?) ON CONFLICT DO NOTHING;`, packageName, file)
	if err != nil {
		return fmt.Errorf("unable to insert package file: %w", err)
	}

	var packageFileID int64
	if rowsAffected, err := result.RowsAffected(); err == nil && rowsAffected == 1 {
		if lastInsertID, err := result.LastInsertId(); err == nil {
			packageFileID = lastInsertID
		}
	} else {
		row := tx.QueryRowContext(ctx, `SELECT package_file_id FROM package_file WHERE package = ? AND file = ?;`, packageName, file)
		if err := row.Scan(&packageFileID); err != nil {
			return fmt.Errorf("unable to get package file ID: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO link_command_package_file (link_command_id, package_file_id) VALUES (?, ?);`, linkCommandID, packageFileID)
	if err != nil {
		return fmt.Errorf("unable to insert link command package file: %w", err)
	}

	return nil
}

func updateLinkCommand(ctx context.Context, tx *sql.Tx, linkCommandID int64) error {
	_, err := tx.ExecContext(ctx, `
UPDATE link_command
SET main_package_id = (
	SELECT package_file_id
	FROM package_file
	WHERE file = (
		SELECT arg
		FROM link_command_args
		WHERE link_command_id = ?
		ORDER BY pos DESC
		LIMIT 1
	)
)
WHERE link_command_id = ?;
`, linkCommandID, linkCommandID)
	if err != nil {
		return fmt.Errorf("unable to update link command: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
UPDATE link_command_args
SET arg = "MAIN PACKAGE"
WHERE link_command_id = ?
	AND arg = (
		SELECT file
		FROM package_file
		WHERE package_file_id = (
			SELECT main_package_id
			FROM link_command
			WHERE link_command_id = ?
		)
	);
`, linkCommandID, linkCommandID)
	if err != nil {
		return fmt.Errorf("unable to update link command args: %w", err)
	}

	return nil
}

func insertAdditionalLines(ctx context.Context, tx *sql.Tx, linkCommandID int64, line string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO importcfg_additional_lines (link_command_id, line) VALUES (?, ?);`, linkCommandID, line)
	if err != nil {
		return fmt.Errorf("unable to insert additional lines: %w", err)
	}

	return nil
}
