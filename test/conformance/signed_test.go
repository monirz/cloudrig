package conformance

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// signingKey makes a key the client can sign with. The emulator never sees it:
// signing is entirely client-side, and there is no identity here to verify a
// signature against.
func signingKey(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// signedURL rewrites the URL the client produces to point at the emulator,
// which is what a developer does with a custom endpoint.
func signedURL(t *testing.T, base, bucket, object string, opts *storage.SignedURLOptions) string {
	t.Helper()

	full, err := storage.SignedURL(bucket, object, opts)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "https://storage.googleapis.com"
	return base + strings.TrimPrefix(full, prefix)
}

func TestSignedURLs(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "signed")
	base := baseOf(t)

	key := signingKey(t)
	sign := func(object, method string, expires time.Duration) string {
		return signedURL(t, base, "signed", object, &storage.SignedURLOptions{
			GoogleAccessID: "cloudrig@example.iam.gserviceaccount.com",
			PrivateKey:     key,
			Method:         method,
			Expires:        time.Now().Add(expires),
			Scheme:         storage.SigningSchemeV4,
		})
	}

	t.Run("upload with a signed PUT", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, sign("uploaded.txt", "PUT", time.Hour),
			strings.NewReader("signed content"))
		req.Header.Set("Content-Type", "text/plain")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d: %s", resp.StatusCode, body)
		}

		// It really landed, readable through the normal client.
		r, err := b.Object("uploaded.txt").NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		got, _ := io.ReadAll(r)
		if string(got) != "signed content" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("download with a signed GET", func(t *testing.T) {
		resp, err := http.Get(sign("uploaded.txt", "GET", time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		got, _ := io.ReadAll(resp.Body)
		if string(got) != "signed content" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("an expired URL still works, deliberately", func(t *testing.T) {
		// Expiry is not enforced: a URL is stamped with the client's wall
		// clock, while the emulator's time is an injected Clock a test may
		// hold anywhere. Checking it would mean reading real time here.
		url := signedURL(t, base, "signed", "uploaded.txt", &storage.SignedURLOptions{
			GoogleAccessID: "cloudrig@example.iam.gserviceaccount.com",
			PrivateKey:     key,
			Method:         "GET",
			Expires:        time.Now().Add(-time.Minute),
			Scheme:         storage.SigningSchemeV4,
		})
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("delete with a signed DELETE", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, sign("uploaded.txt", "DELETE", time.Hour), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
		if _, err := b.Object("uploaded.txt").Attrs(ctx); err == nil {
			t.Error("the object survived a signed delete")
		}
	})
}
