package plugin

import (
	"context"
	"errors"
	"testing"
)

func TestRegistryCreateAndNames(t *testing.T) {
	r := NewRegistry[Notifier]("notifier")
	r.Register("noop", func([]byte) (Notifier, error) { return noopNotifier{}, nil })

	if !r.Has("noop") {
		t.Fatal("Has(noop) = false, want true")
	}
	n, err := r.Create("noop", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n.Name() != "noop" {
		t.Fatalf("Name = %q, want noop", n.Name())
	}

	if _, err := r.Create("missing", nil); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Create(missing) err = %v, want ErrNotRegistered", err)
	}

	if got := r.Names(); len(got) != 1 || got[0] != "noop" {
		t.Fatalf("Names = %v, want [noop]", got)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	r := NewRegistry[Notifier]("notifier")
	f := func([]byte) (Notifier, error) { return noopNotifier{}, nil }
	r.Register("dup", f)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	r.Register("dup", f)
}

type noopNotifier struct{}

func (noopNotifier) Name() string                        { return "noop" }
func (noopNotifier) Notify(context.Context, Event) error { return nil }
