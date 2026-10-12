package main

// Account-security primitives for web-auth: password hashing, brute-force
// limiting and real-name document validation.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- password hashing ----------------------------------------------------------
//
// Passwords used to be stored as a single salted SHA-256 — GPU-crackable at
// billions of guesses per second. They are now PBKDF2-HMAC-SHA256 at the OWASP
// 2023 work factor (stdlib crypto/pbkdf2, no new dependency). The iteration
// count is stored in the hash, so it can be raised later without breaking old
// hashes; legacy "salt$sha256" hashes still verify.

// pbkdf2Iterations is the work factor for NEW hashes (tests lower it for speed;
// the stored hash always records the count it was made with).
var pbkdf2Iterations = 600_000

const pbkdf2Prefix = "pbkdf2-sha256"

func hashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Prefix, pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk))
}

// verifyPassword checks input against a stored hash (PBKDF2 or legacy).
func verifyPassword(stored, input string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) == 4 && parts[0] == pbkdf2Prefix {
		iter, err := strconv.Atoi(parts[1])
		if err != nil || iter < 1 {
			return false
		}
		salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
		want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
		if err1 != nil || err2 != nil {
			return false
		}
		got, err := pbkdf2.Key(sha256.New, input, salt, iter, len(want))
		if err != nil {
			return false
		}
		return subtle.ConstantTimeCompare(got, want) == 1
	}
	// Legacy salted SHA-256 ("salt$hex").
	if len(parts) == 2 {
		return hmac.Equal([]byte(parts[1]), []byte(sha256Hex(parts[0]+input)))
	}
	return false
}

// --- brute-force limiting --------------------------------------------------------

// attemptLimiter locks a key (an email, an account's MFA, ...) after max
// failures inside window, for lockout. 07§2.4 specifies the 5-failure /
// 15-minute lockout; it was documented but not implemented, so passwords and
// 6-digit MFA codes could be guessed without limit.
//
// Entries expire: a key whose lockout is over and whose failures are all
// older than window carries no information, and is swept (see sweepLocked).
// Without that, every distinct email ever submitted stayed in memory until a
// successful login for it — an email spray grew the map without bound.
type attemptLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	lockout time.Duration
	state   map[string]*attemptState

	ops       int       // Fail calls since the last sweep
	lastSweep time.Time // time of the last sweep
}

// minSweepOps is the smallest number of Fail calls between sweeps. A sweep is
// O(entries); requiring at least max(minSweepOps, entries) calls in between
// keeps the amortised cost per call constant however large the map grows.
const minSweepOps = 1024

type attemptState struct {
	failures    []time.Time
	lockedUntil time.Time
}

func newAttemptLimiter(max int, window, lockout time.Duration) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, lockout: lockout, state: map[string]*attemptState{}}
}

// Locked reports whether key is currently locked out.
func (l *attemptLimiter) Locked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state[key]
	return s != nil && now.Before(s.lockedUntil)
}

// Fail records a failure and reports whether key is now locked.
func (l *attemptLimiter) Fail(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops++
	if l.ops >= max(minSweepOps, len(l.state)) || now.Sub(l.lastSweep) >= l.window {
		l.sweepLocked(now)
	}
	s := l.state[key]
	if s == nil {
		s = &attemptState{}
		l.state[key] = s
	}
	kept := s.failures[:0]
	for _, t := range s.failures {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	s.failures = append(kept, now)
	if len(s.failures) >= l.max {
		s.lockedUntil = now.Add(l.lockout)
		s.failures = nil
		return true
	}
	return false
}

// Reset clears key after a success.
func (l *attemptLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, key)
}

// sweepLocked drops entries that are no longer locked and hold no failure
// inside the window. Caller holds l.mu.
func (l *attemptLimiter) sweepLocked(now time.Time) {
	for k, s := range l.state {
		if now.Before(s.lockedUntil) {
			continue
		}
		live := false
		for _, t := range s.failures {
			if now.Sub(t) < l.window {
				live = true
				break
			}
		}
		if !live {
			delete(l.state, k)
		}
	}
	l.ops = 0
	l.lastSweep = now
}

var (
	// loginLimiter is keyed by the submitted email, whether or not the
	// account exists, so a lockout response reveals nothing about existence.
	loginLimiter = newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)
	// mfaLimiter is keyed per account across all three code-checking routes.
	mfaLimiter = newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)
	// passwordChangeLimiter stops old-password guessing through a live session.
	passwordChangeLimiter = newAttemptLimiter(5, 15*time.Minute, 15*time.Minute)
)

func loginKey(email string) string { return "login:" + normalizeEmail(email) }

// normalizeEmail is the one canonical form of a login email — used for the
// accounts map key, the stored AccountName and the lockout key, so they agree
// with each other and with account_db's case-insensitive unique key.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// --- real-name documents ------------------------------------------------------
//
// Real-name verification only checked lengths, so any 15 characters "verified"
// an identity (and fed the trial anti-abuse dedup). Without a third-party
// verification channel the platform cannot prove a document belongs to the
// person, but it can refuse malformed ones: the national-standard check digits
// below reject random input, and the plaintext number is no longer kept.

var idWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

const idCheckChars = "10X98765432"

// validResidentID validates an 18-digit resident identity number (GB 11643):
// digits, a real birth date and the mod-11 check character.
func validResidentID(id string, now time.Time) bool {
	id = strings.ToUpper(id)
	if len(id) != 18 {
		return false
	}
	sum := 0
	for i := 0; i < 17; i++ {
		c := id[i]
		if c < '0' || c > '9' {
			return false
		}
		sum += int(c-'0') * idWeights[i]
	}
	if id[17] != idCheckChars[sum%11] {
		return false
	}
	birth, err := time.Parse("20060102", id[6:14])
	if err != nil || birth.Year() < 1900 || birth.After(now) {
		return false
	}
	return true
}

const creditCodeChars = "0123456789ABCDEFGHJKLMNPQRTUWXY"

var creditWeights = [17]int{1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28}

// validCreditCode validates an 18-character unified social credit code
// (GB 32100): restricted alphabet and the mod-31 check character.
func validCreditCode(code string) bool {
	code = strings.ToUpper(code)
	if len(code) != 18 {
		return false
	}
	sum := 0
	for i := 0; i < 17; i++ {
		v := strings.IndexByte(creditCodeChars, code[i])
		if v < 0 {
			return false
		}
		sum += v * creditWeights[i]
	}
	check := (31 - sum%31) % 31
	return code[17] == creditCodeChars[check]
}

// validPersonName bounds a real/enterprise name: present, ≤ 64 runes, no
// control characters.
func validPersonName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > 64 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// identityPepperEnv keys the document fingerprint. A plain SHA-256 of an ID
// number is reversible by enumeration (the space is small and structured); a
// keyed HMAC is not, without the key.
const identityPepperEnv = "EULER_IDENTITY_PEPPER"

var identityPepper = loadIdentityPepper()

func loadIdentityPepper() []byte {
	if v := strings.TrimSpace(os.Getenv(identityPepperEnv)); v != "" {
		return []byte(v)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	slog.Warn("no identity pepper configured — ephemeral (dev only)", "set", identityPepperEnv)
	return b
}

// identityFingerprint is the dedup key for a document number.
func identityFingerprint(kind, number string) string {
	m := hmac.New(sha256.New, identityPepper)
	m.Write([]byte(kind + ":" + strings.ToUpper(strings.TrimSpace(number))))
	return hex.EncodeToString(m.Sum(nil))
}
