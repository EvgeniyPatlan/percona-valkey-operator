/*
Copyright Percona LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package perconavalkeycluster

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// TestReloadNodeAuthOrdersMasterauthBeforeACLLoad verifies the per-node live
// reload issues CONFIG SET masterauth BEFORE ACL LOAD: masterauth must be set
// before the (possibly password-changing) aclfile is reloaded so a replica never
// observes a window where the default user requires a password it has not been
// told to present. The gomock InOrder enforces the sequence.
func TestReloadNodeAuthOrdersMasterauthBeforeACLLoad(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	c := valkey.NewMockClusterClient(ctrl)

	gomock.InOrder(
		c.EXPECT().ConfigSet(gomock.Any(), "masterauth", "pw").Return(nil),
		c.EXPECT().ACLLoad(gomock.Any()).Return(nil),
	)

	if err := reloadNodeAuth(context.Background(), c, "pw"); err != nil {
		t.Fatalf("reloadNodeAuth: %v", err)
	}
}

// TestReloadNodeAuthEmptyPasswordClearsMasterauth verifies that an auth-disabled
// cluster (empty requirepass) clears masterauth to "" so the directive agrees
// with the nopass default user.
func TestReloadNodeAuthEmptyPasswordClearsMasterauth(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	c := valkey.NewMockClusterClient(ctrl)

	gomock.InOrder(
		c.EXPECT().ConfigSet(gomock.Any(), "masterauth", "").Return(nil),
		c.EXPECT().ACLLoad(gomock.Any()).Return(nil),
	)

	if err := reloadNodeAuth(context.Background(), c, ""); err != nil {
		t.Fatalf("reloadNodeAuth: %v", err)
	}
}

// TestReloadNodeAuthConfigSetFailureSkipsACLLoad verifies that a CONFIG SET
// failure short-circuits: ACL LOAD is NOT issued (the mock has no ACLLoad
// expectation, so a call would fail the test) and the error is wrapped.
func TestReloadNodeAuthConfigSetFailureSkipsACLLoad(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	c := valkey.NewMockClusterClient(ctrl)

	c.EXPECT().ConfigSet(gomock.Any(), "masterauth", "pw").Return(errors.New("boom"))
	// No ACLLoad expectation: it must never be reached.

	err := reloadNodeAuth(context.Background(), c, "pw")
	if err == nil {
		t.Fatal("expected error when CONFIG SET masterauth fails")
	}
}

// TestReloadNodeAuthACLLoadFailurePropagates verifies an ACL LOAD failure is
// surfaced (after masterauth succeeded).
func TestReloadNodeAuthACLLoadFailurePropagates(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	c := valkey.NewMockClusterClient(ctrl)

	gomock.InOrder(
		c.EXPECT().ConfigSet(gomock.Any(), "masterauth", "pw").Return(nil),
		c.EXPECT().ACLLoad(gomock.Any()).Return(errors.New("nope")),
	)

	if err := reloadNodeAuth(context.Background(), c, "pw"); err == nil {
		t.Fatal("expected error when ACL LOAD fails")
	}
}

// TestAuthSignatureChangesWithContentAndPassword verifies the signature is a pure
// function of (aclContent, requirepass) and that either field changing flips it,
// while identical inputs yield an identical signature (the no-op trigger).
func TestAuthSignatureChangesWithContentAndPassword(t *testing.T) {
	t.Parallel()
	base := authSignature("user default on nopass ~* &* +@all\n", "")
	if base != authSignature("user default on nopass ~* &* +@all\n", "") {
		t.Fatal("signature is not stable for identical inputs")
	}
	if base == authSignature("user default on #abc ~* &* +@all\n", "") {
		t.Fatal("signature did not change when the aclfile content changed")
	}
	if base == authSignature("user default on nopass ~* &* +@all\n", "pw") {
		t.Fatal("signature did not change when the requirepass changed")
	}
	// Guard against a NUL-collision: content+pw must not alias across the boundary.
	if authSignature("ab", "c") == authSignature("a", "bc") {
		t.Fatal("signature aliases across the content/password boundary")
	}
}
