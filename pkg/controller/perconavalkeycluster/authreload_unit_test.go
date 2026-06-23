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

// TestParseACLUsersKeepsComparableTokens verifies parseACLUsers retains only the
// engine-preserved enabled/password/command tokens (dropping key/channel/flag
// tokens that Valkey's ACL LIST normalizes) and ignores blank/malformed lines.
func TestParseACLUsersKeepsComparableTokens(t *testing.T) {
	t.Parallel()
	body := "user default on #abc ~* &* +@all\n\nuser app on #def ~* +@read\nnot-a-user-line\n"
	got := parseACLUsers(body)
	if len(got) != 2 {
		t.Fatalf("want 2 users, got %d: %v", len(got), got)
	}
	app, ok := got["app"]
	if !ok {
		t.Fatal("app user missing")
	}
	if app.enabled != "on" {
		t.Fatalf("app enabled=%q, want on", app.enabled)
	}
	if _, ok := app.passwords["#def"]; !ok {
		t.Fatalf("app missing password #def: %v", app.passwords)
	}
	if _, ok := app.commands["+@read"]; !ok {
		t.Fatalf("app missing command +@read: %v", app.commands)
	}
	// The key pattern ~* is a normalized flag and must NOT be retained as a token.
	if _, ok := app.commands["~*"]; ok {
		t.Fatal("key pattern ~* leaked into command tokens")
	}
}

// TestNodeACLMatchesDetectsPropagationLag is the regression test for the
// Secret-mount propagation race: liveReloadAuth must NOT treat a node as reloaded
// until its loaded ACL (ACL LIST) actually reflects the freshly rendered content.
// A node still serving STALE mounted content (missing the new user or an
// out-of-date grant) must report a mismatch so the reconcile stays pending and
// retries, instead of stamping the signature and silently dropping the change.
func TestNodeACLMatchesDetectsPropagationLag(t *testing.T) {
	t.Parallel()

	// rendered is the operator's aclfile syntax (omits the implicit -@all baseline
	// and the alldbs/sanitize-payload defaults the engine adds on load).
	rendered := "user default on #abc ~* &* +@all\n" +
		"user app on #def ~* +@read +@write\n"
	want := parseACLUsers(rendered)

	cases := []struct {
		name   string
		loaded []string // engine-normalized ACL LIST output (the real shape)
		match  bool
	}{
		{
			name: "fully propagated (engine-normalized) -> match",
			loaded: []string{
				"user default on sanitize-payload #abc ~* &* alldbs +@all",
				"user app on sanitize-payload #def ~* resetchannels alldbs -@all +@read +@write",
			},
			match: true,
		},
		{
			name: "stale grant (missing +@write) -> mismatch (the live bug)",
			loaded: []string{
				"user default on sanitize-payload #abc ~* &* alldbs +@all",
				"user app on sanitize-payload #def ~* resetchannels alldbs -@all +@read",
			},
			match: false,
		},
		{
			name: "stale password (old hash) -> mismatch",
			loaded: []string{
				"user default on sanitize-payload #abc ~* &* alldbs +@all",
				"user app on sanitize-payload #STALE ~* resetchannels alldbs -@all +@read +@write",
			},
			match: false,
		},
		{
			name: "new user not yet loaded -> mismatch",
			loaded: []string{
				"user default on sanitize-payload #abc ~* &* alldbs +@all",
			},
			match: false,
		},
		{
			name: "token order differs but rules equal -> match",
			loaded: []string{
				"user default on ~* &* alldbs +@all sanitize-payload #abc",
				"user app on +@write ~* resetchannels -@all +@read alldbs #def sanitize-payload",
			},
			match: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			c := valkey.NewMockClusterClient(ctrl)
			c.EXPECT().ACLList(gomock.Any()).Return(tc.loaded, nil)

			ok, err := nodeACLMatches(context.Background(), c, want)
			if err != nil {
				t.Fatalf("nodeACLMatches: %v", err)
			}
			if ok != tc.match {
				t.Fatalf("match=%v, want %v (loaded=%v)", ok, tc.match, tc.loaded)
			}
		})
	}
}

// TestNodeACLMatchesReadErrorPropagates verifies an ACL LIST read error is
// surfaced so the caller treats the node as pending (and retries) rather than
// assuming a match.
func TestNodeACLMatchesReadErrorPropagates(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	c := valkey.NewMockClusterClient(ctrl)
	c.EXPECT().ACLList(gomock.Any()).Return(nil, errors.New("NOPERM"))

	if _, err := nodeACLMatches(context.Background(), c, parseACLUsers("user x on nopass")); err == nil {
		t.Fatal("expected error to propagate from ACL LIST failure")
	}
}
