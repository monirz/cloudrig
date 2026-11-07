package kms_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/services/kms"
	"github.com/monirz/cloudrig/store"
)

// flakyStore fails the next write of a key version while failVersion is set,
// and the next write of a key's record while failKey is set.
type flakyStore struct {
	store.Store
	failVersion atomic.Bool
	failKey     atomic.Bool
}

func (f *flakyStore) Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) {
	if strings.HasPrefix(key, "kms/v/") && f.failVersion.Swap(false) {
		return 0, errors.New("disk full")
	}
	if strings.HasPrefix(key, "kms/k/") && f.failKey.Swap(false) {
		return 0, errors.New("disk full")
	}
	return f.Store.Put(ctx, key, val, ifVersion)
}

// A create whose first version fails to store must leave nothing behind, so
// the retry succeeds and the key encrypts.
func TestCreateCryptoKeyUndoneWhenItsFirstVersionFails(t *testing.T) {
	kv := &flakyStore{Store: store.NewMemory()}
	svc := kms.New(kv, clock.NewFake(time.Unix(0, 0)))
	ctx := context.Background()

	ring, err := svc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/p/locations/global", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	req := &kmspb.CreateCryptoKeyRequest{
		Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT},
	}

	kv.failVersion.Store(true)
	if _, err := svc.CreateCryptoKey(ctx, req); status.Code(err) != codes.Internal {
		t.Fatalf("create with the version write failing: %v, want Internal", err)
	}
	if _, err := svc.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: ring.GetName() + "/cryptoKeys/k"}); status.Code(err) != codes.NotFound {
		t.Errorf("the failed key is still there: %v", err)
	}

	key, err := svc.CreateCryptoKey(ctx, req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := svc.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")}); err != nil {
		t.Errorf("encrypt with the retried key: %v", err)
	}
}

// A key made without a version gets its primary from its first version. If
// that version stores but the primary does not, the version must go, so a
// retry is version 1 again, becomes primary, and the key encrypts.
func TestFirstVersionUndoneWhenItsPrimaryFails(t *testing.T) {
	kv := &flakyStore{Store: store.NewMemory()}
	svc := kms.New(kv, clock.NewFake(time.Unix(0, 0)))
	ctx := context.Background()

	ring, err := svc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/p/locations/global", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
		Parent: ring.GetName(), CryptoKeyId: "k", SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT},
	})
	if err != nil {
		t.Fatal(err)
	}

	kv.failKey.Store(true)
	if _, err := svc.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()}); status.Code(err) != codes.Internal {
		t.Fatalf("version with the primary write failing: %v, want Internal", err)
	}
	v, err := svc.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !strings.HasSuffix(v.GetName(), "/cryptoKeyVersions/1") {
		t.Errorf("retry made %s, want version 1", v.GetName())
	}
	if _, err := svc.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")}); err != nil {
		t.Errorf("encrypt after the retry: %v", err)
	}
}
