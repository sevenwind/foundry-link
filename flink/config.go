package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxPlayers   = 11
	defaultPort  = 30000
	alpn         = "flink/1"
	tlsServerSNI = "foundrylink"
)

// PlayerRec — игрок, которого мастер допустил (по ответному коду).
type PlayerRec struct {
	ID    string `json:"id"` // hex отпечатка сертификата (32 символа)
	Name  string `json:"name"`
	V4    string `json:"v4,omitempty"`
	V6    string `json:"v6,omitempty"`
	Added int64  `json:"added"`
}

type Settings struct {
	Mode          string      `json:"mode"` // "", "host", "player"
	HostEnabled   bool        `json:"hostEnabled"`
	PlayerEnabled bool        `json:"playerEnabled"`
	HostName      string      `json:"hostName"`
	PlayerName    string      `json:"playerName"`
	FoundryPort   int         `json:"foundryPort"`
	LocalPort     int         `json:"localPort"`
	UDPPort       int         `json:"udpPort"`
	RoomCode      string      `json:"roomCode"`
	Players       []PlayerRec `json:"players"`
}

// Store хранит настройки в JSON. Все методы потокобезопасны.
type Store struct {
	dir string
	mu  sync.Mutex
	s   Settings
}

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &Store{dir: dir}
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &st.s)
	}
	if st.s.FoundryPort == 0 {
		st.s.FoundryPort = defaultPort
	}
	if st.s.LocalPort == 0 {
		st.s.LocalPort = defaultPort
	}
	if st.s.UDPPort == 0 {
		var b [2]byte
		rand.Read(b[:])
		st.s.UDPPort = 20000 + int(uint16(b[0])<<8|uint16(b[1]))%40000
	}
	if st.s.HostName == "" {
		st.s.HostName, _ = os.Hostname()
	}
	return st, st.saveLocked()
}

func (st *Store) saveLocked() error {
	b, _ := json.MarshalIndent(st.s, "", "  ")
	tmp := filepath.Join(st.dir, "settings.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(st.dir, "settings.json"))
}

// Update применяет изменение и сохраняет файл.
func (st *Store) Update(f func(*Settings)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f(&st.s)
	_ = st.saveLocked()
}

func (st *Store) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.s
	s.Players = append([]PlayerRec(nil), st.s.Players...)
	return s
}

// ---- Личность (самоподписанный сертификат) -------------------------------

type Identity struct {
	Cert tls.Certificate
	FP   [16]byte
}

func fpOf(der []byte) [16]byte {
	h := sha256.Sum256(der)
	var fp [16]byte
	copy(fp[:], h[:16])
	return fp
}

func loadIdentity(dir string) (*Identity, error) {
	path := filepath.Join(dir, "identity.pem")
	if b, err := os.ReadFile(path); err == nil {
		if cert, err := tls.X509KeyPair(b, b); err == nil {
			return &Identity{Cert: cert, FP: fpOf(cert.Certificate[0])}, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "foundrylink"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(20 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemBytes := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})...)
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		return nil, err
	}
	return &Identity{Cert: cert, FP: fpOf(der)}, nil
}

func firstCertFP(raw [][]byte) ([16]byte, error) {
	if len(raw) == 0 {
		return [16]byte{}, errors.New("нет сертификата")
	}
	return fpOf(raw[0]), nil
}
