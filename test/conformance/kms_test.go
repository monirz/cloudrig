package conformance

import (
	"bytes"
	"context"
	"hash/crc32"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/monirz/cloudrig"
)

func kmsClient(t *testing.T) (*kms.KeyManagementClient, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()
	c, err := kms.NewKeyManagementClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("kms.NewKeyManagementClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

// mkKey creates a key ring and a symmetric key in it, and returns the key.
func mkKey(t *testing.T, c *kms.KeyManagementClient, ctx context.Context) *kmspb.CryptoKey {
	t.Helper()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
		Parent: "projects/p/locations/global", KeyRingId: "ring",
	})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
		Parent:      ring.GetName(),
		CryptoKeyId: "key",
		CryptoKey:   &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT},
	})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	return key
}

func crc(b []byte) *wrapperspb.Int64Value {
	return wrapperspb.Int64(int64(crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))))
}

func TestKMSRoundTrip(t *testing.T) {
	c, ctx := kmsClient(t)
	key := mkKey(t, c, ctx)

	if got := key.GetPrimary().GetName(); got != key.GetName()+"/cryptoKeyVersions/1" {
		t.Fatalf("primary = %q, want version 1", got)
	}

	plaintext, aad := []byte("a work file's key"), []byte("delivery-7")
	enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:                              key.GetName(),
		Plaintext:                         plaintext,
		PlaintextCrc32C:                   crc(plaintext),
		AdditionalAuthenticatedData:       aad,
		AdditionalAuthenticatedDataCrc32C: crc(aad),
	})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// What the client library's own samples check before trusting a result.
	if !enc.GetVerifiedPlaintextCrc32C() || !enc.GetVerifiedAdditionalAuthenticatedDataCrc32C() {
		t.Error("the request checksums were not reported verified")
	}
	if enc.GetCiphertextCrc32C().GetValue() != crc(enc.GetCiphertext()).GetValue() {
		t.Error("ciphertext_crc32c does not match the ciphertext")
	}
	if bytes.Contains(enc.GetCiphertext(), plaintext) {
		t.Error("the plaintext is visible in the ciphertext")
	}

	dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{
		Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: aad,
	})
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec.GetPlaintext(), plaintext) || !dec.GetUsedPrimary() {
		t.Errorf("Decrypt = %q (used primary %v), want %q", dec.GetPlaintext(), dec.GetUsedPrimary(), plaintext)
	}

	refused := func(what string, req *kmspb.DecryptRequest) {
		t.Helper()
		if _, err := c.Decrypt(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", what, err)
		}
	}
	refused("wrong additional data", &kmspb.DecryptRequest{
		Name: key.GetName(), Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: []byte("other"),
	})
	tampered := bytes.Clone(enc.GetCiphertext())
	tampered[len(tampered)-1] ^= 1
	refused("tampered ciphertext", &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: tampered, AdditionalAuthenticatedData: aad})
	refused("short ciphertext", &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: []byte("x")})

	if _, err := c.Encrypt(ctx, &kmspb.EncryptRequest{
		Name: key.GetName(), Plaintext: plaintext, PlaintextCrc32C: wrapperspb.Int64(1),
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a wrong plaintext checksum: err = %v, want InvalidArgument", err)
	}
}

// TestKMSRotation covers a new version: old ciphertext still opens, and only
// a primary move changes what Encrypt uses.
func TestKMSRotation(t *testing.T) {
	c, ctx := kmsClient(t)
	key := mkKey(t, c, ctx)

	old, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("before")})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil {
		t.Fatalf("CreateCryptoKeyVersion: %v", err)
	}
	if v2.GetName() != key.GetName()+"/cryptoKeyVersions/2" || v2.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Errorf("new version = %s in %s", v2.GetName(), v2.GetState())
	}

	still, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")})
	if err != nil || still.GetName() != key.GetPrimary().GetName() {
		t.Errorf("before the primary moved, Encrypt used %q, %v; want version 1", still.GetName(), err)
	}

	moved, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{
		Name: key.GetName(), CryptoKeyVersionId: "2",
	})
	if err != nil || moved.GetPrimary().GetName() != v2.GetName() {
		t.Fatalf("UpdateCryptoKeyPrimaryVersion = %v, %v", moved.GetPrimary().GetName(), err)
	}
	now, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("after")})
	if err != nil || now.GetName() != v2.GetName() {
		t.Errorf("after the primary moved, Encrypt used %q, %v", now.GetName(), err)
	}

	dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: old.GetCiphertext()})
	if err != nil || string(dec.GetPlaintext()) != "before" || dec.GetUsedPrimary() {
		t.Errorf("old ciphertext: %q (used primary %v), %v", dec.GetPlaintext(), dec.GetUsedPrimary(), err)
	}

	var names []string
	it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName()})
	for {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, v.GetName())
	}
	if len(names) != 2 {
		t.Errorf("versions = %v, want 2", names)
	}
}

func TestKMSErrors(t *testing.T) {
	c, ctx := kmsClient(t)
	key := mkKey(t, c, ctx)

	cases := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"duplicate ring", func() error {
			_, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/p/locations/global", KeyRingId: "ring"})
			return err
		}, codes.AlreadyExists},
		{"duplicate key", func() error {
			_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
				Parent: "projects/p/locations/global/keyRings/ring", CryptoKeyId: "key",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT},
			})
			return err
		}, codes.AlreadyExists},
		{"key in a missing ring", func() error {
			_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
				Parent: "projects/p/locations/global/keyRings/nope", CryptoKeyId: "k",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT},
			})
			return err
		}, codes.NotFound},
		{"asymmetric key", func() error {
			_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
				Parent: "projects/p/locations/global/keyRings/ring", CryptoKeyId: "sign",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN},
			})
			return err
		}, codes.Unimplemented},
		{"encrypt with a missing key", func() error {
			_, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: "projects/p/locations/global/keyRings/ring/cryptoKeys/nope"})
			return err
		}, codes.NotFound},
		{"a malformed name", func() error {
			_, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: "keyRings/ring"})
			return err
		}, codes.InvalidArgument},
		{"an unimplemented method", func() error {
			_, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: key.GetPrimary().GetName()})
			return err
		}, codes.Unimplemented},
	}
	for _, tc := range cases {
		if got := status.Code(tc.call()); got != tc.want {
			t.Errorf("%s: code = %s, want %s", tc.name, got, tc.want)
		}
	}
}
