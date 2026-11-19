package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pressly/goose/v3"
	"io"
	"os"
	"os/signal"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/db"
	"qushuiyin/service/internal/media"
	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
	"qushuiyin/service/migrations"
	"time"
)

type report struct {
	Mode      string              `json:"mode"`
	Version   string              `json:"adapterVersion"`
	At        time.Time           `json:"sampledAt"`
	Platform  string              `json:"platform"`
	ID        *string             `json:"contentId"`
	Kind      string              `json:"mediaType"`
	Count     int                 `json:"assetCount"`
	Mixed     bool                `json:"imagesAndVideo"`
	Warnings  []string            `json:"warnings"`
	Resources []media.Observation `json:"resources"`
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	return runWithProbeAdapter(ctx, args, getenv, out, nil)
}

// The live batch keeps one adapter so platform limits span all its samples.
func runWithProbeAdapter(ctx context.Context, args []string, getenv func(string) string, out io.Writer, shared *parse.Adapter) error {
	if len(args) > 0 && args[0] == "admin" {
		return adminCommand(ctx, args[1:], getenv, out)
	}
	if len(args) > 0 && args[0] == "deletions" {
		return deletionCommand(ctx, args[1:], getenv, out)
	}
	if len(args) == 1 && args[0] == "serve" {
		return mockCommand(ctx, "serve", getenv, out)
	}
	if len(args) == 2 && args[0] == "dev" && args[1] == "seed" {
		return mockCommand(ctx, "seed", getenv, out)
	}
	if len(args) != 2 || (args[0] != "probe" && (args[0] != "migrate" || (args[1] != "up" && args[1] != "status"))) {
		return errors.New("usage: qushuiyin-server serve | dev seed | migrate up | migrate status | admin create | deletions export | deletions replay <manifest.json> | probe <share-url>")
	}
	l := config.Defaults()
	mode := getenv("PARSE_MODE")
	env := getenv("APP_ENV")
	if mode == "" {
		mode = "live"
	}
	if args[0] == "probe" && (env != "local" && env != "test") {
		return errors.New("probe requires APP_ENV=local or test")
	}
	if mode != "mock" && mode != "live" {
		return errors.New("invalid PARSE_MODE")
	}
	if mode == "live" && (getenv("PARSE_VIDEO_DOUYIN_COOKIE") != "" || getenv("MOCK_MEDIA_ORIGIN") != "") {
		return errors.New("live mode rejects cookie and mock overrides")
	}
	var store media.Domains
	dsn := getenv("MYSQL_DSN")
	if args[0] == "migrate" || (mode == "live" && dsn != "") {
		if dsn == "" {
			return errors.New("MYSQL_DSN required")
		}
		conn, e := sql.Open("mysql", dsn)
		if e != nil {
			return errors.New("invalid database configuration")
		}
		defer conn.Close()
		if args[0] == "migrate" {
			provider, e := goose.NewProvider(goose.DialectMySQL, conn, migrations.FS)
			if e != nil {
				return errors.New("migration initialization failed")
			}
			if args[1] == "up" {
				if _, e = provider.Up(ctx); e != nil {
					return errors.New("migration failed")
				}
				fmt.Fprintln(out, "up to date")
				return nil
			}
			states, e := provider.Status(ctx)
			if e != nil {
				return errors.New("migration status unavailable")
			}
			for _, s := range states {
				fmt.Fprintln(out, s.Source.Version, s.State)
			}
			return nil
		}
		values, e := config.Load(ctx, conn)
		if e != nil {
			return e
		}
		l = values.Limits()
		l.Upstream, e = values.Upstream()
		if e != nil {
			return e
		}
		if e = values.Validate("parse_job_timeout_seconds", "parser_call_timeout_seconds"); e != nil {
			return e
		}
		store = db.Domains{DB: conn}
	}
	client := outbound.Client{Transport: outbound.Transport(), Redirects: l.Redirects}
	policy := parse.ValidationPolicy()
	var adapter parse.Adapter
	if mode == "mock" {
		var e error
		adapter, e = parse.Mock(l, policy)
		if e != nil {
			return errors.New("invalid fixtures")
		}
		client.Transport = media.FixtureHeaders{}
	} else if shared != nil && shared.Resolve != nil {
		adapter = *shared
	} else {
		adapter = parse.Live(l, policy, client)
		if shared != nil {
			*shared = adapter
		}
	}
	if mode == "live" {
		ctx = parse.StopOnVerificationPage(ctx)
	}
	r, e := adapter.Parse(ctx, args[1])
	if e != nil {
		return e
	}
	version := parse.Version
	if mode == "live" && r.Platform == "douyin" {
		version = parse.DouyinH5Version
	}
	rep := report{Mode: mode, Version: version, At: time.Now().UTC(), Platform: r.Platform, ID: r.ContentID, Kind: r.MediaType, Count: len(r.Assets), Mixed: r.Mixed, Warnings: r.Warnings}
	probe := media.Probe{Client: client, Limits: l, Domains: store}
	batch, cancel := context.WithTimeout(ctx, l.Batch)
	defer cancel()
	assets := r.Assets
	if r.Cover != nil {
		assets = append(assets, *r.Cover)
	}
	failedStore := false
	for _, asset := range assets {
		obs := probe.Inspect(batch, r.Platform, asset)
		failedStore = failedStore || obs.Reason == "DOMAIN_STORE_FAILED"
		rep.Resources = append(rep.Resources, obs)
	}
	if e = json.NewEncoder(out).Encode(rep); e != nil {
		return errors.New("output failed")
	}
	if failedStore {
		return errors.New("domain observation was not persisted")
	}
	return nil
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		failure := map[string]any{"error": err.Error()}
		if errors.Is(err, parse.ErrUpstreamChallenge) {
			failure["verificationPage"] = true
		}
		json.NewEncoder(os.Stderr).Encode(failure)
		os.Exit(1)
	}
}
