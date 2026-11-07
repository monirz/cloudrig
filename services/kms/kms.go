// Package kms is the Cloud KMS emulation: key rings, symmetric keys and their
// versions, and encrypt and decrypt.
//
// The cryptography is real, AES-256-GCM with a random key per version, so
// ciphertext is opaque, tampering is caught, and additional authenticated data
// must match. The key material is kept in the emulator's store in the clear:
// this is a stand-in for local runs, not somewhere to keep a secret.
//
// Only symmetric ENCRYPT_DECRYPT keys exist. Every other method answers
// Unimplemented rather than pretending.
package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// Service holds key rings, keys and versions.
type Service struct {
	kmspb.UnimplementedKeyManagementServiceServer

	kv  store.Store
	clk clock.Clock

	// mu serialises adding a version and moving the primary, which read a
	// key's state and write it back.
	mu sync.Mutex
}

// New wires a service.
func New(kv store.Store, clk clock.Clock) *Service {
	return &Service{kv: kv, clk: clk}
}

// Key layout. Versions are zero-padded so key order is version order.
const versionDigits = 12

func ringKey(name string) string { return "kms/r/" + name }
func keyKey(name string) string  { return "kms/k/" + name }
func versionKey(key string, n int64) string {
	return fmt.Sprintf("kms/v/%s/%0*d", key, versionDigits, n)
}
func versionPrefix(key string) string { return "kms/v/" + key + "/" }

// GCS limits KMS plaintext and additional data to 64 KiB each.
const maxData = 64 << 10

var (
	idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)
	marshal   = protojson.MarshalOptions{}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
	crcTable  = crc32.MakeTable(crc32.Castagnoli)
)

func crc32c(b []byte) int64 { return int64(crc32.Checksum(b, crcTable)) }

// keyRecord is a CryptoKey as stored: the proto, minus the primary version,
// which is filled in on read from primaryID so it can never go stale.
type keyRecord struct {
	Key       json.RawMessage `json:"key"`
	PrimaryID int64           `json:"primaryId"`
}

// versionRecord is a version as stored, with its key material.
type versionRecord struct {
	Version  json.RawMessage `json:"version"`
	Material []byte          `json:"material"`
}

// Names.

func parseLocation(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "locations" || parts[1] == "" || parts[3] == "" {
		return status.Errorf(codes.InvalidArgument,
			"invalid location %q; expected projects/{project}/locations/{location}", name)
	}
	return nil
}

func parseRing(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[4] != "keyRings" || parts[5] == "" {
		return status.Errorf(codes.InvalidArgument,
			"invalid key ring name %q; expected projects/{p}/locations/{l}/keyRings/{r}", name)
	}
	return parseLocation(strings.Join(parts[:4], "/"))
}

func parseKey(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 8 || parts[6] != "cryptoKeys" || parts[7] == "" {
		return status.Errorf(codes.InvalidArgument,
			"invalid crypto key name %q; expected projects/{p}/locations/{l}/keyRings/{r}/cryptoKeys/{k}", name)
	}
	return parseRing(strings.Join(parts[:6], "/"))
}

// parseVersion splits a version name into its key and number.
func parseVersion(name string) (string, int64, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 10 || parts[8] != "cryptoKeyVersions" {
		return "", 0, status.Errorf(codes.InvalidArgument,
			"invalid crypto key version name %q; expected .../cryptoKeys/{k}/cryptoKeyVersions/{n}", name)
	}
	n, err := strconv.ParseInt(parts[9], 10, 64)
	if err != nil || n < 1 {
		return "", 0, status.Errorf(codes.InvalidArgument, "invalid crypto key version %q", parts[9])
	}
	key := strings.Join(parts[:8], "/")
	return key, n, parseKey(key)
}

func validID(kind, id string) error {
	if !idPattern.MatchString(id) {
		return status.Errorf(codes.InvalidArgument, "invalid %s id %q: 1 to 63 of [a-zA-Z0-9_-]", kind, id)
	}
	return nil
}

// Key rings.

func (s *Service) CreateKeyRing(ctx context.Context, req *kmspb.CreateKeyRingRequest) (*kmspb.KeyRing, error) {
	if err := parseLocation(req.GetParent()); err != nil {
		return nil, err
	}
	if err := validID("key ring", req.GetKeyRingId()); err != nil {
		return nil, err
	}
	ring := &kmspb.KeyRing{
		Name:       req.GetParent() + "/keyRings/" + req.GetKeyRingId(),
		CreateTime: timestamppb.New(s.clk.Now()),
	}
	encoded, err := marshal.Marshal(ring)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding key ring: %v", err)
	}
	if _, err := s.kv.Put(ctx, ringKey(ring.Name), encoded, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return nil, status.Errorf(codes.AlreadyExists, "KeyRing %s already exists.", ring.Name)
		}
		return nil, status.Errorf(codes.Internal, "storing key ring: %v", err)
	}
	return ring, nil
}

func (s *Service) GetKeyRing(ctx context.Context, req *kmspb.GetKeyRingRequest) (*kmspb.KeyRing, error) {
	if err := parseRing(req.GetName()); err != nil {
		return nil, err
	}
	return s.ring(ctx, req.GetName())
}

func (s *Service) ring(ctx context.Context, name string) (*kmspb.KeyRing, error) {
	raw, _, err := s.kv.Get(ctx, ringKey(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "KeyRing %s not found.", name)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading key ring: %v", err)
	}
	ring := &kmspb.KeyRing{}
	if err := unmarshal.Unmarshal(raw, ring); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding key ring: %v", err)
	}
	return ring, nil
}

func (s *Service) ListKeyRings(ctx context.Context, req *kmspb.ListKeyRingsRequest) (*kmspb.ListKeyRingsResponse, error) {
	if err := parseLocation(req.GetParent()); err != nil {
		return nil, err
	}
	if err := noPaging(req.GetPageToken(), req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	entries, _, err := s.kv.List(ctx, ringKey(req.GetParent()+"/keyRings/"), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing key rings: %v", err)
	}
	out := &kmspb.ListKeyRingsResponse{}
	for _, e := range entries {
		ring := &kmspb.KeyRing{}
		if err := unmarshal.Unmarshal(e.Val, ring); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding key ring: %v", err)
		}
		out.KeyRings = append(out.KeyRings, ring)
	}
	out.TotalSize = int32(len(out.KeyRings))
	return out, nil
}

// noPaging refuses what the lists here do not implement. Every list answers
// in one page, so a client never needs a token, but one sent is not ours.
func noPaging(token, filter, orderBy string) error {
	switch {
	case token != "":
		return status.Error(codes.InvalidArgument, "invalid page token")
	case filter != "", orderBy != "":
		return status.Error(codes.Unimplemented, "filter and orderBy are not supported by cloudrig")
	}
	return nil
}

// Crypto keys.

func (s *Service) CreateCryptoKey(ctx context.Context, req *kmspb.CreateCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	if err := parseRing(req.GetParent()); err != nil {
		return nil, err
	}
	if err := validID("crypto key", req.GetCryptoKeyId()); err != nil {
		return nil, err
	}
	if _, err := s.ring(ctx, req.GetParent()); err != nil {
		return nil, err
	}

	key := req.GetCryptoKey()
	if key == nil {
		key = &kmspb.CryptoKey{}
	}
	if key.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return nil, status.Errorf(codes.Unimplemented,
			"cloudrig supports only ENCRYPT_DECRYPT keys, not %s", key.GetPurpose())
	}
	tmpl := key.GetVersionTemplate()
	if tmpl == nil {
		tmpl = &kmspb.CryptoKeyVersionTemplate{}
	}
	if a := tmpl.GetAlgorithm(); a != kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED &&
		a != kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION {
		return nil, status.Errorf(codes.Unimplemented, "cloudrig supports only GOOGLE_SYMMETRIC_ENCRYPTION, not %s", a)
	}
	tmpl.Algorithm = kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION
	if tmpl.GetProtectionLevel() == kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED {
		tmpl.ProtectionLevel = kmspb.ProtectionLevel_SOFTWARE
	}

	key.Name = req.GetParent() + "/cryptoKeys/" + req.GetCryptoKeyId()
	key.VersionTemplate = tmpl
	key.CreateTime = timestamppb.New(s.clk.Now())
	key.Primary = nil

	encoded, err := marshal.Marshal(key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding crypto key: %v", err)
	}
	record, _ := json.Marshal(keyRecord{Key: encoded})

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.kv.Put(ctx, keyKey(key.Name), record, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return nil, status.Errorf(codes.AlreadyExists, "CryptoKey %s already exists.", key.Name)
		}
		return nil, status.Errorf(codes.Internal, "storing crypto key: %v", err)
	}
	// A new symmetric key starts with version 1 as its primary, unless the
	// caller asked to add versions themselves.
	if !req.GetSkipInitialVersionCreation() {
		if _, err := s.addVersion(ctx, key.Name, true); err != nil {
			return nil, err
		}
	}
	return s.cryptoKey(ctx, key.Name)
}

func (s *Service) GetCryptoKey(ctx context.Context, req *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error) {
	if err := parseKey(req.GetName()); err != nil {
		return nil, err
	}
	return s.cryptoKey(ctx, req.GetName())
}

func (s *Service) keyRecord(ctx context.Context, name string) (keyRecord, uint64, error) {
	raw, version, err := s.kv.Get(ctx, keyKey(name))
	if errors.Is(err, store.ErrNotFound) {
		return keyRecord{}, 0, status.Errorf(codes.NotFound, "CryptoKey %s not found.", name)
	}
	if err != nil {
		return keyRecord{}, 0, status.Errorf(codes.Internal, "reading crypto key: %v", err)
	}
	var rec keyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return keyRecord{}, 0, status.Errorf(codes.Internal, "decoding crypto key: %v", err)
	}
	return rec, version, nil
}

func (s *Service) cryptoKey(ctx context.Context, name string) (*kmspb.CryptoKey, error) {
	rec, _, err := s.keyRecord(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.decodeKey(ctx, rec)
}

func (s *Service) decodeKey(ctx context.Context, rec keyRecord) (*kmspb.CryptoKey, error) {
	key := &kmspb.CryptoKey{}
	if err := unmarshal.Unmarshal(rec.Key, key); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding crypto key: %v", err)
	}
	if rec.PrimaryID != 0 {
		v, _, err := s.version(ctx, key.Name, rec.PrimaryID)
		if err != nil {
			return nil, err
		}
		key.Primary = v
	}
	return key, nil
}

func (s *Service) ListCryptoKeys(ctx context.Context, req *kmspb.ListCryptoKeysRequest) (*kmspb.ListCryptoKeysResponse, error) {
	if err := parseRing(req.GetParent()); err != nil {
		return nil, err
	}
	if err := noPaging(req.GetPageToken(), req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	entries, _, err := s.kv.List(ctx, keyKey(req.GetParent()+"/cryptoKeys/"), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing crypto keys: %v", err)
	}
	out := &kmspb.ListCryptoKeysResponse{}
	for _, e := range entries {
		var rec keyRecord
		if err := json.Unmarshal(e.Val, &rec); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding crypto key: %v", err)
		}
		key, err := s.decodeKey(ctx, rec)
		if err != nil {
			return nil, err
		}
		out.CryptoKeys = append(out.CryptoKeys, key)
	}
	out.TotalSize = int32(len(out.CryptoKeys))
	return out, nil
}

// Versions.

func (s *Service) CreateCryptoKeyVersion(ctx context.Context, req *kmspb.CreateCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	if err := parseKey(req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.keyRecord(ctx, req.GetParent()); err != nil {
		return nil, err
	}
	// As in KMS, a new version of a symmetric key is not made primary; that
	// takes UpdateCryptoKeyPrimaryVersion. The first one is the exception.
	return s.addVersion(ctx, req.GetParent(), false)
}

// addVersion creates the next version with fresh key material. Callers hold
// s.mu.
func (s *Service) addVersion(ctx context.Context, keyName string, makePrimary bool) (*kmspb.CryptoKeyVersion, error) {
	entries, _, err := s.kv.List(ctx, versionPrefix(keyName), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing versions: %v", err)
	}
	n := int64(len(entries)) + 1

	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		return nil, status.Errorf(codes.Internal, "generating key material: %v", err)
	}
	now := timestamppb.New(s.clk.Now())
	v := &kmspb.CryptoKeyVersion{
		Name:             fmt.Sprintf("%s/cryptoKeyVersions/%d", keyName, n),
		State:            kmspb.CryptoKeyVersion_ENABLED,
		ProtectionLevel:  kmspb.ProtectionLevel_SOFTWARE,
		Algorithm:        kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
		CreateTime:       now,
		GenerateTime:     now,
		ReimportEligible: false,
	}
	encoded, err := marshal.Marshal(v)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding version: %v", err)
	}
	record, _ := json.Marshal(versionRecord{Version: encoded, Material: material})
	if _, err := s.kv.Put(ctx, versionKey(keyName, n), record, 0); err != nil {
		return nil, status.Errorf(codes.Internal, "storing version: %v", err)
	}

	rec, stored, err := s.keyRecord(ctx, keyName)
	if err != nil {
		return nil, err
	}
	if makePrimary || rec.PrimaryID == 0 && n == 1 {
		if err := s.setPrimary(ctx, keyName, rec, stored, n); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func (s *Service) setPrimary(ctx context.Context, keyName string, rec keyRecord, stored uint64, n int64) error {
	rec.PrimaryID = n
	encoded, _ := json.Marshal(rec)
	if _, err := s.kv.Put(ctx, keyKey(keyName), encoded, stored); err != nil {
		return status.Errorf(codes.Internal, "storing crypto key: %v", err)
	}
	return nil
}

func (s *Service) version(ctx context.Context, keyName string, n int64) (*kmspb.CryptoKeyVersion, []byte, error) {
	raw, _, err := s.kv.Get(ctx, versionKey(keyName, n))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, status.Errorf(codes.NotFound, "CryptoKeyVersion %s/cryptoKeyVersions/%d not found.", keyName, n)
	}
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "reading version: %v", err)
	}
	var rec versionRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, nil, status.Errorf(codes.Internal, "decoding version: %v", err)
	}
	v := &kmspb.CryptoKeyVersion{}
	if err := unmarshal.Unmarshal(rec.Version, v); err != nil {
		return nil, nil, status.Errorf(codes.Internal, "decoding version: %v", err)
	}
	return v, rec.Material, nil
}

func (s *Service) GetCryptoKeyVersion(ctx context.Context, req *kmspb.GetCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	key, n, err := parseVersion(req.GetName())
	if err != nil {
		return nil, err
	}
	v, _, err := s.version(ctx, key, n)
	return v, err
}

func (s *Service) ListCryptoKeyVersions(ctx context.Context, req *kmspb.ListCryptoKeyVersionsRequest) (*kmspb.ListCryptoKeyVersionsResponse, error) {
	if err := parseKey(req.GetParent()); err != nil {
		return nil, err
	}
	if err := noPaging(req.GetPageToken(), req.GetFilter(), req.GetOrderBy()); err != nil {
		return nil, err
	}
	if _, _, err := s.keyRecord(ctx, req.GetParent()); err != nil {
		return nil, err
	}
	entries, _, err := s.kv.List(ctx, versionPrefix(req.GetParent()), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing versions: %v", err)
	}
	out := &kmspb.ListCryptoKeyVersionsResponse{}
	for _, e := range entries {
		var rec versionRecord
		if err := json.Unmarshal(e.Val, &rec); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding version: %v", err)
		}
		v := &kmspb.CryptoKeyVersion{}
		if err := unmarshal.Unmarshal(rec.Version, v); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding version: %v", err)
		}
		out.CryptoKeyVersions = append(out.CryptoKeyVersions, v)
	}
	out.TotalSize = int32(len(out.CryptoKeyVersions))
	return out, nil
}

func (s *Service) UpdateCryptoKeyPrimaryVersion(ctx context.Context, req *kmspb.UpdateCryptoKeyPrimaryVersionRequest) (*kmspb.CryptoKey, error) {
	if err := parseKey(req.GetName()); err != nil {
		return nil, err
	}
	n, err := strconv.ParseInt(req.GetCryptoKeyVersionId(), 10, 64)
	if err != nil || n < 1 {
		return nil, status.Errorf(codes.InvalidArgument, "invalid crypto key version id %q", req.GetCryptoKeyVersionId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, stored, err := s.keyRecord(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if _, _, err := s.version(ctx, req.GetName(), n); err != nil {
		return nil, err
	}
	if err := s.setPrimary(ctx, req.GetName(), rec, stored, n); err != nil {
		return nil, err
	}
	return s.cryptoKey(ctx, req.GetName())
}

// Encrypt and decrypt.
//
// Ciphertext is the version number, 4 bytes big-endian, then the GCM nonce,
// then the sealed bytes. The number is how Decrypt, which names only the key,
// finds the version that encrypted it, as KMS does.

const nonceSize = 12

func seal(material []byte, n int64, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(material)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4+nonceSize, 4+nonceSize+len(plaintext)+gcm.Overhead())
	binary.BigEndian.PutUint32(out, uint32(n))
	if _, err := rand.Read(out[4:]); err != nil {
		return nil, err
	}
	return gcm.Seal(out, out[4:4+nonceSize], plaintext, aad), nil
}

func newGCM(material []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(material)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Service) Encrypt(ctx context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	keyName, n := req.GetName(), int64(0)
	if strings.Contains(keyName, "/cryptoKeyVersions/") {
		var err error
		if keyName, n, err = parseVersion(keyName); err != nil {
			return nil, err
		}
	} else if err := parseKey(keyName); err != nil {
		return nil, err
	}
	if len(req.GetPlaintext()) > maxData {
		return nil, status.Errorf(codes.InvalidArgument, "plaintext is larger than %d bytes", maxData)
	}
	if len(req.GetAdditionalAuthenticatedData()) > maxData {
		return nil, status.Errorf(codes.InvalidArgument, "additional authenticated data is larger than %d bytes", maxData)
	}
	if err := checkCRC("plaintext", req.GetPlaintext(), req.GetPlaintextCrc32C()); err != nil {
		return nil, err
	}
	if err := checkCRC("additional_authenticated_data", req.GetAdditionalAuthenticatedData(),
		req.GetAdditionalAuthenticatedDataCrc32C()); err != nil {
		return nil, err
	}

	rec, _, err := s.keyRecord(ctx, keyName)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		if rec.PrimaryID == 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "CryptoKey %s has no primary version.", keyName)
		}
		n = rec.PrimaryID
	}
	v, material, err := s.version(ctx, keyName, n)
	if err != nil {
		return nil, err
	}
	if v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is not enabled.", v.GetName())
	}

	ciphertext, err := seal(material, n, req.GetPlaintext(), req.GetAdditionalAuthenticatedData())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encrypting: %v", err)
	}
	return &kmspb.EncryptResponse{
		Name:                    v.GetName(),
		Ciphertext:              ciphertext,
		CiphertextCrc32C:        wrapperspb.Int64(crc32c(ciphertext)),
		VerifiedPlaintextCrc32C: req.GetPlaintextCrc32C() != nil,
		VerifiedAdditionalAuthenticatedDataCrc32C: req.GetAdditionalAuthenticatedDataCrc32C() != nil,
		ProtectionLevel: v.GetProtectionLevel(),
	}, nil
}

func (s *Service) Decrypt(ctx context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	if err := parseKey(req.GetName()); err != nil {
		return nil, err
	}
	if err := checkCRC("ciphertext", req.GetCiphertext(), req.GetCiphertextCrc32C()); err != nil {
		return nil, err
	}
	if err := checkCRC("additional_authenticated_data", req.GetAdditionalAuthenticatedData(),
		req.GetAdditionalAuthenticatedDataCrc32C()); err != nil {
		return nil, err
	}
	rec, _, err := s.keyRecord(ctx, req.GetName())
	if err != nil {
		return nil, err
	}

	// KMS answers every ciphertext it cannot open the same way, so a caller
	// learns nothing from which check failed.
	invalid := status.Error(codes.InvalidArgument, "Decryption failed: the ciphertext is invalid.")
	ct := req.GetCiphertext()
	if len(ct) < 4+nonceSize {
		return nil, invalid
	}
	n := int64(binary.BigEndian.Uint32(ct))
	v, material, err := s.version(ctx, req.GetName(), n)
	if status.Code(err) == codes.NotFound {
		return nil, invalid
	}
	if err != nil {
		return nil, err
	}
	if v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is not enabled.", v.GetName())
	}
	gcm, err := newGCM(material)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decrypting: %v", err)
	}
	plaintext, err := gcm.Open(nil, ct[4:4+nonceSize], ct[4+nonceSize:], req.GetAdditionalAuthenticatedData())
	if err != nil {
		return nil, invalid
	}
	return &kmspb.DecryptResponse{
		Plaintext:       plaintext,
		PlaintextCrc32C: wrapperspb.Int64(crc32c(plaintext)),
		UsedPrimary:     n == rec.PrimaryID,
		ProtectionLevel: v.GetProtectionLevel(),
	}, nil
}

// checkCRC verifies a checksum the client sent with a field, as KMS does to
// catch corruption in transit.
func checkCRC(field string, data []byte, sent *wrapperspb.Int64Value) error {
	if sent != nil && sent.GetValue() != crc32c(data) {
		return status.Errorf(codes.InvalidArgument,
			"The checksum in field %s_crc32c did not match the data in field %s.", field, field)
	}
	return nil
}
