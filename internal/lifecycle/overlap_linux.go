//go:build linux

package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
)

// Membership is restricted to a single Linux host and shared local filesystem.
// Locks remain held until every foreground and background dependency has closed.
// Dead request leases may only be reclaimed after Alive proves the owner exited.
type Membership struct {
	ID, Domain, ListenIP string
	dir                  string
	mu                   sync.Mutex
	files                []*os.File
	owner                *os.File
}

var memberID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type overlapDomain struct {
	Version                     int
	Domain, Directory, LockPath string
}

func writeDomain(path string, marker overlapDomain) error {
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".domain-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// DirectoryIdentity detects different mounts even when two containers use the
// same path text. Both releases must observe the same device and inode.
func DirectoryIdentity(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("shared state path is not a directory")
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("cannot identify shared directory")
	}
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino), nil
}

func randomMemberID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func lockFile(path string, mode int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// JoinOverlap retains a shared legacy writer lock, excluding unmodified binaries.
// A domain marker bound to that lock prevents accidental independent coordinators.
func JoinOverlap(lockPath, dir string) (_ *Membership, err error) {
	if lockPath == "" || dir == "" {
		return nil, errors.New("overlap requires state-lock and coordination-dir")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	lockPath, err = filepath.Abs(lockPath)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != dir {
		return nil, errors.New("coordination-dir must be a real local directory, not a symlink")
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("coordination-dir permissions must be 0700")
	}
	m := &Membership{dir: dir}
	defer func() {
		if err != nil {
			m.Close()
		}
	}()
	f, err := lockFile(lockPath, syscall.LOCK_SH|syscall.LOCK_NB)
	if err != nil {
		return nil, fmt.Errorf("legacy or incompatible writer owns state: %w", err)
	}
	m.files = append(m.files, f)
	init, err := lockFile(lockPath+".overlap-init", syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	defer init.Close()
	domainInit, err := lockFile(filepath.Join(dir, "domain-init.lock"), syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	defer domainInit.Close()
	markerPath := lockPath + ".overlap-domain"
	// The primary domain lives in the persistent coordination directory, never
	// only beside a writer lock under /run (which is cleared after reboot).
	domainPath := filepath.Join(dir, "domain.json")
	var marker overlapDomain
	b, e := os.ReadFile(domainPath)
	if os.IsNotExist(e) {
		marker = overlapDomain{Version: 1, Directory: dir, LockPath: lockPath}
		marker.Domain, err = randomMemberID()
		if err != nil {
			return nil, err
		}
		if err = writeDomain(domainPath, marker); err != nil {
			return nil, err
		}
	} else if e != nil {
		return nil, e
	} else if json.Unmarshal(b, &marker) != nil || marker.Version != 1 || marker.Directory != dir || marker.LockPath != lockPath || !memberID.MatchString(marker.Domain) {
		return nil, errors.New("persistent overlap domain mismatch")
	}
	var reference overlapDomain
	b, e = os.ReadFile(markerPath)
	if os.IsNotExist(e) {
		if err = writeDomain(markerPath, marker); err != nil {
			return nil, err
		}
	} else if e != nil {
		return nil, e
	} else if json.Unmarshal(b, &reference) != nil || reference != marker {
		return nil, errors.New("overlap domain mismatch; use the same coordination-dir and writer lock")
	}
	m.Domain = marker.Domain
	m.ID, err = randomMemberID()
	if err != nil {
		return nil, err
	}
	f, err = lockFile(filepath.Join(dir, "member-"+m.ID+".lock"), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return nil, err
	}
	m.files = append(m.files, f)
	// Each in-process Xray listener uses its own loopback address. Reserve the
	// address with a file lock rather than relying on a probabilistic hash.
	for i := 1; i < 65535; i++ {
		ip := net.IPv4(127, 77, byte(i>>8), byte(i)).String()
		f, e := lockFile(filepath.Join(dir, "proxy-"+ip+".lock"), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(e, syscall.EWOULDBLOCK) || errors.Is(e, syscall.EAGAIN) {
			continue
		}
		if e != nil {
			return nil, e
		}
		m.files = append(m.files, f)
		m.ListenIP = ip
		return m, nil
	}
	return nil, errors.New("no overlap proxy loopback addresses available")
}

func (m *Membership) Alive(id string) (bool, error) {
	if !memberID.MatchString(id) {
		return false, errors.New("invalid overlap owner")
	}
	f, err := os.OpenFile(filepath.Join(m.dir, "member-"+id+".lock"), os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// ClaimOwner never steals ownership from a draining process.
func (m *Membership) ClaimOwner() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owner != nil {
		return true, nil
	}
	f, err := lockFile(filepath.Join(m.dir, "management-owner.lock"), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m.owner = f
	return true, nil
}

func (m *Membership) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.files) - 1; i >= 0; i-- {
		_ = m.files[i].Close()
	}
	m.files = nil
	if m.owner != nil {
		_ = m.owner.Close()
		m.owner = nil
	}
	return nil
}
