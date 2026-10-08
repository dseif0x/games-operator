package moonlight

import (
	"crypto"
	"crypto/aes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// hash is SHA-256 over the concatenation of its inputs.
func hash(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// Fingerprint identifies a client certificate: hex SHA-256 of the DER.
func Fingerprint(cert *x509.Certificate) string {
	return hex.EncodeToString(hash(cert.Raw))
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("random: %w", err)
	}
	return b, nil
}

func aesDecryptECB(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%block.BlockSize() != 0 {
		return nil, errors.New("ciphertext is not a multiple of the block size")
	}
	out := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += block.BlockSize() {
		block.Decrypt(out[i:i+block.BlockSize()], ciphertext[i:i+block.BlockSize()])
	}
	return out, nil
}

func aesEncryptECB(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(plaintext)%aes.BlockSize != 0 {
		return nil, errors.New("plaintext is not a multiple of the block size")
	}
	out := make([]byte, len(plaintext))
	for i := 0; i < len(plaintext); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plaintext[i:i+aes.BlockSize])
	}
	return out, nil
}

// verifySignature checks a SHA-256 signature made with the client's key.
func verifySignature(pub any, message, signature []byte) error {
	sum := sha256.Sum256(message)
	switch key := pub.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], signature)
	case *ecdsa.PublicKey:
		if ecdsa.VerifyASN1(key, sum[:], signature) {
			return nil
		}
		return errors.New("ecdsa signature verification failed")
	case ed25519.PublicKey:
		if ed25519.Verify(key, message, signature) {
			return nil
		}
		return errors.New("ed25519 signature verification failed")
	default:
		return errors.New("unsupported public key type")
	}
}

// LoadOrCreateCert loads the Moonlight server certificate from dir, creating
// a self-signed RSA certificate on first start. Clients pin this
// certificate when they pair, so the directory must persist.
func LoadOrCreateCert(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certPEM, err := os.ReadFile(certPath)
	keyPEM, kerr := os.ReadFile(keyPath)
	if err != nil || kerr != nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return tls.Certificate{}, err
		}
		certPEM, keyPEM, err = generateCert()
		if err != nil {
			return tls.Certificate{}, err
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			return tls.Certificate{}, fmt.Errorf("write key: %w", err)
		}
		if err := os.WriteFile(certPath, certPEM, 0o644); err != nil { //nolint:gosec // the certificate is public
			return tls.Certificate{}, fmt.Errorf("write cert: %w", err)
		}
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// generateCert makes a 20-year self-signed RSA-2048 certificate, which is
// what GameStream clients expect from a host.
func generateCert() (certPEM, keyPEM []byte, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "games-operator", Organization: []string{"games-operator"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM, nil
}

// certPEM renders the first certificate of the chain as PEM.
func certPEM(cert tls.Certificate) string {
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return string(out)
}

// certSignature returns the signature bytes of the server certificate.
func certSignature(cert tls.Certificate) ([]byte, error) {
	if cert.Leaf != nil {
		return cert.Leaf.Signature, nil
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	return parsed.Signature, nil
}
