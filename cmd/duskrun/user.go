package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/duskrun/duskrun/internal/auth"
	"github.com/duskrun/duskrun/internal/config"
	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// cmdUser dispatches `duskrun user <add|list|passwd>`.
//
// This is the bootstrap path: until an account exists there is nobody to create
// one over HTTP, so the first administrator is made here. Unlike serve, none of
// these need the master key — passwords are hashed, not encrypted, so no key is
// involved.
func cmdUser(cfg *config.Config, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		return cmdUserAdd(cfg, args[1:])
	case "list":
		return cmdUserList(cfg, args[1:])
	case "passwd":
		return cmdUserPasswd(cfg, args[1:])
	default:
		return errors.New("usage: duskrun user <add|list|passwd> [flags]")
	}
}

func cmdUserAdd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	email := fs.String("email", "", "login email (required)")
	role := fs.String("role", string(core.RoleAdmin), "role: admin | operator | viewer")
	name := fs.String("name", "", "display name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	addr := strings.ToLower(strings.TrimSpace(*email))
	if addr == "" {
		return errors.New("user add: --email is required")
	}
	r := core.Role(strings.TrimSpace(*role))
	if !r.Valid() {
		return fmt.Errorf("user add: unknown role %q (admin, operator or viewer)", *role)
	}

	password, err := readPassword()
	if err != nil {
		return err
	}
	hash, err := auth.Hash(password)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	id, err := st.CreateUser(ctx, core.User{Email: addr, Name: strings.TrimSpace(*name), Role: r, PasswordHash: hash})
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED") {
			return fmt.Errorf("user add: %q already exists (use `duskrun user passwd`)", addr)
		}
		return err
	}
	fmt.Printf("user %q created (id=%d, role=%s)\n", addr, id, r)
	return nil
}

func cmdUserList(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	st, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	users, err := st.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Println("no users yet — sign in with DUSKRUN_API_TOKEN, or run `duskrun user add`")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tNAME\tROLE\tSTATE\tLAST LOGIN")
	for _, u := range users {
		state := "enabled"
		if u.Disabled {
			state = "disabled"
		}
		last := "never"
		if u.LastLoginAt != nil {
			last = u.LastLoginAt.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", u.ID, u.Email, u.Name, u.Role, state, last)
	}
	return tw.Flush()
}

func cmdUserPasswd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
	email := fs.String("email", "", "login email (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	addr := strings.ToLower(strings.TrimSpace(*email))
	if addr == "" {
		return errors.New("user passwd: --email is required")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}
	hash, err := auth.Hash(password)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	u, err := st.GetUserByEmail(ctx, addr)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			return fmt.Errorf("user passwd: no user %q", addr)
		}
		return err
	}
	// keep = 0: a reset from a shell has no session of its own to spare, and an
	// operator resetting a password almost always wants every client kicked.
	if err := st.SetUserPassword(ctx, u.ID, hash, 0); err != nil {
		return err
	}
	fmt.Printf("password for %q updated; all sessions of this user were ended\n", addr)
	return nil
}

// minCLIPasswordLen mirrors the API's floor so the two paths cannot disagree
// about what counts as an acceptable password.
const minCLIPasswordLen = 10

// readPassword takes the password from DUSKRUN_USER_PASSWORD or stdin.
//
// Never from a flag: an argument lands in shell history and in the process list
// of every other user on the host. This mirrors `duskrun secret set`.
func readPassword() (string, error) {
	if v := os.Getenv("DUSKRUN_USER_PASSWORD"); v != "" {
		return validateCLIPassword(v)
	}
	fmt.Fprintln(os.Stderr, "reading password from stdin…")
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	return validateCLIPassword(strings.TrimRight(string(b), "\r\n"))
}

func validateCLIPassword(p string) (string, error) {
	if len([]rune(p)) < minCLIPasswordLen {
		return "", fmt.Errorf("password must be at least %d characters (set DUSKRUN_USER_PASSWORD or pipe via stdin)", minCLIPasswordLen)
	}
	return p, nil
}
