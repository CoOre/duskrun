package webpush

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
)

// Type is the notifier key of a push channel.
const Type = "push"

// ChannelConfig is a push channel's config. Subscriptions say where to
// deliver; the channel says which events go and to whom.
type ChannelConfig struct {
	// MinRole is the lowest role whose browsers receive the channel's events.
	// Empty means viewer: everyone who can see the task.
	MinRole core.Role `json:"min_role,omitempty"`
}

// Register binds the push notifier to reg. svc may report itself unavailable
// (or be nil, for listings that never deliver): the type is still registered,
// so the API recognises it, and building a channel fails with the reason —
// which is how a channel that could never deliver is refused when saved.
func Register(reg *plugin.Registry[plugin.Notifier], svc *Service) {
	reg.Register(Type, func(raw []byte) (plugin.Notifier, error) {
		var c ChannelConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, fmt.Errorf("push: bad config: %w", err)
			}
		}
		if c.MinRole == "" {
			c.MinRole = core.RoleViewer
		}
		if !c.MinRole.Valid() {
			return nil, fmt.Errorf("push: unknown min_role %q", c.MinRole)
		}
		if svc == nil {
			return nil, fmt.Errorf("push: not configured")
		}
		if err := svc.unavailableError(); err != nil {
			return nil, err
		}
		return &Notifier{svc: svc, minRole: c.MinRole}, nil
	})
}

// Notifier is one push channel.
type Notifier struct {
	svc     *Service
	minRole core.Role
}

func (n *Notifier) Name() string { return Type }

// Notify broadcasts the event to the browsers of users at or above minRole.
// Security events go to administrators only, whatever the channel's audience:
// who logged in from where is not for every viewer's phone.
func (n *Notifier) Notify(ctx context.Context, ev plugin.Event) error {
	role := n.minRole
	if ev.Kind == plugin.EventSecurity {
		role = core.RoleAdmin
	}
	return n.svc.Broadcast(ctx, role, ev)
}
