package privsep

import (
	"errors"
	"os/user"
	"strings"
	"testing"
)

type fakeDirectory struct {
	account *user.User
	group   *user.Group
	err     error
}

func (directory fakeDirectory) LookupUser(string) (*user.User, error) {
	if directory.err != nil {
		return nil, directory.err
	}
	return directory.account, nil
}

func (directory fakeDirectory) LookupGroup(string) (*user.Group, error) {
	if directory.err != nil {
		return nil, directory.err
	}
	return directory.group, nil
}

func TestResolveProductionIdentity(t *testing.T) {
	identity, err := ResolveProductionIdentity(fakeDirectory{
		account: &user.User{Username: ProductionUser, Uid: "499", Gid: "499", HomeDir: ProductionHome},
		group:   &user.Group{Name: ProductionGroup, Gid: "499"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity.UID != 499 || identity.GID != 499 || identity.User != ProductionUser {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestResolveProductionIdentityRejectsUnsafeAccount(t *testing.T) {
	_, err := ResolveProductionIdentity(fakeDirectory{
		account: &user.User{Username: ProductionUser, Uid: "0", Gid: "20", HomeDir: "/Users/shared"},
		group:   &user.Group{Name: ProductionGroup, Gid: "499"},
	})
	if err == nil {
		t.Fatal("unsafe production identity was accepted")
	}
	for _, fragment := range []string{"non-root", "does not match", "home"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q missing %q", err, fragment)
		}
	}
}

func TestResolveProductionIdentityReportsLookupFailure(t *testing.T) {
	_, err := ResolveProductionIdentity(fakeDirectory{err: errors.New("missing")})
	if err == nil || !strings.Contains(err.Error(), "dedicated worker user") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeCredentialIDSupportsDarwinNobodyRepresentation(t *testing.T) {
	if got := NormalizeCredentialID(-2); got != 4294967294 {
		t.Fatalf("NormalizeCredentialID(-2) = %d", got)
	}
}
