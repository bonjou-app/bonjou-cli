package network

import (
	"bytes"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// vectorPath holds the canonical known-answer vectors. The browser repository
// keeps a copy pinned to a reviewed CLI commit, so each repository can test
// independently without relying on a sibling checkout.
//
// Regenerate with:
//
//	BONJOU_WRITE_VECTORS=1 go test ./internal/network -run TestProtocolV2Vectors
//
// A failure here means the wire format changed. That is not automatically
// wrong, but it does mean the browser client must change with it and that
// deployed peers on the old format will no longer interoperate.
const vectorPath = "testdata/protocol-v2.json"

// largeStreamLen spans more than one 64 KiB AEAD chunk so the vectors
// exercise counter increment and multi-frame output, which is where an
// independent implementation is most likely to diverge.
const largeStreamLen = streamChunkPlainBytes + 4096

type protocolVectors struct {
	Note    string `json:"note"`
	Version int    `json:"version"`
	AAD     string `json:"aad"`

	X25519 x25519Vector `json:"x25519"`
	HKDF   []hkdfVector `json:"hkdf_expand"`

	Envelope envelopeVector `json:"envelope"`

	StreamSmall streamVector      `json:"stream_small"`
	StreamLarge streamLargeVector `json:"stream_large"`

	ChunkNonces []chunkNonceVector `json:"chunk_nonces"`
}

type x25519Vector struct {
	Note            string `json:"note"`
	AlicePrivateHex string `json:"alice_private_hex"`
	AlicePublicHex  string `json:"alice_public_hex"`
	BobPrivateHex   string `json:"bob_private_hex"`
	BobPublicHex    string `json:"bob_public_hex"`
	RawECDHHex      string `json:"raw_ecdh_hex"`
	SharedHex       string `json:"shared_hex"`
}

// hkdfVector pins one derived key. InfoHex is authoritative: the stream
// label concatenates the raw streamID bytes, which are not valid UTF-8 in
// general, so the human-readable Info field is lossy and exists only for
// eyeballing.
type hkdfVector struct {
	Info    string `json:"info"`
	InfoHex string `json:"info_hex"`
	KeyHex  string `json:"key_hex"`
}

type envelopeVector struct {
	PlaintextJSON string `json:"plaintext_json"`
	KeyHex        string `json:"key_hex"`
	NonceHex      string `json:"nonce_hex"`
	SealedJSON    string `json:"sealed_json"`
	CiphertextB64 string `json:"ciphertext_b64"`
}

type streamVector struct {
	StreamIDHex   string `json:"stream_id_hex"`
	StreamKeyHex  string `json:"stream_key_hex"`
	PlaintextHex  string `json:"plaintext_hex"`
	FramedHex     string `json:"framed_hex"`
	FramedSHA256  string `json:"framed_sha256"`
	PlaintextSize int    `json:"plaintext_size"`
}

// streamLargeVector avoids embedding hundreds of kilobytes of hex by
// describing its plaintext as a reproducible pattern and pinning only the
// digest of the framed output.
type streamLargeVector struct {
	StreamIDHex     string `json:"stream_id_hex"`
	StreamKeyHex    string `json:"stream_key_hex"`
	PlaintextRecipe string `json:"plaintext_recipe"`
	PlaintextSize   int    `json:"plaintext_size"`
	FramedSize      int    `json:"framed_size"`
	FramedSHA256    string `json:"framed_sha256"`
}

type chunkNonceVector struct {
	Counter  uint64 `json:"counter"`
	NonceHex string `json:"nonce_hex"`
}

// patternBytes is the reproducible plaintext used by the large stream
// vector. The browser test generates the same sequence.
func patternBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i % 251)
	}
	return out
}

func TestProtocolV2Vectors(t *testing.T) {
	got := buildVectors(t)

	if os.Getenv("BONJOU_WRITE_VECTORS") == "1" {
		writeVectors(t, got)
		t.Logf("wrote %s", vectorPath)
		return
	}

	raw, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatalf("read vectors: %v\n\nRegenerate with:\n  BONJOU_WRITE_VECTORS=1 go test ./internal/network -run TestProtocolV2Vectors", err)
	}
	var want protocolVectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal generated vectors: %v", err)
	}
	wantJSON, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("marshal stored vectors: %v", err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("protocol v2 vectors changed.\n\nThe wire format no longer matches the vectors the browser client is built against.\nIf the change is intended, update the web implementation and regenerate:\n  BONJOU_WRITE_VECTORS=1 go test ./internal/network -run TestProtocolV2Vectors\n\ngot:  %s\n\nwant: %s", gotJSON, wantJSON)
	}
}

func buildVectors(t *testing.T) *protocolVectors {
	t.Helper()

	// Public X25519 examples from RFC 7748 section 6.1. These are test
	// values only and are never used by the application.
	alicePriv := mustHex(t, "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	bobPriv := mustHex(t, "5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb")

	aliceKey, err := ecdh.X25519().NewPrivateKey(alicePriv)
	if err != nil {
		t.Fatalf("alice private key: %v", err)
	}
	bobKey, err := ecdh.X25519().NewPrivateKey(bobPriv)
	if err != nil {
		t.Fatalf("bob private key: %v", err)
	}
	rawECDH, err := aliceKey.ECDH(bobKey.PublicKey())
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	// crypto.go hashes the raw ECDH output before it is used as key
	// material; the browser must do the same.
	sharedArr := sha256.Sum256(rawECDH)
	shared := sharedArr[:]

	// Cross-check: the reverse direction must agree, or the vector is
	// meaningless.
	reverseECDH, err := bobKey.ECDH(aliceKey.PublicKey())
	if err != nil {
		t.Fatalf("reverse ecdh: %v", err)
	}
	if !bytes.Equal(rawECDH, reverseECDH) {
		t.Fatal("ECDH is not symmetric; vectors would be invalid")
	}

	envKey, err := deriveEnvelopeKey(shared)
	if err != nil {
		t.Fatalf("deriveEnvelopeKey: %v", err)
	}
	macKey, err := deriveMACKey(shared)
	if err != nil {
		t.Fatalf("deriveMACKey: %v", err)
	}

	streamID := mustHex(t, "000102030405060708090a0b0c0d0e0f")
	streamKey, err := deriveStreamKey(shared, streamID)
	if err != nil {
		t.Fatalf("deriveStreamKey: %v", err)
	}

	// Envelope vector. This mirrors sealEnvelope exactly but pins the
	// nonce, which sealEnvelope draws at random.
	env := &envelope{
		Kind:      kindFileOffer,
		From:      "ada",
		FromIP:    "192.0.2.10",
		To:        "bob",
		Name:      "report.pdf",
		Size:      1048576,
		Timestamp: 1767225600,
		Checksum:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		StreamID:  hex.EncodeToString(streamID),
	}
	plain, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	aead, err := newGCM(envKey)
	if err != nil {
		t.Fatalf("newGCM: %v", err)
	}
	envNonce := mustHex(t, "0b0a09080706050403020100")
	aad := []byte(fmt.Sprintf("bonjou.v%d", sealedEnvelopeVersion))
	ciphertext := aead.Seal(nil, envNonce, plain, aad)
	sealed := sealedEnvelope{
		Version: sealedEnvelopeVersion,
		Nonce:   hex.EncodeToString(envNonce),
		Payload: base64.StdEncoding.EncodeToString(ciphertext),
	}
	sealedJSON, err := json.Marshal(sealed)
	if err != nil {
		t.Fatalf("marshal sealed envelope: %v", err)
	}

	// The vector must round-trip through the real production decoder,
	// otherwise it only proves the test agrees with itself.
	reopened, _, err := openEnvelope(sealedJSON, shared)
	if err != nil {
		t.Fatalf("openEnvelope on generated vector: %v", err)
	}
	if reopened.Name != env.Name || reopened.Size != env.Size {
		t.Fatalf("round-trip mismatch: %+v", reopened)
	}

	smallPlain := []byte("bonjou relays bytes it cannot read")
	smallFramed := frameStream(t, streamKey, smallPlain)
	largePlain := patternBytes(largeStreamLen)
	largeStreamID := mustHex(t, "0f0e0d0c0b0a09080706050403020100")
	largeStreamKey, err := deriveStreamKey(shared, largeStreamID)
	if err != nil {
		t.Fatalf("deriveStreamKey large: %v", err)
	}
	largeFramed := frameStream(t, largeStreamKey, largePlain)

	nonces := make([]chunkNonceVector, 0, 3)
	for _, counter := range []uint64{0, 1, 255} {
		nonces = append(nonces, chunkNonceVector{
			Counter:  counter,
			NonceHex: hex.EncodeToString(chunkNonce(streamDirSend, counter)),
		})
	}

	return &protocolVectors{
		Note: "Public test data for Bonjou protocol v2; never use these keys in production. " +
			"X25519 examples come from RFC 7748 section 6.1. Generated by " +
			"internal/network/vectors_test.go for Go/browser compatibility tests. Do not hand-edit.",
		Version: sealedEnvelopeVersion,
		AAD:     string(aad),
		X25519: x25519Vector{
			Note:            "shared_hex is SHA-256 of the raw ECDH output, matching crypto.go",
			AlicePrivateHex: hex.EncodeToString(alicePriv),
			AlicePublicHex:  hex.EncodeToString(aliceKey.PublicKey().Bytes()),
			BobPrivateHex:   hex.EncodeToString(bobPriv),
			BobPublicHex:    hex.EncodeToString(bobKey.PublicKey().Bytes()),
			RawECDHHex:      hex.EncodeToString(rawECDH),
			SharedHex:       hex.EncodeToString(shared),
		},
		HKDF: []hkdfVector{
			{
				Info:    envelopeInfo,
				InfoHex: hex.EncodeToString([]byte(envelopeInfo)),
				KeyHex:  hex.EncodeToString(envKey),
			},
			{
				Info:    macInfo,
				InfoHex: hex.EncodeToString([]byte(macInfo)),
				KeyHex:  hex.EncodeToString(macKey),
			},
			{
				Info:    streamInfoPrefix + "<raw streamID bytes>",
				InfoHex: hex.EncodeToString([]byte(streamInfoPrefix + string(streamID))),
				KeyHex:  hex.EncodeToString(streamKey),
			},
		},
		Envelope: envelopeVector{
			PlaintextJSON: string(plain),
			KeyHex:        hex.EncodeToString(envKey),
			NonceHex:      hex.EncodeToString(envNonce),
			SealedJSON:    string(sealedJSON),
			CiphertextB64: sealed.Payload,
		},
		StreamSmall: streamVector{
			StreamIDHex:   hex.EncodeToString(streamID),
			StreamKeyHex:  hex.EncodeToString(streamKey),
			PlaintextHex:  hex.EncodeToString(smallPlain),
			FramedHex:     hex.EncodeToString(smallFramed),
			FramedSHA256:  sha256Hex(smallFramed),
			PlaintextSize: len(smallPlain),
		},
		StreamLarge: streamLargeVector{
			StreamIDHex:     hex.EncodeToString(largeStreamID),
			StreamKeyHex:    hex.EncodeToString(largeStreamKey),
			PlaintextRecipe: "byte[i] = i % 251",
			PlaintextSize:   len(largePlain),
			FramedSize:      len(largeFramed),
			FramedSHA256:    sha256Hex(largeFramed),
		},
		ChunkNonces: nonces,
	}
}

// frameStream runs plaintext through the production chunked-AEAD writer and
// verifies the production reader accepts the result.
func frameStream(t *testing.T, streamKey, plaintext []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := newChunkedFrameWriter(&buf, streamKey)
	if err != nil {
		t.Fatalf("newChunkedFrameWriter: %v", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		t.Fatalf("write stream: %v", err)
	}

	reader, err := newChunkedFrameReader(bytes.NewReader(buf.Bytes()), streamKey)
	if err != nil {
		t.Fatalf("newChunkedFrameReader: %v", err)
	}
	back := make([]byte, len(plaintext))
	if _, err := readFull(reader, back); err != nil {
		t.Fatalf("read stream back: %v", err)
	}
	if !bytes.Equal(back, plaintext) {
		t.Fatal("stream did not round-trip through the production reader")
	}
	return buf.Bytes()
}

// readFull fills buf, tolerating the short reads chunkedFrameReader
// produces at chunk boundaries.
func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, fmt.Errorf("no progress at %d bytes", total)
		}
	}
	return total, nil
}

func writeVectors(t *testing.T, v *protocolVectors) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(vectorPath), 0o755); err != nil {
		t.Fatalf("create vector dir: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal vectors: %v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(vectorPath, out, 0o644); err != nil {
		t.Fatalf("write vectors: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}
