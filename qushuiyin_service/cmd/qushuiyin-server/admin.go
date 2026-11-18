package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
	"qushuiyin/service/internal/auth"
)

func adminCommand(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	flags := flag.NewFlagSet("admin create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	username := flags.String("username", "", "administrator account")
	role := flags.String("role", "", "first administrator role")
	if len(args) == 0 || args[0] != "create" || flags.Parse(args[1:]) != nil || flags.NArg() != 0 || !auth.ValidAdminUsername(*username) || *role != "super_admin" {
		return errors.New("usage: admin create --username <name> --role super_admin")
	}
	dsn := getenv("MYSQL_DSN")
	if dsn == "" {
		return errors.New("MYSQL_DSN required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer db.Close()
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("administrator password must be entered on an interactive terminal")
	}
	fmt.Fprint(out, "Password (12-256 bytes): ")
	password, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	defer clear(password)
	if err != nil || !auth.ValidAdminPassword(password) {
		return errors.New("invalid password input or length")
	}
	fmt.Fprint(out, "Repeat password: ")
	repeat, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	defer clear(repeat)
	if err != nil || !bytes.Equal(password, repeat) {
		return errors.New("passwords do not match")
	}
	store := auth.AdminStore{DB: db}
	err = store.Bootstrap(ctx, *username, password)
	if errors.Is(err, auth.ErrBootstrapComplete) {
		return err
	}
	if err != nil {
		return errors.New("cannot create administrator; check the database and migrations")
	}
	fmt.Fprintln(out, "first administrator created")
	return nil
}
