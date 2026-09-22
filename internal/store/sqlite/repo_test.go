package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/duskrun/duskrun/internal/core"
)

func coreConn(name, engine, connType string) core.Connection {
	return core.Connection{Name: name, Engine: engine, ConnectorType: connType}
}

func coreStorage(name, typ string) core.Storage {
	return core.Storage{Name: name, Type: typ}
}

// seedTaskNamed inserts a connection+storage+task with the given name and enabled
// flag, reusing a single shared connection/storage row. Returns the task id.
func seedTaskNamed(t *testing.T, st *Store, name string, enabled bool) int64 {
	t.Helper()
	ctx := context.Background()
	// One shared connection+storage (id 1) created on first call.
	var n int
	if err := st.write.QueryRowContext(ctx, `SELECT COUNT(*) FROM connection`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		if _, err := st.write.ExecContext(ctx,
			`INSERT INTO connection (name, engine, connector_type, created_at) VALUES ('c','postgres','direct', unixepoch())`,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := st.write.ExecContext(ctx,
			`INSERT INTO storage (name, type, created_at) VALUES ('s','localfs', unixepoch())`,
		); err != nil {
			t.Fatal(err)
		}
	}
	en := 0
	if enabled {
		en = 1
	}
	res, err := st.write.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, cron, enabled, created_at)
		 VALUES (?, 1, 1, '0 2 * * *', ?, unixepoch())`, name, en,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TestListConnections seeds two connections and expects both back, ordered.
func TestListConnections(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.CreateConnection(ctx, coreConn("pg", "postgres", "direct")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConnection(ctx, coreConn("my", "mysql", "ssh-tunnel")); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListConnections(ctx)
	if err != nil {
		t.Fatalf("ListConnections: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(got), got)
	}
	if got[0].Name != "pg" || got[1].Name != "my" {
		t.Fatalf("order/names = %q,%q, want pg,my", got[0].Name, got[1].Name)
	}
	if got[1].Engine != "mysql" || got[1].ConnectorType != "ssh-tunnel" {
		t.Fatalf("conn[1] = %+v, want engine=mysql connector_type=ssh-tunnel", got[1])
	}
}

// TestListStorages seeds two storages and expects both back, ordered.
func TestListStorages(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.CreateStorage(ctx, coreStorage("local", "localfs")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateStorage(ctx, coreStorage("bucket", "s3")); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListStorages(ctx)
	if err != nil {
		t.Fatalf("ListStorages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(got), got)
	}
	if got[0].Name != "local" || got[0].Type != "localfs" {
		t.Fatalf("storage[0] = %+v, want name=local type=localfs", got[0])
	}
	if got[1].Name != "bucket" || got[1].Type != "s3" {
		t.Fatalf("storage[1] = %+v, want name=bucket type=s3", got[1])
	}
}

// TestListEnabledTasks seeds two enabled and one disabled task; only the enabled
// pair must come back.
func TestListEnabledTasks(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedTaskNamed(t, st, "a", true)
	seedTaskNamed(t, st, "b", true)
	seedTaskNamed(t, st, "c", false)

	got, err := st.ListEnabledTasks(ctx)
	if err != nil {
		t.Fatalf("ListEnabledTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(got), got)
	}
	for _, tk := range got {
		if !tk.Enabled {
			t.Errorf("task %q returned but not enabled", tk.Name)
		}
		if tk.Name == "c" {
			t.Errorf("disabled task %q was returned", tk.Name)
		}
	}
}

// TestGetTask round-trips a task by id.
func TestGetTask(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := seedTaskNamed(t, st, "solo", true)

	got, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != id || got.Name != "solo" {
		t.Fatalf("GetTask = %+v, want id=%d name=solo", got, id)
	}
	if _, err := st.GetTask(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask(missing) err = %v, want ErrNotFound", err)
	}
}
