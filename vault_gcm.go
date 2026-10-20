package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Where Windows has DPAPI, the other systems get a random 256-bit key and AES-GCM. The key is
// kept where only this user can read it: the login Keychain on a Mac, the Secret Service on Linux
// (through libsecret's secret-tool, when it is installed and a desktop session is running), and
// otherwise a private file in the data folder. That file means anyone who can read your files as
// you can read the key; it only keeps other users out.

// systemKeyStore says whether the Keychain and the Secret Service may be asked. The tests turn it
// off, so they never touch the real ones.
var systemKeyStore = true

const (
	tagGCM   = 1 // the first byte of what this sealer makes
	keyBytes = 32
)

var sealAAD = []byte("Zipline saved secret v1")

// A vaultKey is one key and where it came from.
type vaultKey struct {
	key  []byte
	from string
}

// A keySource finds the keys. The first one returned seals new secrets; all of them open. Without
// create, no key at all is (nil, nil); with it, one is made when none exists. An error means the
// store couldn't be asked, or answered something odd: nothing is made then, so a key that is
// merely out of reach is never replaced.
type keySource interface {
	load(create bool) ([]vaultKey, error)
	kind() string // where a key is kept, when there isn't one yet
}

// gcmSealer seals with AES-GCM under the first of its keys. It asks its source once, and again
// after a failure no sooner than retry.
type gcmSealer struct {
	src   keySource
	retry time.Duration

	mu     sync.Mutex
	keys   []vaultKey
	err    error
	tried  time.Time
	loaded atomic.Value // the first key's origin, once there is one
}

func newGCMSealer(src keySource) *gcmSealer { return &gcmSealer{src: src, retry: time.Minute} }

func (g *gcmSealer) how() string {
	if s, _ := g.loaded.Load().(string); s != "" {
		return s
	}
	return g.src.kind()
}

func (g *gcmSealer) keysFor(create bool) ([]vaultKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.keys) > 0 {
		return g.keys, nil
	}
	if g.err != nil && time.Since(g.tried) < g.retry {
		return nil, g.err
	}
	keys, err := g.src.load(create)
	g.tried, g.err = time.Now(), err
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errNoKey
	}
	g.keys = keys
	g.loaded.Store(keys[0].from)
	return keys, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func (g *gcmSealer) seal(plain []byte) ([]byte, error) {
	keys, err := g.keysFor(true)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(keys[0].key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1, 1+aead.NonceSize()+len(plain)+aead.Overhead())
	out[0] = tagGCM
	out = append(out, make([]byte, aead.NonceSize())...)
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[1:], plain, sealAAD), nil
}

func (g *gcmSealer) open(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 || sealed[0] != tagGCM {
		return nil, errForeign
	}
	keys, err := g.keysFor(false)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		aead, err := newAEAD(k.key)
		if err != nil {
			continue
		}
		n := aead.NonceSize()
		if len(sealed) < 1+n+aead.Overhead() {
			return nil, errDamaged
		}
		if plain, err := aead.Open(nil, sealed[1:1+n], sealed[1+n:], sealAAD); err == nil {
			return plain, nil
		}
	}
	return nil, errWrongKey
}

func newKey() []byte {
	k := make([]byte, keyBytes)
	rand.Read(k)
	return k
}

func parseKey(s string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(k) != keyBytes {
		return nil, errors.New("the stored key is damaged")
	}
	return k, nil
}

// ---------------------------------------------------------------- a key file

// fileKeys keeps the key as hex in a file only its owner can read.
type fileKeys struct{ path string }

func (f fileKeys) kind() string { return "file" }

func (f fileKeys) read() ([]byte, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	k, err := parseKey(string(b))
	if err != nil {
		return nil, fmt.Errorf("the key file %s is damaged", filepath.Base(f.path))
	}
	return k, nil
}

func (f fileKeys) load(create bool) ([]vaultKey, error) {
	k, err := f.read()
	switch {
	case err == nil:
		return []vaultKey{{k, "file"}}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	case !create:
		return nil, nil
	}
	if err = f.create(newKey()); errors.Is(err, fs.ErrExist) { // another Zipline made it first
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if k, err = f.read(); err != nil {
		return nil, err
	}
	return []vaultKey{{k, "file"}}, nil
}

// create writes the key complete or not at all: through a temporary file, linked into place so a
// reader never sees half a key and a key that exists is never replaced (fs.ErrExist).
func (f fileKeys) create(key []byte) error {
	tmp := fmt.Sprintf("%s.%x.tmp", f.path, newKey()[:4])
	w, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFile)
	if err != nil {
		return err
	}
	defer func() { // (Windows refuses while another name of the same file is open: try again a moment later)
		for i := 0; i < 20; i++ {
			if e := os.Remove(tmp); e == nil || errors.Is(e, fs.ErrNotExist) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_, err = w.WriteString(hex.EncodeToString(key) + "\n")
	if err == nil {
		err = w.Sync()
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Link(tmp, f.path); err == nil || errors.Is(err, fs.ErrExist) {
		return err
	}
	// no hard links here (some file systems): make it where it goes, but only if it isn't there
	x, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFile)
	if err != nil {
		return err
	}
	_, err = x.WriteString(hex.EncodeToString(key) + "\n")
	if cerr := x.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---------------------------------------------------------------- the programs that hold keys

type cmdResult struct {
	out, errOut string
	code        int
}

// A runner runs a program and says what it printed and how it ended. The error is for a program
// that couldn't start or ran out of time.
type runner func(stdin, name string, args ...string) (cmdResult, error)

// runCommand is the real runner. A Keychain or keyring asking for its password can take a while.
func runCommand(stdin, name string, args ...string) (cmdResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	res := cmdResult{out: out.String(), errOut: errOut.String()}
	if ctx.Err() != nil {
		return res, fmt.Errorf("%s didn't answer in time", name)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.code = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

const keychainService = "Zipline"

// keychainKeys keeps the key in the login Keychain, through the security command.
type keychainKeys struct {
	run  runner
	user string // the account name of the item
}

func (k keychainKeys) kind() string { return "keychain" }

func (k keychainKeys) get() (key []byte, found bool, err error) {
	r, err := k.run("", "security", "find-generic-password", "-s", keychainService, "-a", k.user, "-w")
	if err != nil {
		return nil, false, err
	}
	switch {
	case r.code == 0:
		key, err = parseKey(r.out)
		return key, err == nil, err
	case r.code == 44 || strings.Contains(r.errOut, "could not be found"): // errSecItemNotFound
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("the Keychain said: %s", firstLine(r.errOut))
}

// put adds the key. The tool's own prompt (security -i, commands on its standard input) goes
// first, so the key never shows in the process list; the plain command is the fallback.
func (k keychainKeys) put(key []byte) {
	h := hex.EncodeToString(key)
	k.run("add-generic-password -s "+keychainService+" -a "+quoteTool(k.user)+" -w "+h+"\n", "security", "-i")
	if got, found, _ := k.get(); found && bytes.Equal(got, key) {
		return
	}
	k.run("", "security", "add-generic-password", "-s", keychainService, "-a", k.user, "-w", h)
}

// quoteTool quotes a word for security -i.
func quoteTool(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (k keychainKeys) load(create bool) ([]vaultKey, error) {
	key, found, err := k.get()
	if err != nil {
		return nil, err
	}
	if !found && create {
		k.put(newKey())
		key, found, err = k.get() // whatever the Keychain holds now is the key (another Zipline may have won)
		if err == nil && !found {
			err = errors.New("the Keychain wouldn't keep the key")
		}
	}
	if err != nil || !found {
		return nil, err
	}
	return []vaultKey{{key, "keychain"}}, nil
}

// secretToolKeys keeps the key in the Secret Service (GNOME Keyring, KWallet) through libsecret's
// secret-tool, under the attribute app=zipline.
type secretToolKeys struct{ run runner }

func (s secretToolKeys) get() (key []byte, found bool, err error) {
	r, err := s.run("", "secret-tool", "lookup", "app", "zipline")
	if err != nil {
		return nil, false, err
	}
	switch {
	case r.code == 0 && strings.TrimSpace(r.out) != "":
		key, err = parseKey(r.out)
		return key, err == nil, err
	case r.code == 1 && strings.TrimSpace(r.errOut) == "": // nothing stored (a failure says why)
		return nil, false, nil
	}
	if msg := firstLine(r.errOut); msg != "" {
		return nil, false, fmt.Errorf("the Secret Service said: %s", msg)
	}
	return nil, false, errors.New("the Secret Service didn't answer")
}

func (s secretToolKeys) put(key []byte) error {
	r, err := s.run(hex.EncodeToString(key), "secret-tool", "store", "--label=Zipline key for saved passwords", "app", "zipline")
	if err == nil && r.code != 0 {
		err = fmt.Errorf("the Secret Service said: %s", firstLine(r.errOut))
	}
	return err
}

// linuxKeys prefers the Secret Service and falls back to a key file. A key that is already in
// one place is used from there; one is made in the Secret Service only if neither has it, and
// in the file only when the Secret Service isn't there to ask (no secret-tool, no desktop session).
// Older keys stay readable, so values sealed under either are opened.
type linuxKeys struct {
	ss   *secretToolKeys // nil: not installed, or no session to ask
	file fileKeys
}

func (l linuxKeys) kind() string {
	if l.ss != nil {
		return "secret-service"
	}
	return "file"
}

func (l linuxKeys) load(create bool) ([]vaultKey, error) {
	if l.ss == nil {
		return l.file.load(create)
	}
	key, found, err := l.ss.get()
	if err != nil {
		return nil, err
	}
	old, ferr := l.file.load(false) // (a key file from before the Secret Service was there)
	if ferr != nil {
		old = nil
	}
	switch {
	case found:
		return append([]vaultKey{{key, "secret-service"}}, old...), nil
	case len(old) > 0:
		return old, nil
	case !create:
		return nil, nil
	}
	if err := l.ss.put(newKey()); err != nil {
		return nil, err
	}
	key, found, err = l.ss.get()
	if err == nil && !found {
		err = errors.New("the Secret Service wouldn't keep the key")
	}
	if err != nil {
		return nil, err
	}
	return []vaultKey{{key, "secret-service"}}, nil
}
