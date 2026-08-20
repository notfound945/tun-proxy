package privsep

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
)

const (
	ProductionUser  = "_tun-proxy"
	ProductionGroup = "_tun-proxy"
	ProductionHome  = "/var/empty"
)

type Directory interface {
	LookupUser(string) (*user.User, error)
	LookupGroup(string) (*user.Group, error)
}

type SystemDirectory struct{}

func (SystemDirectory) LookupUser(name string) (*user.User, error)   { return user.Lookup(name) }
func (SystemDirectory) LookupGroup(name string) (*user.Group, error) { return user.LookupGroup(name) }

type Identity struct {
	User  string
	Group string
	UID   uint32
	GID   uint32
	Home  string
}

func ResolveProductionIdentity(directory Directory) (Identity, error) {
	if directory == nil {
		return Identity{}, errors.New("worker account directory is required")
	}
	account, err := directory.LookupUser(ProductionUser)
	if err != nil {
		return Identity{}, fmt.Errorf("lookup dedicated worker user %q: %w", ProductionUser, err)
	}
	group, err := directory.LookupGroup(ProductionGroup)
	if err != nil {
		return Identity{}, fmt.Errorf("lookup dedicated worker group %q: %w", ProductionGroup, err)
	}
	uid, err := parseID("UID", account.Uid)
	if err != nil {
		return Identity{}, err
	}
	primaryGID, err := parseID("primary GID", account.Gid)
	if err != nil {
		return Identity{}, err
	}
	groupGID, err := parseID("group GID", group.Gid)
	if err != nil {
		return Identity{}, err
	}
	identity := Identity{
		User: account.Username, Group: group.Name, UID: uid, GID: primaryGID, Home: account.HomeDir,
	}
	var failures []error
	if identity.User != ProductionUser {
		failures = append(failures, fmt.Errorf("worker user resolved as %q, want %q", identity.User, ProductionUser))
	}
	if identity.Group != ProductionGroup {
		failures = append(failures, fmt.Errorf("worker group resolved as %q, want %q", identity.Group, ProductionGroup))
	}
	if uid == 0 || primaryGID == 0 {
		failures = append(failures, fmt.Errorf("worker identity must be non-root, got uid=%d gid=%d", uid, primaryGID))
	}
	if primaryGID != groupGID {
		failures = append(failures, fmt.Errorf("worker primary GID=%d does not match dedicated group GID=%d", primaryGID, groupGID))
	}
	if identity.Home != ProductionHome {
		failures = append(failures, fmt.Errorf("worker home=%q, want %q", identity.Home, ProductionHome))
	}
	if err := errors.Join(failures...); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func ValidateCurrentIdentity(identity Identity) error {
	effectiveUID := NormalizeCredentialID(os.Geteuid())
	effectiveGID := NormalizeCredentialID(os.Getegid())
	groups, err := os.Getgroups()
	if err != nil {
		return fmt.Errorf("read worker supplementary groups: %w", err)
	}
	if effectiveUID != identity.UID || effectiveGID != identity.GID {
		return fmt.Errorf("worker credential mismatch uid=%d gid=%d, want uid=%d gid=%d", effectiveUID, effectiveGID, identity.UID, identity.GID)
	}
	if len(groups) != 1 || NormalizeCredentialID(groups[0]) != identity.GID {
		return fmt.Errorf("worker supplementary groups=%v, want only gid=%d", groups, identity.GID)
	}
	return nil
}

func NormalizeCredentialID(value int) uint32 { return uint32(value) }

func parseID(name, raw string) (uint32, error) {
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid worker %s %q: %w", name, raw, err)
	}
	return uint32(value), nil
}
