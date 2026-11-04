package cluster

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

// Once another process with this identity is seen holding the member lease,
// the conflict lasts until this process holds the lease. An API error does
// not clear it, and losing the race to create the lease is a conflict too.
func TestIdentityConflictIsStickyUntilTheLeaseIsHeld(t *testing.T) {
	m := NewMembership(MembershipConfig{Identity: "atlantis-0", LeaseDuration: time.Hour, Logger: logging.NewNoopLogger(t)})

	m.trackConflict(errors.New("dial tcp: i/o timeout"))
	Assert(t, !m.IdentityConflict(), "an API error alone is not a conflict")

	m.trackConflict(fmt.Errorf("%w: lost acquire race", ErrHeld))
	Assert(t, m.IdentityConflict(), "losing the create race is a conflict")

	m.trackConflict(errors.New("dial tcp: i/o timeout"))
	Assert(t, m.IdentityConflict(), "an API error cleared the conflict")

	m.trackConflict(nil)
	Assert(t, !m.IdentityConflict(), "holding the lease did not clear the conflict")
}
