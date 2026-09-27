package home

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const homeCertificateRequestTimeout = 30 * time.Second

type homeJWTClaims struct {
	CertificateID    string `json:"certificate_id"`
	ClusterID        string `json:"cluster_id"`
	CAFingerprint    string `json:"ca_fingerprint"`
	EnrollmentSecret string `json:"enrollment_secret"`
	IP               string `json:"ip"`
	Port             int    `json:"port"`
	IssuedAt         int64  `json:"iat"`
}

type certificateRequestResponse struct {
	OK          bool   `json:"ok"`
	Certificate string `json:"certificate"`
	CA          string `json:"ca"`
}

type certificatePaths struct {
	Dir        string
	ClientCert string
	ClientKey  string
	CACert     string
}

// ConfigFromJWT prepares a Home config from the JWT and ensures local mTLS files exist.
func ConfigFromJWT(ctx context.Context, rawJWT string) (config.HomeConfig, error) {
	claims, errClaims := parseHomeJWTClaims(rawJWT)
	if errClaims != nil {
		return config.HomeConfig{}, errClaims
	}
	paths, errPaths := defaultCertificatePaths()
	if errPaths != nil {
		return config.HomeConfig{}, errPaths
	}
	if errEnsure := ensureHomeCertificateFiles(ctx, claims, paths); errEnsure != nil {
		return config.HomeConfig{}, errEnsure
	}
	return config.HomeConfig{
		Enabled: true,
		NodeID:  strings.TrimSpace(claims.CertificateID),
		Host:    strings.TrimSpace(claims.IP),
		Port:    claims.Port,
		TLS: config.HomeTLSConfig{
			Enable:              true,
			CACert:              paths.CACert,
			ClientCert:          paths.ClientCert,
			ClientKey:           paths.ClientKey,
			UseTargetServerName: true,
		},
	}, nil
}

func parseHomeJWTClaims(rawJWT string) (homeJWTClaims, error) {
	var claims homeJWTClaims
	parts := strings.Split(strings.TrimSpace(rawJWT), ".")
	if len(parts) != 3 {
		return claims, fmt.Errorf("home jwt is invalid")
	}
	payload, errDecode := decodeJWTPart(parts[1])
	if errDecode != nil {
		return claims, errDecode
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return claims, errUnmarshal
	}
	if strings.TrimSpace(claims.CertificateID) == "" {
		return claims, fmt.Errorf("home jwt certificate_id is required")
	}
	if strings.TrimSpace(claims.ClusterID) == "" {
		return claims, fmt.Errorf("home jwt cluster_id is required")
	}
	if normalizeFingerprint(claims.CAFingerprint) == "" {
		return claims, fmt.Errorf("home jwt ca_fingerprint is required")
	}
	if strings.TrimSpace(claims.EnrollmentSecret) == "" {
		return claims, fmt.Errorf("home jwt enrollment_secret is required")
	}
	if strings.TrimSpace(claims.IP) == "" || claims.Port <= 0 {
		return claims, fmt.Errorf("home jwt target address is invalid")
	}
	return claims, nil
}

func decodeJWTPart(part string) ([]byte, error) {
	if decoded, errDecode := base64.RawURLEncoding.DecodeString(part); errDecode == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(part)
}

func defaultCertificatePaths() (certificatePaths, error) {
	homeDir, errHome := os.UserHomeDir()
	if errHome != nil {
		return certificatePaths{}, errHome
	}
	dir := filepath.Join(homeDir, ".cli-proxy-api")
	return certificatePaths{
		Dir:        dir,
		ClientCert: filepath.Join(dir, "client-crt.pem"),
		ClientKey:  filepath.Join(dir, "client-key.pem"),
		CACert:     filepath.Join(dir, "home-ca-crt.pem"),
	}, nil
}

func ensureHomeCertificateFiles(ctx context.Context, claims homeJWTClaims, paths certificatePaths) error {
	if fileExists(paths.ClientCert) && fileExists(paths.ClientKey) {
		if !fileExists(paths.CACert) {
			return fmt.Errorf("home ca certificate file is missing")
		}
		certPEM, errCert := os.ReadFile(paths.ClientCert)
		if errCert != nil {
			return errCert
		}
		keyPEM, errKey := os.ReadFile(paths.ClientKey)
		if errKey != nil {
			return errKey
		}
		caPEM, errCA := os.ReadFile(paths.CACert)
		if errCA != nil {
			return errCA
		}
		if errVerify := verifyHomeClientCertificate(certPEM, keyPEM, caPEM, claims); errVerify != nil {
			return fmt.Errorf("home cached certificate is invalid; use the matching identity or renew the certificate files: %w", errVerify)
		}
		if errChmod := chmodCertificateFiles(paths); errChmod != nil {
			return errChmod
		}
		return nil
	}
	if errMkdir := os.MkdirAll(paths.Dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	key, errKey := loadOrCreateClientKey(paths.ClientKey)
	if errKey != nil {
		return errKey
	}
	csrPEM, errCSR := createClientCSR(claims.CertificateID, key)
	if errCSR != nil {
		return errCSR
	}
	response, errRequest := requestClientCertificate(ctx, claims, csrPEM)
	if errRequest != nil {
		return errRequest
	}
	if strings.TrimSpace(response.Certificate) == "" || strings.TrimSpace(response.CA) == "" {
		return fmt.Errorf("home certificate response is incomplete")
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if errVerify := verifyHomeClientCertificate([]byte(response.Certificate), keyPEM, []byte(response.CA), claims); errVerify != nil {
		return fmt.Errorf("home issued certificate is invalid: %w", errVerify)
	}
	if errWrite := writeFile0600(paths.ClientCert, []byte(response.Certificate)); errWrite != nil {
		return errWrite
	}
	if errWrite := writeFile0600(paths.CACert, []byte(response.CA)); errWrite != nil {
		return errWrite
	}
	return nil
}

// pinnedCACertificate accepts exactly one trust anchor, never additional unpinned roots.
func pinnedCACertificate(raw []byte, expectedFingerprint string) (*x509.Certificate, error) {
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("home ca must contain exactly one PEM certificate")
	}
	cert, errParse := x509.ParseCertificate(block.Bytes)
	if errParse != nil {
		return nil, fmt.Errorf("home ca certificate: %w", errParse)
	}
	if errVerify := verifyPinnedCA(cert, expectedFingerprint); errVerify != nil {
		return nil, errVerify
	}
	return cert, nil
}

func verifyPinnedCA(cert *x509.Certificate, expectedFingerprint string) error {
	expected := normalizeFingerprint(expectedFingerprint)
	decoded, errDecode := hex.DecodeString(expected)
	if errDecode != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("home ca fingerprint must be a SHA-256 digest")
	}
	sum := sha256.Sum256(cert.Raw)
	if !bytes.Equal(sum[:], decoded) {
		return fmt.Errorf("home ca fingerprint mismatch")
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || (cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0) {
		return fmt.Errorf("home pinned certificate is not a signing CA")
	}
	return nil
}

func verifyHomeClientCertificate(certPEM, keyPEM, caPEM []byte, claims homeJWTClaims) error {
	ca, errCA := pinnedCACertificate(caPEM, claims.CAFingerprint)
	if errCA != nil {
		return errCA
	}
	pair, errPair := tls.X509KeyPair(certPEM, keyPEM)
	if errPair != nil {
		return fmt.Errorf("home client certificate/key pair: %w", errPair)
	}
	leaf, errLeaf := x509.ParseCertificate(pair.Certificate[0])
	if errLeaf != nil {
		return fmt.Errorf("home client certificate: %w", errLeaf)
	}
	if certificateID := strings.TrimSpace(claims.CertificateID); certificateID == "" || leaf.Subject.CommonName != certificateID {
		return fmt.Errorf("home client certificate identity does not match certificate_id")
	}
	if leaf.IsCA {
		return fmt.Errorf("home client certificate must not be a CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	intermediates := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		cert, errParse := x509.ParseCertificate(raw)
		if errParse != nil {
			return fmt.Errorf("home client certificate chain: %w", errParse)
		}
		intermediates.AddCert(cert)
	}
	if _, errVerify := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); errVerify != nil {
		return fmt.Errorf("home client certificate verification: %w", errVerify)
	}
	return nil
}

// enrollmentTLSConfig bootstraps trust only from the CA fingerprint in the claim.
// The server must send that CA with its chain, including a self-signed root when used.
func enrollmentTLSConfig(claims homeJWTClaims) (*tls.Config, error) {
	fingerprint := normalizeFingerprint(claims.CAFingerprint)
	decoded, errDecode := hex.DecodeString(fingerprint)
	if errDecode != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("home ca fingerprint must be a SHA-256 digest")
	}
	serverName := strings.TrimSpace(claims.IP)
	if serverName == "" {
		return nil, fmt.Errorf("home enrollment server name is required")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
		// Trust is bootstrapped from the pinned CA below, not the system roots.
		// VerifyConnection performs full chain, validity, serverAuth and name checks.
		InsecureSkipVerify: true, //nolint:gosec -- verification is mandatory in VerifyConnection.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("home enrollment server sent no certificates")
			}
			roots := x509.NewCertPool()
			intermediates := x509.NewCertPool()
			pinned := false
			for _, cert := range state.PeerCertificates {
				if errVerify := verifyPinnedCA(cert, fingerprint); errVerify == nil {
					roots.AddCert(cert)
					pinned = true
				}
			}
			if !pinned {
				return fmt.Errorf("home enrollment server did not present the pinned CA")
			}
			for _, cert := range state.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			if _, errVerify := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots: roots, Intermediates: intermediates, DNSName: serverName,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}); errVerify != nil {
				return fmt.Errorf("home enrollment server certificate verification: %w", errVerify)
			}
			return nil
		},
	}, nil
}

func certificateFingerprintPEM(raw []byte) (string, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("home ca certificate pem is invalid")
	}
	cert, errParse := x509.ParseCertificate(block.Bytes)
	if errParse != nil {
		return "", errParse
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeFingerprint(fingerprint string) string {
	fingerprint = strings.TrimSpace(strings.ToLower(fingerprint))
	fingerprint = strings.ReplaceAll(fingerprint, ":", "")
	fingerprint = strings.ReplaceAll(fingerprint, " ", "")
	return fingerprint
}

func loadOrCreateClientKey(path string) (*rsa.PrivateKey, error) {
	if fileExists(path) {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return nil, errRead
		}
		key, errParse := parseRSAPrivateKeyPEM(raw)
		if errParse != nil {
			return nil, errParse
		}
		if errChmod := os.Chmod(path, 0o600); errChmod != nil {
			return nil, errChmod
		}
		return key, nil
	}
	key, errKey := rsa.GenerateKey(rand.Reader, 2048)
	if errKey != nil {
		return nil, errKey
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if errWrite := writeFile0600(path, raw); errWrite != nil {
		return nil, errWrite
	}
	return key, nil
}

func writeFile0600(path string, raw []byte) error {
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Chmod(path, 0o600)
}

func chmodCertificateFiles(paths certificatePaths) error {
	for _, path := range []string{paths.ClientCert, paths.ClientKey, paths.CACert} {
		if errChmod := os.Chmod(path, 0o600); errChmod != nil {
			return errChmod
		}
	}
	return nil
}

func parseRSAPrivateKeyPEM(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("client key pem is invalid")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, errParse := x509.ParsePKCS8PrivateKey(block.Bytes)
		if errParse != nil {
			return nil, errParse
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("client key is not rsa")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("client key pem type %q is unsupported", block.Type)
	}
}

func createClientCSR(certificateID string, key *rsa.PrivateKey) ([]byte, error) {
	certificateID = strings.TrimSpace(certificateID)
	if certificateID == "" {
		return nil, fmt.Errorf("certificate id is required")
	}
	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName: certificateID,
		},
	}
	der, errCreate := x509.CreateCertificateRequest(rand.Reader, template, key)
	if errCreate != nil {
		return nil, errCreate
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

func requestClientCertificate(ctx context.Context, claims homeJWTClaims, csrPEM []byte) (certificateRequestResponse, error) {
	var response certificateRequestResponse
	if ctx == nil {
		ctx = context.Background()
	}
	tlsConfig, errTLS := enrollmentTLSConfig(claims)
	if errTLS != nil {
		return response, errTLS
	}
	dialCtx, cancel := context.WithTimeout(ctx, homeCertificateRequestTimeout)
	defer cancel()
	addr := net.JoinHostPort(strings.TrimSpace(claims.IP), strconv.Itoa(claims.Port))
	rawConn, errDial := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	if errDial != nil {
		return response, errDial
	}
	conn := tls.Client(rawConn, tlsConfig)
	defer func() {
		_ = conn.Close()
	}()
	stopCancel := context.AfterFunc(dialCtx, func() { _ = rawConn.Close() })
	defer stopCancel()
	// This short-lived connection only acquires credentials; no deadline is
	// applied to subsequent authenticated Home traffic.
	if deadline, ok := dialCtx.Deadline(); ok {
		if errDeadline := conn.SetDeadline(deadline); errDeadline != nil {
			return response, errDeadline
		}
	}
	if errHandshake := conn.HandshakeContext(dialCtx); errHandshake != nil {
		return response, fmt.Errorf("home enrollment requires authenticated TLS: %w", errHandshake)
	}
	if _, errWrite := conn.Write(encodeRESPArray("CERTIFICATE", "REQUEST", claims.CertificateID, claims.EnrollmentSecret, string(csrPEM))); errWrite != nil {
		return response, errWrite
	}
	raw, errRead := readRESPBulk(bufio.NewReader(conn))
	if errRead != nil {
		return response, errRead
	}
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return response, errUnmarshal
	}
	if !response.OK {
		return response, fmt.Errorf("home certificate request failed")
	}
	return response, nil
}

func encodeRESPArray(args ...string) []byte {
	var buf bytes.Buffer
	buf.WriteString("*")
	buf.WriteString(strconv.Itoa(len(args)))
	buf.WriteString("\r\n")
	for _, arg := range args {
		buf.WriteString("$")
		buf.WriteString(strconv.Itoa(len(arg)))
		buf.WriteString("\r\n")
		buf.WriteString(arg)
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

func readRESPBulk(reader *bufio.Reader) ([]byte, error) {
	prefix, errRead := reader.ReadByte()
	if errRead != nil {
		return nil, errRead
	}
	switch prefix {
	case '$':
		line, errLine := reader.ReadString('\n')
		if errLine != nil {
			return nil, errLine
		}
		size, errSize := strconv.Atoi(strings.TrimSpace(line))
		if errSize != nil {
			return nil, errSize
		}
		if size < 0 {
			return nil, fmt.Errorf("home certificate request returned nil")
		}
		payload := make([]byte, size+2)
		if _, errFull := io.ReadFull(reader, payload); errFull != nil {
			return nil, errFull
		}
		return payload[:size], nil
	case '-':
		line, errLine := reader.ReadString('\n')
		if errLine != nil {
			return nil, errLine
		}
		return nil, fmt.Errorf("%s", strings.TrimSpace(line))
	default:
		return nil, fmt.Errorf("home certificate request returned unsupported resp prefix %q", prefix)
	}
}

func fileExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && !info.IsDir()
}
