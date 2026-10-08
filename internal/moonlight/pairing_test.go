package moonlight

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/dseif0x/games-operator/internal/store"
)

// TestPairingRoundTrip plays the Moonlight client side of the handshake
// against the manager, the way moonlight-common-c does it.
func TestPairingRoundTrip(t *testing.T) {
	serverCert, err := LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	m := NewPairingManager(serverCert, st.Pairings(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Client identity.
	clientKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "NVIDIA GameStream Client"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &clientKey.PublicKey, clientKey)
	clientCert, _ := x509.ParseCertificate(der)
	clientPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	pin := "4242"
	aesKey := hash(salt, []byte(pin))[:16]
	key := keyFor("client-1", "10.0.0.5")

	// Phase 1 blocks until the PIN arrives; feed it from the "UI".
	go func() {
		for i := 0; i < 100; i++ {
			if p := m.Pending(); len(p) == 1 {
				if err := m.SubmitPin(p[0].Secret, pin, "user-1"); err != nil {
					t.Error(err)
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("no pending pairing seen")
	}()
	r1 := m.phase1(context.Background(), key, "10.0.0.5", hex.EncodeToString(salt), hex.EncodeToString(clientPEM))
	if r1.Paired != 1 || r1.PlainCert == "" {
		t.Fatalf("phase 1: %+v", r1)
	}
	serverPEM, _ := hex.DecodeString(r1.PlainCert)
	block, _ := pem.Decode(serverPEM)
	serverX509, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	// Phase 2: send an encrypted random challenge, expect hash + server challenge.
	clientChallenge := make([]byte, 16)
	_, _ = rand.Read(clientChallenge)
	enc, _ := aesEncryptECB(clientChallenge, aesKey)
	r2 := m.phase2(key, hex.EncodeToString(enc))
	if r2.Paired != 1 {
		t.Fatalf("phase 2: %+v", r2)
	}
	resp, _ := hex.DecodeString(r2.ChallengeResponse)
	dec, err := aesDecryptECB(resp, aesKey)
	if err != nil {
		t.Fatal(err)
	}
	serverResponse, serverChallenge := dec[:32], dec[32:48]

	// Phase 3: hash(serverChallenge, clientCert.Signature, clientSecret), encrypted.
	clientSecret := make([]byte, 16)
	_, _ = rand.Read(clientSecret)
	clientHash := hash(serverChallenge, clientCert.Signature, clientSecret)
	enc3, _ := aesEncryptECB(clientHash, aesKey)
	r3 := m.phase3(key, hex.EncodeToString(enc3))
	if r3.Paired != 1 {
		t.Fatalf("phase 3: %+v", r3)
	}
	pairingSecret, _ := hex.DecodeString(r3.PairingSecret)
	serverSecret, serverSig := pairingSecret[:16], pairingSecret[16:]
	// The client verifies the server: its response must be hash(clientChallenge, serverCert.Signature, serverSecret) …
	if want := hash(clientChallenge, serverX509.Signature, serverSecret); string(want) != string(serverResponse) {
		t.Fatal("server response hash mismatch")
	}
	// … and the secret must be signed by the server key.
	sum := sha256.Sum256(serverSecret)
	if err := rsa.VerifyPKCS1v15(serverX509.PublicKey.(*rsa.PublicKey), crypto.SHA256, sum[:], serverSig); err != nil {
		t.Fatalf("server signature: %v", err)
	}

	// Phase 4: client secret signed by the client key.
	csum := sha256.Sum256(clientSecret)
	clientSig, _ := rsa.SignPKCS1v15(rand.Reader, clientKey, crypto.SHA256, csum[:])
	r4 := m.phase4(context.Background(), key, hex.EncodeToString(append(append([]byte{}, clientSecret...), clientSig...)))
	if r4.Paired != 1 {
		t.Fatalf("phase 4: %+v", r4)
	}
	p, err := st.Pairings().Get(context.Background(), Fingerprint(clientCert))
	if err != nil || p.UserID != "user-1" || p.Name != "NVIDIA GameStream Client" {
		t.Fatalf("pairing not stored: %v %+v", err, p)
	}
	if len(m.Pending()) != 0 {
		t.Fatal("pending list not cleared")
	}
}

func TestPairingWrongPin(t *testing.T) {
	serverCert, _ := LoadOrCreateCert(t.TempDir())
	m := NewPairingManager(serverCert, store.NewMemory().Pairings(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	clientKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &clientKey.PublicKey, clientKey)
	clientPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	salt := make([]byte, 16)
	key := keyFor("c", "ip")
	go func() {
		time.Sleep(20 * time.Millisecond)
		for _, p := range m.Pending() {
			_ = m.SubmitPin(p.Secret, "0000", "u")
		}
	}()
	if r := m.phase1(context.Background(), key, "ip", hex.EncodeToString(salt), hex.EncodeToString(clientPEM)); r.Paired != 1 {
		t.Fatalf("phase 1: %+v", r)
	}
	// Client derived its key from a different PIN: the challenge is garbage
	// to us, and the hash in phase 4 cannot match.
	wrongKey := hash(salt, []byte("1234"))[:16]
	chal := make([]byte, 16)
	enc, _ := aesEncryptECB(chal, wrongKey)
	r2 := m.phase2(key, hex.EncodeToString(enc))
	if r2.Paired != 1 {
		t.Fatalf("phase 2 should still answer: %+v", r2)
	}
	enc3, _ := aesEncryptECB(make([]byte, 32), wrongKey)
	if r3 := m.phase3(key, hex.EncodeToString(enc3)); r3.Paired != 1 {
		t.Fatalf("phase 3: %+v", r3)
	}
	r4 := m.phase4(context.Background(), key, hex.EncodeToString(make([]byte, 16+256)))
	if r4.Paired != 0 {
		t.Fatal("wrong PIN must not pair")
	}
}

func TestParseMode(t *testing.T) {
	w, h, f, err := parseMode("3840x2160x120")
	if err != nil || w != 3840 || h != 2160 || f != 120 {
		t.Fatal(w, h, f, err)
	}
	if _, _, _, err := parseMode("bad"); err == nil {
		t.Fatal("expected error")
	}
	if w, h, f, _ := parseMode(""); w != 1920 || h != 1080 || f != 60 {
		t.Fatal("default mode")
	}
}

var _ = tls.Certificate{}
