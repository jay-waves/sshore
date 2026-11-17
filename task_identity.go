package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Keep sequence order private and reserve display codes for immutable instances.
type taskIDIndex struct {
	Next  uint64
	IDs   map[string]uint64
	Codes map[string]string
}

type taskReference struct {
	order uint64
	code  string
}

func canonicalTaskCode(key string) string {
	kind, uid, ok := strings.Cut(key, ":")
	if !ok || (kind != "task" && kind != "peer-task") || !validTaskUID(uid) {
		return ""
	}
	return uid[:6]
}

func validTaskUID(uid string) bool {
	raw, err := hex.DecodeString(uid)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == uid
}

var errTaskPrefixInUse = errors.New("task ID prefix is reserved")

func newTaskUID() (string, error) {
	for {
		sum := sha256.Sum256([]byte(rand.Text()))
		uid := hex.EncodeToString(sum[:16])
		if _, err := assignTaskIDsMode([]string{"task:" + uid}, true); err != nil {
			if errors.Is(err, errTaskPrefixInUse) {
				continue
			}
			return "", err
		}
		return uid, nil
	}
}

func taskShortCode(key string, salt uint64) string {
	if salt == 0 {
		if code := canonicalTaskCode(key); code != "" {
			return code
		}
	}

	if salt != 0 {
		key = fmt.Sprintf("%s\x00%d", key, salt)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:3])
}

func readTaskIDIndex(path string) (taskIDIndex, error) {
	index := taskIDIndex{Next: 1, IDs: map[string]uint64{}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &index); err != nil {
			return taskIDIndex{}, err
		}
	} else if !os.IsNotExist(err) {
		return taskIDIndex{}, err
	}
	if index.Next == 0 || index.IDs == nil {
		return taskIDIndex{}, fmt.Errorf("invalid task ID index")
	}
	seen := map[uint64]bool{}
	for _, id := range index.IDs {
		if id == 0 || id >= index.Next || seen[id] {
			return taskIDIndex{}, fmt.Errorf("invalid task ID index")
		}
		seen[id] = true
	}
	if index.Codes == nil {
		index.Codes = map[string]string{}
	}
	canonicalCodes := map[string]bool{}
	for key, code := range index.Codes {
		if canonicalTaskCode(key) == code {
			canonicalCodes[code] = true
		}
	}
	seenCodes := map[string]string{}
	for key, code := range index.Codes {
		raw, err := hex.DecodeString(code)
		if index.IDs[key] == 0 || err != nil || len(raw) != 3 ||
			hex.EncodeToString(raw) != code {
			return taskIDIndex{}, fmt.Errorf("invalid task short ID index")
		}
		if old := seenCodes[code]; old != "" && !canonicalCodes[code] {
			return taskIDIndex{}, fmt.Errorf("invalid task short ID index")
		}
		seenCodes[code] = key
	}
	return index, nil
}

func assignTaskIDs(keys []string) (map[string]taskReference, error) {
	return assignTaskIDsMode(keys, false)
}

func assignTaskIDsMode(keys []string, creating bool) (map[string]taskReference, error) {
	dir, err := stateDir("tasks")
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "ids.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = privateStatePath(lockPath, false); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = lockResourceState(f)
		if err == nil {
			break
		}
		if !resourceLockBusy(err) || time.Now().After(deadline) {
			return nil, fmt.Errorf("task IDs unavailable: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	path := filepath.Join(dir, "ids.json")
	index, err := readTaskIDIndex(path)
	if err != nil {
		return nil, err
	}
	reserved := make(map[string]bool, len(index.Codes))
	for _, code := range index.Codes {
		reserved[code] = true
	}
	result := make(map[string]taskReference, len(keys))
	changed := false
	for _, key := range keys {
		if key == "" {
			return nil, fmt.Errorf("missing task identity")
		}
		id := index.IDs[key]
		if id == 0 {
			if index.Next == ^uint64(0) {
				return nil, fmt.Errorf("task IDs exhausted")
			}
			id = index.Next
			index.Next++
			index.IDs[key] = id
			changed = true
		}
		code := index.Codes[key]
		if code == "" {
			if len(reserved) >= 1<<24 {
				return nil, fmt.Errorf("task short IDs exhausted")
			}
			code = canonicalTaskCode(key)
			if code != "" {
				if creating && reserved[code] {
					return nil, errTaskPrefixInUse
				}
			} else {
				for salt := uint64(0); ; salt++ {
					code = taskShortCode(key, salt)
					if !reserved[code] {
						break
					}
				}
			}
			index.Codes[key], reserved[code], changed = code, true, true
		}
		result[key] = taskReference{id, code}
	}
	if changed {
		if err = writeStateJSON(path, index); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func resourceIdentity(r resourceRecord) string {
	if validTaskUID(r.UID) {
		return "task:" + r.UID
	}
	if r.Token == "" {
		return ""
	}
	return fmt.Sprintf("%s:%x", r.Kind, sha256.Sum256([]byte(r.Token)))
}

func numberInventoryResources(inv *inventory) error {
	keys := []string{}
	for _, r := range inv.Resources {
		if r.Identity != "" {
			keys = append(keys, r.Identity)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return err
	}
	for i := range inv.Resources {
		id := ids[inv.Resources[i].Identity]
		inv.Resources[i].order, inv.Resources[i].ShortID = id.order, id.code
	}
	sort.SliceStable(inv.Resources, func(i, j int) bool { return inv.Resources[i].order < inv.Resources[j].order })
	return nil
}

func remoteShellIdentity(host, instance string) string {
	if validTaskUID(instance) {
		return "task:" + instance
	}
	if instance == "" {
		return ""
	}
	return "remote:" + host + ":" + instance
}

func numberInventoryShells(inv *inventory) error {
	keys := []string{}
	for _, s := range inv.Shells {
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		if key := remoteShellIdentity(host, s.Instance); key != "" {
			keys = append(keys, key)
		}
	}
	ids, err := assignTaskIDs(keys)
	if err != nil {
		return err
	}
	for i := range inv.Shells {
		s := &inv.Shells[i]
		host := s.Host
		if host == "" {
			host = inv.Host
		}
		id := ids[remoteShellIdentity(host, s.Instance)]
		s.order, s.ShortID = id.order, id.code
	}
	return nil
}

// Automatic labels are short references; user labels are optional aliases.
func automaticSessionNames() []string {
	names := make([]string, 32)
	for i := range names {
		sum := sha256.Sum256([]byte(rand.Text()))
		names[i] = hex.EncodeToString(sum[:3])
	}
	return names
}

func validSession(name string) error {
	if len(name) < 1 || len(name) > 48 {
		return usage("name must contain 1–48 ASCII characters")
	}
	for i, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return usage("invalid name: use letters, digits, _ and -")
		}
	}
	return nil
}

var errObjectNameInUse = errors.New("name is already in use")

type taskIdentitySelection struct{ name, uid string }

// Selection is advisory. Only insertion into the daemon task table claims a
// name; there is no reservation to recover or transfer between processes.
func chooseTaskIdentity(kind, requested string) (*taskIdentitySelection, error) {
	return chooseTaskIdentityExcept(kind, requested, "")
}
func chooseTaskIdentityExcept(kind, requested, exceptID string) (*taskIdentitySelection, error) {
	if requested != "" {
		if err := validSession(requested); err != nil {
			return nil, err
		}
	}
	occupied, err := occupiedObjectNames(exceptID)
	if err != nil {
		return nil, err
	}
	for {
		uid, err := newTaskUID()
		if err != nil {
			return nil, err
		}
		name := requested
		if name == "" {
			name = uid[:6]
		}
		if !occupied[name] {
			return &taskIdentitySelection{name: name, uid: uid}, nil
		}
		if requested != "" {
			return nil, fmt.Errorf("%s/%s: %w", kind, name, errObjectNameInUse)
		}
	}
}

func occupiedObjectNames(exceptID string) (map[string]bool, error) {
	r, err := existingDaemonRecord()
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := dialLocalSession(r)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := writeMessage(c, message{Type: "task_names", Instance: exceptID}); err != nil {
		return nil, err
	}
	var names map[string]bool
	err = json.NewDecoder(c).Decode(&names)
	return names, err
}

func isShortTaskID(name string) bool {
	raw, err := hex.DecodeString(name)
	return err == nil && len(raw) == 3
}

func validAlias(name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	if isShortTaskID(name) {
		return usage("six hexadecimal characters are reserved for task IDs; choose another alias")
	}
	return nil
}
