//go:build !linux

package lifecycle

import "errors"

type Membership struct{ ID, Domain, ListenIP string }

func JoinOverlap(string, string) (*Membership, error) {
	return nil, errors.New("overlap requires Linux host-local locks")
}
func (*Membership) Alive(string) (bool, error) { return false, errors.New("overlap requires Linux") }
func (*Membership) ClaimOwner() (bool, error)  { return false, errors.New("overlap requires Linux") }
func (*Membership) Close() error               { return nil }
func DirectoryIdentity(string) (string, error) { return "", errors.New("overlap requires Linux") }
