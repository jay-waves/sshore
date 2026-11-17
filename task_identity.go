package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

type taskReference struct {
	order uint64
	code  string
}

func taskUID(key string) string {
	kind, uid, ok := strings.Cut(key, ":")
	if ok && (kind == "task" || kind == "peer-task") && validTaskUID(uid) {
		return uid
	}
	// Legacy observations retain a deterministic reference without allocating state.
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func canonicalTaskCode(key string) string {
	kind, uid, ok := strings.Cut(key, ":")
	if ok && (kind == "task" || kind == "peer-task") && validTaskUID(uid) {
		return uid[:6]
	}
	return ""
}

func validTaskUID(uid string) bool {
	raw, err := hex.DecodeString(uid)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == uid
}

func newTaskUID() (string, error) {
	var random [16]byte
	rand.Read(random[:])
	return hex.EncodeToString(random[:]), nil
}

func taskShortCode(key string) string { return taskUID(key)[:6] }

// References are derived from identities; observing tasks never reserves IDs.
func assignTaskIDs(keys []string) (map[string]taskReference, error) {
	result := make(map[string]taskReference, len(keys))
	for _, key := range keys {
		if key == "" {
			return nil, fmt.Errorf("missing task identity")
		}
		uid := taskUID(key)
		raw, _ := hex.DecodeString(uid)
		result[key] = taskReference{order: binary.BigEndian.Uint64(raw[:8]), code: uid[:6]}
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
		var random [3]byte
		rand.Read(random[:])
		names[i] = hex.EncodeToString(random[:])
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
	uid, err := newTaskUID()
	if err != nil {
		return nil, err
	}
	name := requested
	if name == "" {
		for length := 6; length <= len(uid); length++ {
			name = uid[:length]
			if !occupied[name] {
				return &taskIdentitySelection{name: name, uid: uid}, nil
			}
		}
		return nil, fmt.Errorf("task UID already exists")
	}
	if occupied[name] {
		return nil, fmt.Errorf("%s/%s: %w", kind, name, errObjectNameInUse)
	}
	return &taskIdentitySelection{name: name, uid: uid}, nil
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

// Displayed IDs stay six characters; selectors may use shorter unique prefixes.
func isShortTaskID(name string) bool { return len(name) >= 6 && isTaskIDPrefix(name) }

func isTaskIDPrefix(name string) bool {
	if len(name) < 2 || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func validAlias(name string) error {
	if err := validSession(name); err != nil {
		return err
	}
	if isShortTaskID(name) {
		return usage("6–32 hexadecimal characters are reserved for task IDs; choose another alias")
	}
	return nil
}
