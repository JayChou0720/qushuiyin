package db

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"github.com/pressly/goose/v3"
	"os"
	"qushuiyin/service/migrations"
	"testing"
)

func TestObserveMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("H3: no dedicated MySQL test DSN")
	}
	conn, e := sql.Open("mysql", dsn)
	if e != nil {
		t.Fatal("test connection configuration invalid")
	}
	defer conn.Close()
	ctx := t.Context()
	var name string
	if conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name) != nil || name != "qushuiyin_test" {
		t.Fatal("requires qushuiyin_test")
	}
	provider, e := goose.NewProvider(goose.DialectMySQL, conn, migrations.FS)
	if e != nil {
		t.Fatal("migration setup")
	}
	if _, e = provider.Up(ctx); e != nil {
		t.Fatal("test migration failed")
	}
	var nonce [16]byte
	rand.Read(nonce[:])
	h := fmt.Sprintf("%x.test", nonce)
	d := Domains{conn}
	s, e := d.Observe(ctx, "douyin", h)
	if e != nil || s != "observed" {
		t.Fatal(s, e)
	}
	defer conn.Exec("DELETE FROM download_domain WHERE host=?", h)
	if _, e = conn.ExecContext(ctx, "UPDATE download_domain SET status='registered' WHERE host=?", h); e != nil {
		t.Fatal("setup failed")
	}
	s, e = d.Observe(ctx, "douyin", h)
	var count int
	if e != nil || s != "registered" || conn.QueryRowContext(ctx, "SELECT seen_count FROM download_domain WHERE host=?", h).Scan(&count) != nil || count != 2 {
		t.Fatal("repeat observation changed registration or count")
	}
}
