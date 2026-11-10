package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Multiple watcher/refresh objects may refer to the same file. Coordinate
// identity initialization with credential saves by canonical file path.
var deviceFileLocks sync.Map

func lockDeviceFile(path string) func() {
	key, err := filepath.Abs(path)
	if err != nil {
		key = filepath.Clean(path)
	}
	value, _ := deviceFileLocks.LoadOrStore(key, new(sync.Mutex))
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func validDeviceIdentity(id string) bool {
	if len(id) != 36 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func savedDeviceIdentity(path, uid string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	a, err := Parse(raw)
	if err != nil || a.UID != uid {
		return ""
	}
	return a.DeviceID
}

// DeviceIdentity initializes one opaque account identifier on demand. It
// preserves the original document, including unknown provider fields, and
// publishes the ID only after the atomic write succeeds. Read-only auths and
// in-memory accounts retain legacy behavior instead of blocking model calls.
func (a *Auth) DeviceIdentity() string {
	// A refresh may call this while holding its shared credential transaction.
	// In overlap mode, legacy files retain their deterministic legacy identity;
	// lazy initialization must never write outside that transaction or reenter it.
	if currentSharedAuthority() != nil {
		return a.Snapshot().DeviceID
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	c := a.Snapshot()
	if c.DeviceID != "" {
		return c.DeviceID
	}
	if a.deviceFallback || c.FilePath == "" {
		return ""
	}
	unlock := lockDeviceFile(c.FilePath)
	defer unlock()
	id, err := initializeDeviceIdentity(c.FilePath, c.UID)
	if err != nil {
		a.deviceFallback = true
		return ""
	}
	a.mu.Lock()
	a.DeviceID = id
	a.mu.Unlock()
	return id
}

func initializeDeviceIdentity(path, uid string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	current, err := Parse(raw)
	if err != nil || current.UID != uid {
		return "", errors.New("account changed before device initialization")
	}
	if current.DeviceID != "" {
		return current.DeviceID, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	var random [18]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	doc["_device_identity"], _ = json.Marshal(id)
	raw, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".device-identity-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return id, nil
}
