package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/duskrun/duskrun/internal/config"
	"github.com/duskrun/duskrun/internal/secret"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// cmdSecretSet seals a secret value and stores it.
// Usage: duskrun secret set <name> <type>
// Value is read from DUSKRUN_SECRET_VALUE, or stdin if that env is unset.
func cmdSecretSet(cfg *config.Config, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: duskrun secret set <name> <type>")
	}
	if err := cfg.RequireMasterKey(); err != nil {
		return err
	}
	name, typ := args[0], args[1]

	value := os.Getenv("DUSKRUN_SECRET_VALUE")
	if value == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read value from stdin: %w", err)
		}
		value = strings.TrimRight(string(b), "\n")
	}
	if value == "" {
		return fmt.Errorf("empty secret value (set DUSKRUN_SECRET_VALUE or pipe via stdin)")
	}

	box, err := secret.NewBox(cfg.MasterKey, "env:v1")
	if err != nil {
		return err
	}
	sealed, err := box.Seal(name, typ, []byte(value))
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if _, err := st.PutSecret(ctx, sealed); err != nil {
		return err
	}
	fmt.Printf("secret %q stored (type=%s, key=%s)\n", name, typ, box.KeyID())
	return nil
}
