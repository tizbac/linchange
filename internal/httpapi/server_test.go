package httpapi_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/pquerna/otp"
	libtotp "github.com/pquerna/otp/totp"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/captcha"
	"github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/auth/totp"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/httpapi"
	"github.com/linchange/linchange/internal/imapimport"
	"github.com/linchange/linchange/internal/store"
)

const bobSecret = "JBSWY3DPEHPK3PXP"

// testConfig returns a valid, deterministic config the server is built from. The
// same value is also used to build the verifier that enrols bob, so the two
// sides agree on the encryption key and TOTP parameters.
func testConfig() *config.Config {
	return &config.Config{
		Auth: config.Auth{
			Local: config.LocalAuth{
				Argon2:            config.Argon2Params{MemoryKib: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32},
				MinPasswordLength: 12,
			},
			EncryptionKey: "test-encryption-key-0123456789abcdef0123456789",
			JWT: config.JWTAuth{
				Secret:     strings.Repeat("http-test-signing-key-0123456789", 2),
				Issuer:     "linchange-test",
				AccessTTL:  15 * time.Minute,
				RefreshTTL: 7 * 24 * time.Hour,
			},
			TOTP:      config.TOTPAuth{Enabled: true, Issuer: "linchange", Window: 1, RecoveryCodes: 8},
			Challenge: config.Challenge{TTL: 5 * time.Minute},
			Lockout: config.Lockout{
				Account: config.LockoutScope{MaxAttempts: 1000, Window: 15 * time.Minute, Duration: time.Hour},
				IP:      config.LockoutScope{MaxAttempts: 1000, Window: 15 * time.Minute, Duration: time.Hour},
			},
			Reset:         config.ResetAuth{Enabled: true, TokenTTL: 5 * time.Minute},
			Impersonation: config.Impersonation{Allowed: true},
		},
		// Join and reply links in invitations are built from this, and an
		// empty host would produce a URL no assertion about a link could be
		// written against.
		Meetings: config.Meetings{PublicURL: "http://mail.test.invalid"},
	}
}

func testMigratedDB(t *testing.T) *sqlx.DB {
	t.Helper()

	base := os.Getenv("LINCHANGE_TEST_DATABASE_URL")
	if base == "" {
		base = "postgres://linchange:linchange@127.0.0.1:55432/linchange?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := store.Open(ctx, store.Options{
		URL: base, MaxOpen: 2, ConnectTimeout: 10 * time.Second,
		ApplicationName: "linchange-http-test-admin",
	})
	if err != nil {
		if os.Getenv("LINCHANGE_TEST_DATABASE_URL") != "" {
			t.Fatalf("%s is set to %q but is unreachable: %v", "LINCHANGE_TEST_DATABASE_URL", base, err)
		}
		t.Skipf("no test database at %s (%v); run `make up` or set LINCHANGE_TEST_DATABASE_URL", base, err)
	}

	name := fmt.Sprintf("lc_http_test_%d_%d", time.Now().UnixNano(), rand.Int63())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	db, err := store.Open(ctx, store.Options{
		URL: replaceDatabase(base, name), MaxOpen: 8, MaxIdle: 4,
		ConnectTimeout: 10 * time.Second, ApplicationName: "linchange-http-test",
	})
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: dropping %s: %v", name, err)
		}
		admin.Close()
	})

	if err := store.Migrate(ctx, db, nil); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func replaceDatabase(dsn, name string) string {
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		rest = rest[:q]
	}
	return dsn[:i+1] + name + dsn[i+1+len(rest):]
}

func exec(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("executing %q: %v", query, err)
	}
}

func seedUser(t *testing.T, db *sqlx.DB, address, source string, mailbox, enabled bool) {
	t.Helper()
	exec(t, db, `INSERT INTO domains (name) VALUES ('example.com') ON CONFLICT DO NOTHING`)
	local := address
	if i := strings.IndexByte(address, '@'); i >= 0 {
		local = address[:i]
	}
	// The domain_admin source carries the administration grant with it, so the
	// row has to say so: the schema refuses a domain admin who is not one.
	exec(t, db,
		`INSERT INTO users (address, address_local, source, mailbox_enabled, enabled, is_domain_admin)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		address, local, source, mailbox, enabled, source == "domain_admin")
}

func setPassword(t *testing.T, db *sqlx.DB, address, plaintext string) {
	t.Helper()
	hash, err := password.Hash(plaintext, testConfig().Auth.Local.Argon2)
	if err != nil {
		t.Fatalf("hashing %s: %v", address, err)
	}
	if err := store.NewCredentials().Set(context.Background(), db, address, hash); err != nil {
		t.Fatalf("storing credential for %s: %v", address, err)
	}
}

func addSecondaryEmail(t *testing.T, db *sqlx.DB, address, email string, verified, isPrimary bool) {
	t.Helper()
	var at any
	if verified {
		at = time.Now()
	}
	exec(t, db,
		`INSERT INTO user_secondary_emails (address, email, is_primary, verified_at)
		 VALUES ($1, $2, $3, $4)`, address, email, isPrimary, at)
}

// enrolBob seals a TOTP secret under the same key the server uses and stores an
// active enrolment, so a test can then sign in and present a real code.
func enrolBob(t *testing.T, db *sqlx.DB) {
	t.Helper()
	cfg := testConfig()
	v, err := totp.New(cfg.Auth.TOTP, cfg.Auth.EncryptionKey)
	if err != nil {
		t.Fatalf("building the TOTP verifier: %v", err)
	}
	enc, err := v.Encrypt(bobSecret, nil)
	if err != nil {
		t.Fatalf("encrypting the TOTP secret: %v", err)
	}
	if err := store.NewTOTP().Store(context.Background(), db, "bob@example.com", enc.Ciphertext, nil, true); err != nil {
		t.Fatalf("storing the TOTP enrolment: %v", err)
	}
}

func totpCode(t *testing.T, at time.Time) string {
	t.Helper()
	code, err := libtotp.GenerateCodeCustom(bobSecret, at.UTC(), libtotp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	return code
}

// testBlobs opens a blob store in the test's temporary directory.
func testBlobs(t *testing.T) *blobstore.Store {
	t.Helper()

	blobs, err := blobstore.Open(blobstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("opening the blob store: %v", err)
	}
	return blobs
}

func testCaptchaKey() []byte {
	sum := sha256.Sum256([]byte("httpapi-test-captcha-key"))
	return sum[:]
}

// testCaptcha builds a challenge manager with a key the test also knows, so it
// can answer a challenge it issued without the package exposing the answer.
func testCaptcha(t *testing.T) *captcha.Manager {
	t.Helper()
	m, err := captcha.NewWithKey(testCaptchaKey(), 10*time.Minute)
	if err != nil {
		t.Fatalf("building the captcha manager: %v", err)
	}
	return m
}

func solveCaptcha(t *testing.T, challenge captcha.Challenge) string {
	t.Helper()
	block, err := aes.NewCipher(testCaptchaKey())
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("sealed box: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(challenge.Token)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		t.Fatalf("opening the challenge: %v", err)
	}
	return string(plain[9:])
}

func resetRequestWithCaptcha(t *testing.T, srv *httpapi.Server, login string) (int, map[string]any) {
	t.Helper()
	m := testCaptcha(t)
	challenge, err := m.Issue()
	if err != nil {
		t.Fatalf("issuing a challenge: %v", err)
	}
	return do(t, srv, http.MethodPost, "/api/v1/auth/reset/request",
		map[string]string{
			"login":          login,
			"captcha_token":  challenge.Token,
			"captcha_answer": solveCaptcha(t, challenge),
		}, nil)
}

func newServer(t *testing.T, db *sqlx.DB) *httpapi.Server {
	t.Helper()
	blobs := testBlobs(t)
	srv, err := httpapi.New(httpapi.Options{
		DB: db, Config: testConfig(), Blobs: blobs,
		Imports: imapimport.New(db, blobs, nil, testConfig().Auth.EncryptionKey),
		Captcha: testCaptcha(t),
	})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return srv
}

// str reads a string field, treating an absent key (an omitempty token) as "".
func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// do issues a request against the router and decodes the JSON body, if any.
func do(t *testing.T, srv *httpapi.Server, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	var out map[string]any
	if rr.Body.Len() > 0 {
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("decoding response %q: %v", rr.Body.String(), err)
		}
	}
	return rr.Code, out
}

func TestHealthz(t *testing.T) {
	srv := newServer(t, testMigratedDB(t))

	code, body := do(t, srv, http.MethodGet, "/healthz", nil, nil)
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz = %d %v, want 200 ok", code, body)
	}
}

func TestLoginLocalIssuesTokens(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	srv := newServer(t, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "alice@example.com", "password": "correct horse battery staple"}, nil)

	if code != http.StatusOK {
		t.Fatalf("login = %d %v, want 200", code, body)
	}
	if str(body, "access_token") == "" || str(body, "refresh_token") == "" {
		t.Fatalf("both tokens must be issued, got %v", body)
	}
	if body["requires_second_factor"] != false {
		t.Errorf("a local account has no second factor, got %v", body["requires_second_factor"])
	}
}

func TestLoginWrongPasswordOrUnknownAddressIsRejected(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	srv := newServer(t, db)

	for _, tc := range []struct {
		name     string
		login    string
		password string
	}{
		{"wrong password", "alice@example.com", "not the right one"},
		{"unknown address", "ghost@example.com", "whatever it may be"},
	} {
		code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
			map[string]string{"login": tc.login, "password": tc.password}, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("%s: login = %d %v, want 401", tc.name, code, body)
		}
	}
}

func TestLoginSecondFactorFlow(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "bob@example.com", "local", true, true)
	setPassword(t, db, "bob@example.com", "correct horse battery staple")
	enrolBob(t, db)
	srv := newServer(t, db)

	// Step one stops at a challenge and issues no tokens.
	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "bob@example.com", "password": "correct horse battery staple"}, nil)
	if code != http.StatusOK {
		t.Fatalf("login = %d %v, want 200", code, body)
	}
	if body["requires_second_factor"] != true || body["challenge_id"] == "" {
		t.Fatalf("a TOTP account must return a challenge, got %v", body)
	}
	if str(body, "access_token") != "" || str(body, "refresh_token") != "" {
		t.Fatalf("no tokens may be issued before the second factor: %v", body)
	}
	challengeID, _ := body["challenge_id"].(string)

	// A wrong code is refused without consuming the challenge.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/second-factor",
		map[string]string{"challenge_id": challengeID, "code": "000000"}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong code = %d %v, want 401", code, body)
	}

	// The right code completes the login and issues tokens.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/second-factor",
		map[string]string{"challenge_id": challengeID, "code": totpCode(t, time.Now())}, nil)
	if code != http.StatusOK {
		t.Fatalf("second factor = %d %v, want 200", code, body)
	}
	if str(body, "access_token") == "" || str(body, "refresh_token") == "" {
		t.Fatalf("a completed second factor must issue tokens, got %v", body)
	}
}

func TestRefreshThenLogoutRevokes(t *testing.T) {
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	setPassword(t, db, "alice@example.com", "correct horse battery staple")
	srv := newServer(t, db)

	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"login": "alice@example.com", "password": "correct horse battery staple"}, nil)
	if code != http.StatusOK {
		t.Fatalf("login = %d %v, want 200", code, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("login did not issue tokens: %v", body)
	}

	// A live refresh mints a fresh access token.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusOK || str(body, "access_token") == "" {
		t.Fatalf("refresh = %d %v, want a new access token", code, body)
	}

	// Logging out ends the session the access token points at.
	code, _ = do(t, srv, http.MethodPost, "/api/v1/auth/logout", nil,
		map[string]string{"Authorization": "Bearer " + access})
	if code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", code)
	}

	// The old refresh token is now dead with its session.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout = %d %v, want 401", code, body)
	}
}

func TestProfileOutOfOffice(t *testing.T) {
	db := testMigratedDB(t)
	srv := newServer(t, db)
	headers := mailUser(t, srv, db, "alice@example.com")

	code, body := do(t, srv, http.MethodGet, "/api/v1/profile", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("profile = %d %v", code, body)
	}
	initial, _ := body["out_of_office"].(map[string]any)
	if initial == nil || initial["enabled"] == true {
		t.Fatalf("initial out_of_office = %v, want disabled", body["out_of_office"])
	}

	start := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	end := start.Add(24 * time.Hour)
	code, body = do(t, srv, http.MethodPut, "/api/v1/profile/out-of-office", map[string]any{
		"enabled": true,
		"start":   start.Format(time.RFC3339),
		"end":     end.Format(time.RFC3339),
		"subject": "Away",
		"message": "I am away until tomorrow.",
	}, headers)
	if code != http.StatusOK {
		t.Fatalf("saving out-of-office = %d %v", code, body)
	}
	if body["enabled"] != true || body["subject"] != "Away" {
		t.Errorf("saved setting = %v", body)
	}

	code, body = do(t, srv, http.MethodGet, "/api/v1/profile", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("profile after save = %d %v", code, body)
	}
	saved, _ := body["out_of_office"].(map[string]any)
	if saved == nil || saved["enabled"] != true || saved["message"] != "I am away until tomorrow." {
		t.Errorf("stored out_of_office = %v", body["out_of_office"])
	}

	code, _ = do(t, srv, http.MethodPut, "/api/v1/profile/out-of-office", map[string]any{
		"enabled": true,
		"start":   end.Format(time.RFC3339),
		"end":     start.Format(time.RFC3339),
		"message": "nonsense",
	}, headers)
	if code != http.StatusBadRequest {
		t.Errorf("end before start = %d, want 400", code)
	}
}

func TestResetEndpoints(t *testing.T) {
	ctx := context.Background()
	db := testMigratedDB(t)
	seedUser(t, db, "alice@example.com", "local", true, true)
	addSecondaryEmail(t, db, "alice@example.com", "alice.other@example.com", true, true)
	srv := newServer(t, db)

	// A request without a challenge is refused before the address is read.
	code, body := do(t, srv, http.MethodPost, "/api/v1/auth/reset/request",
		map[string]string{"login": "alice@example.com"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("reset request without a captcha = %d %v, want 400", code, body)
	}

	// A request always answers generically and stores a token row.
	code, body = resetRequestWithCaptcha(t, srv, "alice@example.com")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("reset request = %d %v, want 200 ok", code, body)
	}
	var count int
	if err := db.GetContext(ctx, &count, `SELECT count(*) FROM reset_tokens WHERE address = $1`, "alice@example.com"); err != nil {
		t.Fatalf("counting reset tokens: %v", err)
	}
	if count != 1 {
		t.Fatalf("a verified reset-able account should have one token row, got %d", count)
	}

	// An unknown address answers the same way and stores nothing.
	code, body = resetRequestWithCaptcha(t, srv, "ghost@example.com")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("reset request for unknown address = %d %v, want 200 ok", code, body)
	}
	if err := db.GetContext(ctx, &count, `SELECT count(*) FROM reset_tokens WHERE address = $1`, "ghost@example.com"); err != nil {
		t.Fatalf("counting reset tokens: %v", err)
	}
	if count != 0 {
		t.Fatalf("an unknown address must not store a token, got %d", count)
	}

	// Completing with a token nobody issued is refused specifically.
	code, body = do(t, srv, http.MethodPost, "/api/v1/auth/reset/complete",
		map[string]string{"token": "nobody-issued-this", "password": "correct horse battery staple"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("reset complete with a bad token = %d %v, want 400", code, body)
	}
}
