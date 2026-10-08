package moonlight

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dseif0x/games-operator/internal/store"
)

// PinTimeout is how long phase 1 waits for someone to enter the PIN.
const PinTimeout = 2 * time.Minute

// ErrNoPending is returned when a PIN arrives for an unknown pairing.
var ErrNoPending = errors.New("no pending pairing request for that secret")

// PendingPair is a pairing waiting for its PIN. The hub UI lists these so
// the logged-in user can type the PIN Moonlight shows.
type PendingPair struct {
	Secret    string    `json:"secret"`
	ClientIP  string    `json:"client_ip"`
	StartedAt time.Time `json:"started_at"`
}

type pinSubmission struct {
	pin    string
	userID string
	via    string
}

type pairState struct {
	clientCert      *x509.Certificate
	aesKey          []byte
	phase           string
	serverSecret    []byte
	serverChallenge []byte
	clientHash      []byte
	userID          string
	via             string
}

type pending struct {
	PendingPair
	ch chan pinSubmission
}

// PairingManager runs the four-phase handshake and persists the result.
type PairingManager struct {
	cert     tls.Certificate
	pairings store.Pairings
	log      *slog.Logger

	mu      sync.Mutex
	states  map[string]*pairState // key: clientID@clientIP
	pending map[string]*pending   // key: secret
}

// NewPairingManager returns a manager serving cert to clients.
func NewPairingManager(cert tls.Certificate, pairings store.Pairings, log *slog.Logger) *PairingManager {
	return &PairingManager{cert: cert, pairings: pairings, log: log, states: map[string]*pairState{}, pending: map[string]*pending{}}
}

// Pending lists pairings waiting for a PIN.
func (m *PairingManager) Pending() []PendingPair {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PendingPair, 0, len(m.pending))
	for _, p := range m.pending {
		out = append(out, p.PendingPair)
	}
	return out
}

// SubmitPin delivers the PIN a user typed for the pairing identified by
// secret and binds the resulting pairing to that user.
// via records how the PIN arrived ("" from the Pair page,
// store.PairingViaBrowser from the embedded moonlight-web).
func (m *PairingManager) SubmitPin(secret, pin, userID, via string) error {
	m.mu.Lock()
	p, ok := m.pending[secret]
	m.mu.Unlock()
	if !ok {
		return ErrNoPending
	}
	select {
	case p.ch <- pinSubmission{pin: pin, userID: userID, via: via}:
		return nil
	default:
		return errors.New("a PIN was already submitted for this pairing")
	}
}

func failPair(log *slog.Logger, msg string) PairingResponse {
	log.Warn("pairing failed", "reason", msg)
	return PairingResponse{Paired: 0, Response: Response{StatusCode: 400, StatusMessage: msg}}
}

func (m *PairingManager) getState(key string) (*pairState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[key]
	return s, ok
}

func (m *PairingManager) setState(key string, s *pairState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[key] = s
}

func (m *PairingManager) deleteState(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, key)
}

// phase1: Moonlight sends a salt and its certificate; we wait for the PIN,
// derive the AES key from salt+PIN and answer with our certificate.
func (m *PairingManager) phase1(ctx context.Context, key, clientIP, salt, clientCertHex string) PairingResponse {
	if _, found := m.getState(key); found {
		m.deleteState(key)
		return failPair(m.log, "out of order pair request (phase 1)")
	}
	certData, err := hex.DecodeString(clientCertHex)
	if err != nil {
		return failPair(m.log, "bad client certificate encoding")
	}
	block, _ := pem.Decode(certData)
	if block == nil {
		return failPair(m.log, "client certificate is not PEM")
	}
	clientCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return failPair(m.log, "bad client certificate: "+err.Error())
	}
	saltData, err := hex.DecodeString(salt)
	if err != nil || len(saltData) < 16 {
		return failPair(m.log, "bad salt")
	}
	secretBytes, err := randomBytes(8)
	if err != nil {
		return failPair(m.log, err.Error())
	}
	secret := hex.EncodeToString(secretBytes)
	p := &pending{PendingPair: PendingPair{Secret: secret, ClientIP: clientIP, StartedAt: time.Now()}, ch: make(chan pinSubmission, 1)}
	m.mu.Lock()
	m.pending[secret] = p
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pending, secret)
		m.mu.Unlock()
	}()
	m.log.Info("pairing started, waiting for PIN", "client_ip", clientIP, "secret", secret)

	var sub pinSubmission
	select {
	case sub = <-p.ch:
	case <-time.After(PinTimeout):
		return failPair(m.log, "no PIN entered within "+PinTimeout.String())
	case <-ctx.Done():
		return failPair(m.log, "client went away")
	}
	m.setState(key, &pairState{
		clientCert: clientCert,
		phase:      "GETSERVERCERT",
		userID:     sub.userID,
		via:        sub.via,
		aesKey:     hash(saltData[:16], []byte(sub.pin))[:16],
	})
	return PairingResponse{
		Paired:    1,
		PlainCert: hex.EncodeToString([]byte(certPEM(m.cert))),
		Response:  Response{StatusCode: 200},
	}
}

// phase2: decrypt the client challenge and answer with an encrypted hash
// of it, our certificate signature and a fresh server secret.
func (m *PairingManager) phase2(key, clientChallenge string) PairingResponse {
	challenge, err := hex.DecodeString(clientChallenge)
	if err != nil {
		return failPair(m.log, "bad client challenge")
	}
	st, ok := m.getState(key)
	if !ok {
		return failPair(m.log, "pairing session not found")
	}
	if st.phase != "GETSERVERCERT" {
		return failPair(m.log, "out of order pair request (phase 2)")
	}
	serverSecret, err := randomBytes(16)
	if err != nil {
		return failPair(m.log, err.Error())
	}
	serverChallenge, err := randomBytes(16)
	if err != nil {
		return failPair(m.log, err.Error())
	}
	sig, err := certSignature(m.cert)
	if err != nil {
		return failPair(m.log, "server certificate: "+err.Error())
	}
	decrypted, err := aesDecryptECB(challenge, st.aesKey)
	if err != nil {
		return failPair(m.log, "decrypt client challenge: "+err.Error())
	}
	plain := append(hash(decrypted, sig, serverSecret), serverChallenge...)
	encrypted, err := aesEncryptECB(plain, st.aesKey)
	if err != nil {
		return failPair(m.log, "encrypt challenge response: "+err.Error())
	}
	st.phase = "CLIENTCHALLENGE"
	st.serverSecret = serverSecret
	st.serverChallenge = serverChallenge
	m.setState(key, st)
	return PairingResponse{Paired: 1, ChallengeResponse: hex.EncodeToString(encrypted), Response: Response{StatusCode: 200}}
}

// phase3: store the client's hash and send back the signed server secret.
func (m *PairingManager) phase3(key, serverChallengeResp string) PairingResponse {
	resp, err := hex.DecodeString(serverChallengeResp)
	if err != nil {
		return failPair(m.log, "bad server challenge response")
	}
	st, ok := m.getState(key)
	if !ok {
		return failPair(m.log, "pairing session not found")
	}
	if st.phase != "CLIENTCHALLENGE" {
		return failPair(m.log, "out of order pair request (phase 3)")
	}
	clientHash, err := aesDecryptECB(resp, st.aesKey)
	if err != nil {
		return failPair(m.log, "decrypt server challenge response: "+err.Error())
	}
	signer, ok := m.cert.PrivateKey.(crypto.Signer)
	if !ok {
		return failPair(m.log, "server key cannot sign")
	}
	signature, err := signer.Sign(rand.Reader, hash(st.serverSecret), crypto.SHA256)
	if err != nil {
		return failPair(m.log, "sign server secret: "+err.Error())
	}
	st.phase = "SERVERCHALLENGERESP"
	st.clientHash = clientHash
	m.setState(key, st)
	return PairingResponse{
		Paired:        1,
		PairingSecret: hex.EncodeToString(bytes.Join([][]byte{st.serverSecret, signature}, nil)),
		Response:      Response{StatusCode: 200},
	}
}

// phase4: verify the client's pairing secret and persist the pairing.
func (m *PairingManager) phase4(ctx context.Context, key, pairingSecret string) PairingResponse {
	data, err := hex.DecodeString(pairingSecret)
	if err != nil || len(data) < 16+32 {
		return failPair(m.log, "bad pairing secret")
	}
	st, ok := m.getState(key)
	if !ok {
		return failPair(m.log, "pairing session not found")
	}
	if st.phase != "SERVERCHALLENGERESP" {
		return failPair(m.log, "out of order pair request (phase 4)")
	}
	clientSecret, clientSignature := data[:16], data[16:]
	if !bytes.Equal(hash(st.serverChallenge, st.clientCert.Signature, clientSecret), st.clientHash) {
		m.deleteState(key)
		return failPair(m.log, "client hash mismatch (wrong PIN?)")
	}
	if err := verifySignature(st.clientCert.PublicKey, clientSecret, clientSignature); err != nil {
		m.deleteState(key)
		return failPair(m.log, "client signature: "+err.Error())
	}
	m.deleteState(key)
	p := &store.Pairing{
		ID:      Fingerprint(st.clientCert),
		UserID:  st.userID,
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: st.clientCert.Raw})),
		Name:    st.clientCert.Subject.CommonName,
		Via:     st.via,
	}
	if err := m.pairings.Upsert(ctx, p); err != nil {
		return failPair(m.log, "save pairing: "+err.Error())
	}
	m.log.Info("client paired", "fingerprint", p.ID, "user", p.UserID)
	return PairingResponse{Paired: 1, Response: Response{StatusCode: 200}}
}

// Unpair forgets any in-flight handshake for the key; persisted pairings
// are removed through the hub UI.
func (m *PairingManager) Unpair(key string) { m.deleteState(key) }

// keyFor builds the handshake key from the client id and address.
func keyFor(clientID, clientIP string) string { return fmt.Sprintf("%s@%s", clientID, clientIP) }
