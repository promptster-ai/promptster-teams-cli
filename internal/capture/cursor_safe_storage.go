package capture

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1" // #nosec G505 -- Chromium's os_crypt key derivation is PBKDF2-HMAC-SHA1; we match it, we don't choose it.
	"encoding/base64"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Cursor's encrypted-at-rest sign-in token (PD-52, 2026-11-12).
//
// Cursor ships a codec (`cursorAuthTokenStorageCodec`, in both 3.27 and 3.29)
// that, behind its server-side gate `cursor_auth_token_encryption_at_rest`,
// stores `cursorAuth/accessToken` and `cursorAuth/refreshToken` as
//
//	"cursor-safe-storage:v1:" + base64(safeStorage.encryptString(token))
//
// The gate is applied at Cursor's startup, so it took effect on the 3.29.15
// update restart, and from then on this collector read a value with no JWT in
// it (token_no_sub on every poll). Electron's safeStorage on macOS is Chromium's
// os_crypt: "v10" + AES-128-CBC (PKCS#7), key = PBKDF2-HMAC-SHA1(password,
// "saltysalt", 1003 iterations, 16 bytes), IV = 16 spaces, where password is the
// generic-password item "Cursor Safe Storage" in the login keychain.
//
// THE KEYCHAIN PASSWORD AND THE DECRYPTED TOKEN GET THE SAME RULES AS A
// PLAINTEXT TOKEN: read fresh every cycle (once per cycle, shared by every
// store in it), never cached across cycles, never in an error, a log line or an
// event. The derived key and the password bytes are zeroed when done; the
// string copy pbkdf2.Key takes, like any token string, lives until GC.
//
// WHO THE PROMPT NAMES. The read goes through /usr/bin/security, so macOS asks
// "security wants to use ... Cursor Safe Storage", and "Always Allow" adds
// /usr/bin/security to that item's access list. Any process of this user can
// then read Cursor's key through `security` without a prompt, which is the same
// exposure Cursor's token had in plaintext before it was encrypted. The other
// way, Security.framework from our own binary, would name promptster-teams, but
// our macOS binaries are only ad-hoc signed, so the grant would not survive a
// CLI update and the engineer would be asked again after every release.
const (
	cursorSafeStoragePrefix  = "cursor-safe-storage:v1:"
	cursorSafeStorageService = "Cursor Safe Storage"
	cursorOSCryptVersion     = "v10"
	cursorOSCryptSalt        = "saltysalt"
	cursorOSCryptIterations  = 1003
	cursorOSCryptKeyBytes    = 16

	// cursorKeychainBackoff is how long a denied or dismissed prompt is left
	// alone, so an engineer who said no is not asked every 15 minutes. It also
	// follows a second prompted success in a row: "Allow" (not "Always Allow")
	// grants one read, and asking again every cycle would be a dialog every 15
	// minutes all day. In memory only: a daemon restart (a CLI update) asks again.
	cursorKeychainBackoff = 12 * time.Hour
	// cursorKeychainTimeoutBackoff follows a prompt nobody answered. Shorter, so
	// an engineer who was away and answers a dialog left on screen (macOS may
	// keep it after the call is killed) is collected within the hour.
	cursorKeychainTimeoutBackoff = time.Hour
)

// errCursorKeychainItemMissing is no "Cursor Safe Storage" item: nothing was
// asked of the engineer, so no backoff.
var errCursorKeychainItemMissing = errors.New("cursor safe storage item not found")

// errCursorKeychainBackoff is a cycle skipped because the engineer recently
// declined (or never answered) the prompt.
var errCursorKeychainBackoff = errors.New("cursor safe storage prompt backing off")

// cursorKeychainPassword returns the "Cursor Safe Storage" password bytes. A
// var so tests never touch the developer's keychain (TestMain replaces it).
var cursorKeychainPassword = readCursorKeychainPassword

var (
	// cursorSecurityBin and cursorKeychainReadTimeout are vars for tests only.
	cursorSecurityBin = "/usr/bin/security"
	// cursorKeychainReadTimeout bounds one `security` call. While the macOS
	// prompt is up the call blocks, and the poll must not: the call is killed
	// and the cycle reports token_encrypted.
	cursorKeychainReadTimeout = 60 * time.Second
	// cursorKeychainPromptedAfter: a read slower than this was a human answering
	// a dialog; an allowed item answers in milliseconds.
	cursorKeychainPromptedAfter = 2 * time.Second

	cursorKeychainMu           sync.Mutex
	cursorKeychainBackoffUntil time.Time
	cursorKeychainLastPrompted bool // the last successful read needed a prompt
)

// readCursorKeychainPassword runs /usr/bin/security with a bound. stdout (the
// password) is returned as bytes for the caller to zero; stderr and exit text
// are never surfaced, only classified.
func readCursorKeychainPassword(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cursorKeychainReadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cursorSecurityBin, "find-generic-password", "-w", "-s", cursorSafeStorageService) // #nosec G204 -- fixed binary and arguments.
	// A killed call must return even if something still holds its stdout.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err == nil {
		return bytes.TrimRight(out, "\r\n"), nil
	}
	zero(out)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errCursorKeychainItemMissing // no `security` (not macOS)
	}
	var exit *exec.ExitError
	// 44 is errSecItemNotFound as `security` reports it.
	if errors.As(err, &exit) && exit.ExitCode() == 44 {
		return nil, errCursorKeychainItemMissing
	}
	return nil, errors.New("cursor safe storage read refused")
}

// cursorSafeStorageKey asks the keychain for the password and derives the
// AES key, honoring the backoff. The caller zeroes the key.
func cursorSafeStorageKey() ([]byte, error) {
	cursorKeychainMu.Lock()
	defer cursorKeychainMu.Unlock()
	if time.Now().Before(cursorKeychainBackoffUntil) {
		return nil, errCursorKeychainBackoff
	}
	start := time.Now()
	password, err := cursorKeychainPassword(context.Background())
	switch {
	case errors.Is(err, errCursorKeychainItemMissing):
	case errors.Is(err, context.DeadlineExceeded):
		cursorKeychainBackoffUntil = time.Now().Add(cursorKeychainTimeoutBackoff)
	case err != nil: // denied, dismissed, or no GUI session to ask in
		cursorKeychainBackoffUntil = time.Now().Add(cursorKeychainBackoff)
	default:
		prompted := time.Since(start) > cursorKeychainPromptedAfter
		if prompted && cursorKeychainLastPrompted {
			// Asked and allowed twice running: "Allow", not "Always Allow".
			cursorKeychainBackoffUntil = time.Now().Add(cursorKeychainBackoff)
		}
		cursorKeychainLastPrompted = prompted
	}
	if err != nil {
		return nil, err
	}
	defer zero(password)
	if len(password) == 0 {
		return nil, errCursorKeychainItemMissing
	}
	return pbkdf2.Key(sha1.New, string(password), []byte(cursorOSCryptSalt), cursorOSCryptIterations, cursorOSCryptKeyBytes)
}

// decryptCursorSafeStorage turns one stored "cursor-safe-storage:v1:" value
// into the token, given the derived key. Every failure is the same constant
// error: which byte was wrong is not worth a message that might carry one.
func decryptCursorSafeStorage(stored string, key []byte) (string, error) {
	errBad := errors.New("cursor safe storage value undecryptable")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, cursorSafeStoragePrefix))
	if err != nil || !bytes.HasPrefix(raw, []byte(cursorOSCryptVersion)) {
		return "", errBad
	}
	ct := raw[len(cursorOSCryptVersion):]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", errBad
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", errBad
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(plain, ct)
	defer zero(plain)
	n := int(plain[len(plain)-1])
	if n == 0 || n > aes.BlockSize {
		return "", errBad
	}
	for _, b := range plain[len(plain)-n:] {
		if int(b) != n {
			return "", errBad
		}
	}
	// A wrong key still passes the padding check about 1 time in 256. A token
	// is printable ASCII; garbage that slipped through is not, and is reported
	// as token_encrypted rather than as a token with no sub.
	body := plain[:len(plain)-n]
	if len(body) == 0 {
		return "", errBad
	}
	for _, b := range body {
		if b < 0x21 || b > 0x7e {
			return "", errBad
		}
	}
	return string(body), nil
}

// cursorCycleKey is the keychain key for ONE poll cycle: fetched on the first
// encrypted value any store in the cycle holds, shared by the rest (so extra
// profiles do not each raise a prompt), and zeroed by close at the cycle's end.
type cursorCycleKey struct {
	key     []byte
	err     error
	fetched bool
}

func (c *cursorCycleKey) get() ([]byte, error) {
	if !c.fetched {
		c.key, c.err = cursorSafeStorageKey()
		c.fetched = true
	}
	return c.key, c.err
}

func (c *cursorCycleKey) close() { zero(c.key); c.key = nil }

// decryptCursorStoreTokens replaces any encrypted value in values (the two
// token keys only) with its plaintext. The keychain is asked only when a value
// is actually encrypted, so a plaintext store behaves exactly as before and
// never prompts. A value that cannot be decrypted is blanked; the call fails
// (token_encrypted) only when that leaves no token at all, so one bad value
// never hides a usable other.
func decryptCursorStoreTokens(values map[string]string, ck *cursorCycleKey) error {
	var failed error
	for _, k := range []string{cursorAuthAccessTokenKey, cursorAuthRefreshTokenKey} {
		v := strings.TrimSpace(values[k])
		if !strings.HasPrefix(v, cursorSafeStoragePrefix) {
			continue
		}
		values[k] = ""
		key, keyErr := ck.get()
		if keyErr != nil {
			failed = credentialErr(CursorVendorAbsenceCredentialAbsent, cursorDetailTokenEncrypted, "token encrypted; keychain key unavailable")
			continue
		}
		token, err := decryptCursorSafeStorage(v, key)
		if err != nil {
			failed = credentialErr(CursorVendorAbsenceCredentialAbsent, cursorDetailTokenEncrypted, "token encrypted; could not decrypt")
			continue
		}
		values[k] = token
	}
	if failed != nil && strings.TrimSpace(values[cursorAuthAccessTokenKey]) == "" && strings.TrimSpace(values[cursorAuthRefreshTokenKey]) == "" {
		return failed
	}
	return nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
