package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
	"qushuiyin/service/internal/httpapi"
)

func deletionCommand(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	const usage = "usage: deletions export | deletions replay <manifest.json>"
	if len(args) < 1 || (args[0] != "export" || len(args) != 1) && (args[0] != "replay" || len(args) != 2) {
		return errors.New(usage)
	}
	key, err := base64.StdEncoding.DecodeString(getenv("HMAC_KEY"))
	if err != nil || len(key) < 32 {
		return errors.New("HMAC_KEY must be base64 and at least 32 bytes")
	}
	defer clear(key)
	database, err := mysql.ParseDSN(getenv("MYSQL_DSN"))
	if err != nil || database.DBName == "" {
		return errors.New("MYSQL_DSN must identify a database")
	}
	database.ParseTime, database.Loc = true, time.UTC
	conn, err := sql.Open("mysql", database.FormatDSN())
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer conn.Close()
	if args[0] == "export" {
		if _, err = httpapi.ExportDeletions(ctx, conn, key, out); err != nil {
			return errors.New("export failed; check the database and manifest destination")
		}
		return nil
	}
	file, err := os.Open(args[1])
	if err != nil {
		return errors.New("cannot open deletion manifest")
	}
	defer file.Close()
	count, err := httpapi.ReplayDeletions(ctx, conn, key, file)
	if err != nil {
		return fmt.Errorf("deletion replay stopped after %d markers; check the manifest, key and database", count)
	}
	_, err = fmt.Fprintf(out, "replayed %d deletion markers\n", count)
	return err
}
