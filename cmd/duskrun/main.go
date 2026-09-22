// Command duskrun is the single static binary: CLI + daemon. This is the M0
// skeleton — it wires config, logging, the metadata store, and the plugin
// registries, and exposes stub subcommands that later milestones fill in.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/duskrun/duskrun/internal/config"
	"github.com/duskrun/duskrun/internal/obs"
	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/store/sqlite"

	// Blank-import plugin packages so their init() registers them. The core
	// never imports concretes directly — this is the only wiring point.
	_ "github.com/duskrun/duskrun/internal/codec/age"             // M3: age encryption
	_ "github.com/duskrun/duskrun/internal/codec/gzip"            // gzip compression
	_ "github.com/duskrun/duskrun/internal/codec/tar"             // tar archive (→ .tar.gz with gzip)
	_ "github.com/duskrun/duskrun/internal/codec/zstd"            // M1
	_ "github.com/duskrun/duskrun/internal/connector/dockerproxy" // docker-proxy
	_ "github.com/duskrun/duskrun/internal/connector/local"       // M1: direct, socket
	_ "github.com/duskrun/duskrun/internal/connector/sshtunnel"   // M4: ssh-tunnel
	_ "github.com/duskrun/duskrun/internal/dumper/mongodb"        // mongodump
	_ "github.com/duskrun/duskrun/internal/dumper/mysql"          // M3: mysqldump
	_ "github.com/duskrun/duskrun/internal/dumper/postgres"       // M1
	_ "github.com/duskrun/duskrun/internal/dumper/redis"          // redis-cli --rdb
	_ "github.com/duskrun/duskrun/internal/notifier/logn"         // M4: log
	_ "github.com/duskrun/duskrun/internal/notifier/smtp"         // email
	_ "github.com/duskrun/duskrun/internal/notifier/telegram"     // M4: telegram
	_ "github.com/duskrun/duskrun/internal/notifier/webhook"      // M4: webhook
	_ "github.com/duskrun/duskrun/internal/storage/localfs"       // M1
	_ "github.com/duskrun/duskrun/internal/storage/s3"            // M3: s3 multipart
	_ "github.com/duskrun/duskrun/internal/storage/sftp"          // sftp over ssh
)

// version is stamped at build time with -ldflags "-X main.version=…" (see the
// Makefile and Dockerfile). It is deliberately empty rather than "0.0.0-dev":
// a hardcoded default is indistinguishable from a real stamp that failed, and
// the Docker image spent its whole life reporting a version nobody set.
var version string

// resolveVersion reports the build's version, falling back through what is
// actually knowable.
//
// The ldflags stamp is the authoritative answer. Without it, Go records the VCS
// revision for any `go build` inside a checkout, which covers `go install` and
// a developer's local build. Docker gets neither by default — .dockerignore
// excludes .git, so the image must be built with --build-arg VERSION — and an
// honest "unknown" beats a fake number when someone is trying to work out which
// build is running in production.
func resolveVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "unknown"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified == "true" {
		// Uncommitted changes: the revision alone would misidentify the build.
		return revision + "-dirty"
	}
	return revision
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "duskrun: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "help"
	if len(args) > 0 {
		cmd = args[0]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := obs.NewLogger(cfg.LogFormat)

	switch cmd {
	case "serve":
		return cmdServe(cfg, log) // M2+ fills the engine in
	case "run":
		return cmdRun(cfg, args[1:])
	case "secret":
		if len(args) >= 2 && args[1] == "set" {
			return cmdSecretSet(cfg, args[2:])
		}
		return fmt.Errorf("usage: duskrun secret set <name> <type>")
	case "user":
		return cmdUser(cfg, args[1:])
	case "plugins":
		printPlugins()
		return nil
	case "doctor":
		return cmdDoctor()
	case "migrate":
		ctx := context.Background()
		st, err := sqlite.Open(ctx, cfg.DBPath)
		if err != nil {
			return err
		}
		defer st.Close()
		log.Info("migrations applied", "db", cfg.DBPath)
		return nil
	case "version":
		fmt.Println("duskrun " + resolveVersion())
		return nil
	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		printHelp()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func printPlugins() {
	fmt.Println("connectors:", plugin.Connectors.Names())
	fmt.Println("dumpers:   ", plugin.Dumpers.Names())
	fmt.Println("codecs:    ", plugin.Codecs.Names())
	fmt.Println("storages:  ", plugin.Storages.Names())
	fmt.Println("notifiers: ", plugin.Notifiers.Names())
}

func printHelp() {
	fmt.Print(`duskrun — self-hosted database backup

usage: duskrun <command>

commands:
  run <task>        execute one task now (dump → codec → storage)
  secret set <n> <t>  seal and store a secret (value via stdin or DUSKRUN_SECRET_VALUE)
  user add          create a login (--email, --role, --name; password via stdin)
  user list         list logins
  user passwd       set a login's password (--email; password via stdin)
  serve             run the daemon (scheduler + workers + API)   [M2+]
  migrate           apply metadata migrations and exit
  plugins           list registered plugins
  doctor            check external tools (pg_dump, mysqldump, age)
  version           print version
  help              show this help
`)
}
