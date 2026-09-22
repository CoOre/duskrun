package core

import (
	"context"

	"github.com/duskrun/duskrun/internal/plugin"
)

// testNotifications wires a Notifications that delivers to spy (or nowhere when
// spy is nil), bypassing channel storage and secret resolution.
func testNotifications(spy *spyNotifier) *Notifications {
	return NewNotifications(nil, nil, NotificationsConfig{
		Resolve: func(context.Context, *Task, plugin.EventKind) []plugin.Notifier {
			if spy == nil {
				return nil
			}
			return []plugin.Notifier{spy}
		},
	}, nil)
}
