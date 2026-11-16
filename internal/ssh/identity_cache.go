package ssh

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const identityCacheLifetime = 15 * time.Minute

// IdentityCache retains unlocked file identities in memory for a fixed lifetime.
// It exposes no agent socket and retains neither passphrases nor file contents.
type IdentityCache struct {
	mu      sync.Mutex
	entries map[string]*cachedIdentity
	unlocks map[string]*identityUnlock
	done    chan struct{}
	closed  bool
}

type identityUnlock struct {
	gate  chan struct{}
	users int
}

type cachedIdentity struct {
	digest  [sha256.Size]byte
	ring    agent.Agent
	expires time.Time
	timer   *time.Timer
}

func NewIdentityCache() *IdentityCache {
	return &IdentityCache{entries: make(map[string]*cachedIdentity)}
}

// Acquire serializes unlock prompts for one path. The caller must check Get
// again after acquiring it and release it after unlocking. Other paths and
// cache expiration continue independently while the user answers a prompt.
func (c *IdentityCache) Acquire(ctx context.Context, path string) (func(), error) {
	if c == nil {
		return nil, fmt.Errorf("identity cache is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("identity cache is closed")
	}
	if c.unlocks == nil {
		c.unlocks = make(map[string]*identityUnlock)
	}
	if c.done == nil {
		c.done = make(chan struct{})
	}
	entry := c.unlocks[path]
	if entry == nil {
		entry = &identityUnlock{gate: make(chan struct{}, 1)}
		c.unlocks[path] = entry
	}
	entry.users++
	done := c.done
	c.mu.Unlock()
	acquired := false
	var once sync.Once
	release := func() {
		once.Do(func() {
			if acquired {
				<-entry.gate
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			entry.users--
			if entry.users == 0 {
				delete(c.unlocks, path)
			}
		})
	}
	select {
	case entry.gate <- struct{}{}:
		acquired = true
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-done:
		release()
		return nil, fmt.Errorf("identity cache is closed")
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		release()
		return nil, fmt.Errorf("identity cache is closed")
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Get does not renew an identity's lifetime. A changed file invalidates the
// previous entry even when the replacement has not yet been unlocked.
func (c *IdentityCache) Get(path string, data []byte) (cryptossh.Signer, bool) {
	if c == nil {
		return nil, false
	}
	digest := sha256.Sum256(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false
	}
	return c.getLocked(filepath.Clean(path), digest)
}

func (c *IdentityCache) getLocked(path string, digest [sha256.Size]byte) (cryptossh.Signer, bool) {
	entry := c.entries[path]
	if entry == nil {
		return nil, false
	}
	if entry.digest != digest || !time.Now().Before(entry.expires) {
		c.removeLocked(path, entry)
		return nil, false
	}
	signers, err := entry.ring.Signers()
	if err != nil || len(signers) != 1 {
		c.removeLocked(path, entry)
		return nil, false
	}
	// The local keyring returns a signer snapshot, not a protocol proxy. An
	// authentication already holding it may finish after eviction from the cache.
	return signers[0], true
}

// Add caches a parsed private key and returns its signer. Repeated insertion of
// the same file does not extend its deadline. Certificates can wrap this signer
// with ssh.NewCertSigner without retaining certificate-specific cache entries.
func (c *IdentityCache) Add(path string, data []byte, privateKey any) (cryptossh.Signer, error) {
	if c == nil {
		return nil, fmt.Errorf("identity cache is unavailable")
	}
	path = filepath.Clean(path)
	digest := sha256.Sum256(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("identity cache is closed")
	}
	if signer, ok := c.getLocked(path, digest); ok {
		return signer, nil
	}
	// Each file has its own keyring: duplicate public keys in different files
	// must not replace each other or reset each other's expiration.
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: privateKey}); err != nil {
		return nil, err
	}
	signers, err := ring.Signers()
	if err != nil || len(signers) != 1 {
		_ = ring.RemoveAll()
		return nil, fmt.Errorf("cannot obtain cached identity signer: %v", err)
	}
	entry := &cachedIdentity{digest: digest, ring: ring, expires: time.Now().Add(identityCacheLifetime)}
	if c.entries == nil {
		c.entries = make(map[string]*cachedIdentity)
	}
	c.entries[path] = entry
	entry.timer = time.AfterFunc(identityCacheLifetime, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries[path] == entry {
			c.removeLocked(path, entry)
		}
	})
	return signers[0], nil
}

func (c *IdentityCache) removeLocked(path string, entry *cachedIdentity) {
	if entry.timer != nil {
		entry.timer.Stop()
	}
	_ = entry.ring.RemoveAll()
	delete(c.entries, path)
}

// Close evicts every cached identity and stops expiration timers. Signers already
// acquired by an authentication remain usable until their caller releases them.
func (c *IdentityCache) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.done != nil {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}
	for path, entry := range c.entries {
		c.removeLocked(path, entry)
	}
	return nil
}
