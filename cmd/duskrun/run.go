package main

import (
	"context"
	"fmt"
	"time"

	"github.com/duskrun/duskrun/internal/config"
	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/secret"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// cmdRun executes one task's pipeline synchronously ("Run now", TZ §7),
// recording a run and its artifact. Usage: duskrun run <task-name>
//
// It is a thin wrapper: all pipeline assembly (connector/dumper/codecs/storage,
// credential decryption, artifact key, RunPipeline, InsertArtifact/Finish) lives
// in core.Executor, shared with the worker pool.
func cmdRun(cfg *config.Config, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: duskrun run <task-name>")
	}
	if err := cfg.RequireMasterKey(); err != nil {
		return err
	}
	taskName := args[0]
	ctx := context.Background()

	box, err := secret.NewBox(cfg.MasterKey, "env:v1")
	if err != nil {
		return err
	}
	st, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	task, err := st.GetTaskByName(ctx, taskName)
	if err != nil {
		return fmt.Errorf("task %q: %w", taskName, err)
	}

	runID, err := st.StartManualRun(ctx, task.ID)
	if err != nil {
		return err
	}
	fmt.Printf("run #%d · %s\n", runID, task.Name)

	exec := core.NewExecutor(st, box, time.Now)
	out, err := exec.Run(ctx, task, runID)
	if err != nil {
		return fmt.Errorf("run #%d failed: %w", runID, err)
	}
	fmt.Printf("ok · %s · %d bytes · sha256:%s\n", out.Key, out.Size, out.Checksum)
	fmt.Printf("restore: %s\n", out.RestoreHint)
	return nil
}
