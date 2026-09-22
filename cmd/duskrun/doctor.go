package main

import (
	"fmt"

	"github.com/duskrun/duskrun/internal/core"
)

// cmdDoctor reports the presence and versions of the external tools duskrun
// shells out to. It always exits 0 — it is a diagnostic, not a gate.
func cmdDoctor() error {
	// age is a Go library in-process; the binary is only relevant if a operator
	// wants to decrypt artifacts by hand, so it is reported but never required.
	tools := []string{"pg_dump", "mysqldump", "mongodump", "redis-cli", "docker", "age"}

	fmt.Println("duskrun doctor — external tool check")
	for _, t := range tools {
		v, err := core.CheckTool(t)
		if err != nil {
			fmt.Printf("  %-12s MISSING — %v\n", t, err)
			continue
		}
		fmt.Printf("  %-12s %s\n", t, v)
	}
	return nil
}
