package capture

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Made outside Go (Python's hashlib.pbkdf2_hmac + `openssl enc -aes-128-cbc`)
// with Chromium os_crypt's macOS parameters, so the round trip is checked
// against an independent implementation, not against our own encrypt.
const (
	safeStorageVectorPassword = "test-keychain-password"
	safeStorageVectorStored   = "cursor-safe-storage:v1:djEwC2VxEX5qqs+bri3TEiie2dwrIxv8jX6bzxckKUg8iWXyG2OueY8dq7POzyYYbZz0vXY3xecHV6O2QRZIbiXf6NnpqaOkJzRuGHENEOvCjk8="
	// A synthetic unsigned JWT ({"alg":"none"}, sub "user_vector", signature
	// "sig"), never issued. The stored vector above encrypts exactly this string,
	// so it can't change; the directive stops generic-api-key reading it as a key.
	safeStorageVectorToken = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1c2VyX3ZlY3RvciIsImV4cCI6NDEwMjQ0NDgwMH0.sig" //gitleaks:allow
)

// fakeKeychain swaps the keychain read for the test and clears the backoff,
// counting how often the engineer would have been asked.
func fakeKeychain(t *testing.T, read func(context.Context) ([]byte, error)) *int {
	t.Helper()
	calls := 0
	prev := cursorKeychainPassword
	cursorKeychainPassword = func(ctx context.Context) ([]byte, error) { calls++; return read(ctx) }
	cursorKeychainBackoffUntil, cursorKeychainLastPrompted = time.Time{}, false
	t.Cleanup(func() {
		cursorKeychainPassword = prev
		cursorKeychainBackoffUntil, cursorKeychainLastPrompted = time.Time{}, false
	})
	return &calls
}

func vectorPassword(context.Context) ([]byte, error) {
	return []byte(safeStorageVectorPassword), nil
}

func TestCursorSafeStorageKnownVectorReadsAsTheToken(t *testing.T) {
	calls := fakeKeychain(t, vectorPassword)
	store := makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: safeStorageVectorStored, cursorAuthRefreshTokenKey: safeStorageVectorStored,
	})
	cred, err := readCursorCredentialAt(store)
	if err != nil {
		t.Fatal(err)
	}
	if cred.token != safeStorageVectorToken || cred.accountRef != cursorSubAccountRef("user_vector") {
		t.Fatalf("decrypted credential wrong: ref %q", cred.accountRef)
	}
	if *calls != 1 {
		t.Fatalf("keychain asked %d times for one store, want 1", *calls)
	}
}

func TestCursorSafeStoragePlaintextNeverAsksTheKeychain(t *testing.T) {
	calls := fakeKeychain(t, vectorPassword)
	live := jwtWithExpiry(t, time.Now().Add(time.Hour), "plain")
	cred, err := readCursorCredentialAt(makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: live}))
	if err != nil || cred.token != live {
		t.Fatalf("plaintext token changed: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("plaintext store asked the keychain %d times", *calls)
	}
}

func TestCursorSafeStorageFailuresAreTokenEncrypted(t *testing.T) {
	wrongKey := func(context.Context) ([]byte, error) { return []byte("not-the-password"), nil }
	missing := func(context.Context) ([]byte, error) { return nil, errCursorKeychainItemMissing }
	notV10 := cursorSafeStoragePrefix + base64.StdEncoding.EncodeToString([]byte("v11"+strings.Repeat("x", 16)))
	shortBlock := cursorSafeStoragePrefix + base64.StdEncoding.EncodeToString([]byte("v10short"))
	cases := []struct {
		name   string
		stored string
		read   func(context.Context) ([]byte, error)
	}{
		{"malformed base64", cursorSafeStoragePrefix + "%%%not base64%%%", vectorPassword},
		{"wrong key or bad padding", safeStorageVectorStored, wrongKey},
		{"not v10", notV10, vectorPassword},
		{"partial block", shortBlock, vectorPassword},
		{"absent keychain item", safeStorageVectorStored, missing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeKeychain(t, tc.read)
			_, err := readCursorCredentialAt(makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: tc.stored}))
			if got := cursorCredentialAbsenceDetail(err); got != cursorDetailTokenEncrypted {
				t.Fatalf("detail = %q, want token_encrypted (err %v)", got, err)
			}
			if cursorCredentialAbsence(err) != CursorVendorAbsenceCredentialAbsent {
				t.Fatalf("reason = %q", cursorCredentialAbsence(err))
			}
			ev := buildCursorCredentialAbsenceEvent("dev", cursorAccountRefUnknown, err, time.Now())
			for _, v := range ev.Data.(map[string]interface{}) {
				s, _ := v.(string)
				for _, secret := range []string{safeStorageVectorPassword, safeStorageVectorToken, "not-the-password"} {
					if strings.Contains(s, secret) || strings.Contains(err.Error(), secret) {
						t.Fatal("a secret reached the error or the absence")
					}
				}
			}
		})
	}
	// An absent item asked nothing of the engineer, so it is not backed off.
	calls := fakeKeychain(t, missing)
	store := makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: safeStorageVectorStored})
	for i := 0; i < 2; i++ {
		_, _ = readCursorCredentialAt(store)
	}
	if *calls != 2 {
		t.Fatalf("absent item looked up %d times over two cycles, want 2", *calls)
	}
}

// One undecryptable token must not hide a usable other.
func TestCursorSafeStorageBadRefreshKeepsGoodAccess(t *testing.T) {
	fakeKeychain(t, vectorPassword)
	cred, err := readCursorCredentialAt(makeCursorStateDB(t, map[string]string{
		cursorAuthAccessTokenKey: safeStorageVectorStored, cursorAuthRefreshTokenKey: cursorSafeStoragePrefix + "!!",
	}))
	if err != nil || cred.token != safeStorageVectorToken {
		t.Fatalf("good access token lost to a bad refresh token: %v", err)
	}
}

// A denied or unanswered prompt is not raised again every cycle.
func TestCursorSafeStorageDenialBacksOff(t *testing.T) {
	calls := fakeKeychain(t, func(context.Context) ([]byte, error) { return nil, errors.New("denied") })
	store := makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: safeStorageVectorStored})
	for i := 0; i < 3; i++ {
		_, err := readCursorCredentialAt(store)
		if cursorCredentialAbsenceDetail(err) != cursorDetailTokenEncrypted {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if *calls != 1 {
		t.Fatalf("engineer asked %d times in one backoff window, want 1", *calls)
	}
}

// The real exec path: a `security` that never answers is killed at the bound,
// one that reports errSecItemNotFound (44) is "missing", and one that answers
// hands back the password without its newline.
func TestReadCursorKeychainPasswordExecPaths(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	prevBin, prevTimeout := cursorSecurityBin, cursorKeychainReadTimeout
	t.Cleanup(func() { cursorSecurityBin, cursorKeychainReadTimeout = prevBin, prevTimeout })
	cursorKeychainReadTimeout = 300 * time.Millisecond

	cursorSecurityBin = script("hang", "exec sleep 30")
	start := time.Now()
	if _, err := readCursorKeychainPassword(context.Background()); err == nil {
		t.Fatal("hung security call returned a password")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("hung security call blocked for %v", time.Since(start))
	}

	cursorSecurityBin = script("missing", `echo "The specified item could not be found in the keychain." >&2; exit 44`)
	if _, err := readCursorKeychainPassword(context.Background()); !errors.Is(err, errCursorKeychainItemMissing) {
		t.Fatalf("exit 44 = %v, want item missing", err)
	}

	cursorSecurityBin = script("denied", `echo "User canceled the operation." >&2; exit 128`)
	if _, err := readCursorKeychainPassword(context.Background()); err == nil || errors.Is(err, errCursorKeychainItemMissing) || strings.Contains(err.Error(), "canceled") {
		t.Fatalf("denied = %v, want a constant refusal", err)
	}

	cursorSecurityBin = script("ok", "echo "+safeStorageVectorPassword)
	pw, err := readCursorKeychainPassword(context.Background())
	if err != nil || string(pw) != safeStorageVectorPassword {
		t.Fatalf("password read = %v", err)
	}

	cursorSecurityBin = filepath.Join(dir, "no-such-binary")
	if _, err := readCursorKeychainPassword(context.Background()); !errors.Is(err, errCursorKeychainItemMissing) {
		t.Fatalf("no security binary = %v, want item missing", err)
	}
}

// Extra profiles in one cycle share one keychain read, not one prompt each.
func TestCursorSafeStorageOneKeychainReadPerCycle(t *testing.T) {
	calls := fakeKeychain(t, vectorPassword)
	ck := &cursorCycleKey{}
	for i := 0; i < 3; i++ {
		store := makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: safeStorageVectorStored})
		if cred, err := readCursorCredentialWith(store, ck); err != nil || cred.token != safeStorageVectorToken {
			t.Fatalf("store %d: %v", i, err)
		}
	}
	ck.close()
	if *calls != 1 {
		t.Fatalf("keychain read %d times in one cycle, want 1", *calls)
	}
}

// "Allow" (not "Always Allow") grants one read: a second prompted success in a
// row backs off instead of raising the dialog every 15 minutes. An item that
// answers without a prompt is read every cycle.
func TestCursorSafeStorageAllowOnceBacksOff(t *testing.T) {
	prev := cursorKeychainPromptedAfter
	t.Cleanup(func() { cursorKeychainPromptedAfter = prev })
	store := makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: safeStorageVectorStored})

	cursorKeychainPromptedAfter = -1 // every read "needed a prompt"
	calls := fakeKeychain(t, vectorPassword)
	for i := 0; i < 4; i++ {
		_, _ = readCursorCredentialAt(store)
	}
	if *calls != 2 {
		t.Fatalf("prompted %d times over four cycles, want 2", *calls)
	}

	cursorKeychainPromptedAfter = time.Hour // every read answered silently
	calls = fakeKeychain(t, vectorPassword)
	for i := 0; i < 4; i++ {
		if _, err := readCursorCredentialAt(store); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 4 {
		t.Fatalf("silent reads %d over four cycles, want 4", *calls)
	}
}

// An unanswered prompt backs off for an hour, not the twelve a "Deny" gets.
func TestCursorSafeStorageTimeoutBacksOffShort(t *testing.T) {
	fakeKeychain(t, func(context.Context) ([]byte, error) { return nil, context.DeadlineExceeded })
	_, err := readCursorCredentialAt(makeCursorStateDB(t, map[string]string{cursorAuthAccessTokenKey: safeStorageVectorStored}))
	if cursorCredentialAbsenceDetail(err) != cursorDetailTokenEncrypted {
		t.Fatal(err)
	}
	if left := time.Until(cursorKeychainBackoffUntil); left <= 0 || left > cursorKeychainTimeoutBackoff {
		t.Fatalf("timeout backoff %v, want at most %v", left, cursorKeychainTimeoutBackoff)
	}
}
