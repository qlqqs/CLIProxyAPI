package home

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newCertificateTestIdentity(t *testing.T, template *x509.Certificate, parent *tls.Certificate) tls.Certificate {
	t.Helper()
	key, errKey := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if errKey != nil {
		t.Fatal(errKey)
	}
	signer := any(key)
	issuer := template
	if parent != nil {
		signer = parent.PrivateKey
		issuer = parent.Leaf
	}
	der, errCreate := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, signer)
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	leaf, errParse := x509.ParseCertificate(der)
	if errParse != nil {
		t.Fatal(errParse)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func certificateTestTemplate(name string) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
}

func newCertificateTestCA(t *testing.T) tls.Certificate {
	t.Helper()
	template := certificateTestTemplate("home test CA")
	template.IsCA = true
	template.BasicConstraintsValid = true
	template.KeyUsage = x509.KeyUsageCertSign
	return newCertificateTestIdentity(t, template, nil)
}

func certificateTestPEM(cert tls.Certificate) []byte {
	var raw []byte
	for _, der := range cert.Certificate {
		raw = append(raw, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return raw
}

func certificateTestKeyPEM(t *testing.T, key any) []byte {
	t.Helper()
	der, errMarshal := x509.MarshalPKCS8PrivateKey(key)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func certificateTestFingerprint(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	fingerprint, errFingerprint := certificateFingerprintPEM(certificateTestPEM(cert))
	if errFingerprint != nil {
		t.Fatal(errFingerprint)
	}
	return fingerprint
}

func certificateTestClaims(t *testing.T, addr net.Addr, ca tls.Certificate) homeJWTClaims {
	t.Helper()
	host, portString, errSplit := net.SplitHostPort(addr.String())
	if errSplit != nil {
		t.Fatal(errSplit)
	}
	port, errPort := strconv.Atoi(portString)
	if errPort != nil {
		t.Fatal(errPort)
	}
	return homeJWTClaims{
		CertificateID: "test-node", ClusterID: "test-cluster", CAFingerprint: certificateTestFingerprint(t, ca),
		EnrollmentSecret: "synthetic-enrollment-secret", IP: host, Port: port,
	}
}

func waitCertificateTestResult[T any](t *testing.T, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for enrollment test peer")
		var zero T
		return zero
	}
}

// startCertificateTestServer accepts one TLS enrollment and records only decrypted requests.
func startCertificateTestServer(t *testing.T, cert tls.Certificate, reply func([]string) (certificateRequestResponse, error)) (net.Addr, <-chan []string, <-chan error) {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	requests := make(chan []string, 1)
	done := make(chan error, 1)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			done <- errAccept
			return
		}
		defer func() { _ = conn.Close() }()
		tlsConn := tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
		reader := bufio.NewReader(tlsConn)
		line, errLine := reader.ReadString('\n')
		if errLine != nil {
			done <- errLine
			return
		}
		if line != "*5\r\n" {
			done <- fmt.Errorf("unexpected request header %q", line)
			return
		}
		args := make([]string, 5)
		for index := range args {
			raw, errRead := readRESPBulk(reader)
			if errRead != nil {
				done <- errRead
				return
			}
			args[index] = string(raw)
		}
		requests <- args
		response, errReply := reply(args)
		if errReply != nil {
			done <- errReply
			return
		}
		body, errMarshal := json.Marshal(response)
		if errMarshal != nil {
			done <- errMarshal
			return
		}
		_, errWrite := fmt.Fprintf(tlsConn, "$%d\r\n%s\r\n", len(body), body)
		done <- errWrite
	}()
	return listener.Addr(), requests, done
}

func TestRequestClientCertificateAuthenticatesTLSBeforeEnrollment(t *testing.T) {
	ca := newCertificateTestCA(t)
	otherCA := newCertificateTestCA(t)
	for _, name := range []string{"pinned self-signed CA", "pinned intermediate chain", "wrong pin", "wrong name", "expired", "not yet valid", "wrong EKU", "unlinked pinned CA", "missing pinned CA", "non-CA pin"} {
		t.Run(name, func(t *testing.T) {
			template := certificateTestTemplate("home enrollment server")
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			issuer := ca
			pin := ca
			success := false
			switch name {
			case "pinned self-signed CA":
				success = true
			case "pinned intermediate chain":
				success = true
				intermediateTemplate := certificateTestTemplate("intermediate CA")
				intermediateTemplate.IsCA = true
				intermediateTemplate.BasicConstraintsValid = true
				intermediateTemplate.KeyUsage = x509.KeyUsageCertSign
				issuer = newCertificateTestIdentity(t, intermediateTemplate, &ca)
			case "wrong pin":
				pin = otherCA
			case "wrong name":
				template.IPAddresses = []net.IP{net.ParseIP("192.0.2.1")}
			case "expired":
				template.NotAfter = time.Now().Add(-time.Minute)
			case "not yet valid":
				template.NotBefore = time.Now().Add(time.Minute)
			case "wrong EKU":
				template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			case "unlinked pinned CA":
				issuer = otherCA
			}
			server := newCertificateTestIdentity(t, template, &issuer)
			if name == "non-CA pin" {
				pin = server
			}
			if name != "missing pinned CA" {
				server.Certificate = append(server.Certificate, issuer.Certificate[0], ca.Certificate[0])
			}
			addr, requests, done := startCertificateTestServer(t, server, func([]string) (certificateRequestResponse, error) {
				return certificateRequestResponse{OK: true, Certificate: "certificate", CA: "ca"}, nil
			})
			claims := certificateTestClaims(t, addr, pin)
			response, errRequest := requestClientCertificate(context.Background(), claims, []byte("synthetic-csr"))
			errServer := waitCertificateTestResult(t, done)
			if success {
				if errRequest != nil || errServer != nil || !response.OK {
					t.Fatalf("enrollment = %+v, client error %v, server error %v", response, errRequest, errServer)
				}
				args := waitCertificateTestResult(t, requests)
				if args[0] != "CERTIFICATE" || args[1] != "REQUEST" || args[2] != claims.CertificateID || args[3] != claims.EnrollmentSecret || args[4] != "synthetic-csr" {
					t.Fatal("authenticated enrollment request did not preserve the protocol fields")
				}
			} else {
				if errRequest == nil {
					t.Fatal("unauthenticated enrollment succeeded")
				}
				select {
				case <-requests:
					t.Fatal("enrollment secret sent before authenticating the server")
				default:
				}
			}
		})
	}
}

func TestRequestClientCertificateNeverFallsBackToPlaintext(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer func() { _ = listener.Close() }()
	captured := make(chan []byte, 1)
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			captured <- nil
			return
		}
		defer func() { _ = conn.Close() }()
		header := make([]byte, 5)
		if _, errRead := io.ReadFull(conn, header); errRead != nil {
			captured <- nil
			return
		}
		if header[0] != 0x16 {
			captured <- header
			return
		}
		payload := make([]byte, int(binary.BigEndian.Uint16(header[3:])))
		_, _ = io.ReadFull(conn, payload)
		captured <- append(header, payload...)
	}()
	claims := certificateTestClaims(t, listener.Addr(), newCertificateTestCA(t))
	if _, errRequest := requestClientCertificate(context.Background(), claims, []byte("synthetic-csr")); errRequest == nil {
		t.Fatal("plaintext-only enrollment must fail")
	}
	wire := waitCertificateTestResult(t, captured)
	if len(wire) == 0 || wire[0] != 0x16 || bytes.Contains(wire, []byte(claims.EnrollmentSecret)) || bytes.Contains(wire, []byte("CERTIFICATE")) {
		t.Fatal("plaintext peer received something other than a credential-free TLS ClientHello")
	}
}

func TestEnsureHomeCertificateFilesValidatesCache(t *testing.T) {
	ca := newCertificateTestCA(t)
	otherCA := newCertificateTestCA(t)
	for _, name := range []string{"valid", "wrong identity", "wrong key", "expired", "not yet valid", "wrong issuer", "wrong EKU", "CA leaf", "extra trust anchor", "wrong pin"} {
		t.Run(name, func(t *testing.T) {
			template := certificateTestTemplate("test-node")
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
			issuer := ca
			switch name {
			case "wrong identity":
				template.Subject.CommonName = "another-node"
			case "expired":
				template.NotAfter = time.Now().Add(-time.Minute)
			case "not yet valid":
				template.NotBefore = time.Now().Add(time.Minute)
			case "wrong issuer":
				issuer = otherCA
			case "wrong EKU":
				template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			case "CA leaf":
				template.IsCA = true
				template.BasicConstraintsValid = true
			}
			leaf := newCertificateTestIdentity(t, template, &issuer)
			key := leaf.PrivateKey
			if name == "wrong key" {
				key = otherCA.PrivateKey
			}
			caPEM := certificateTestPEM(ca)
			if name == "extra trust anchor" {
				caPEM = append(caPEM, certificateTestPEM(otherCA)...)
			}
			paths := certificateTestPaths(t)
			files := map[string][]byte{paths.ClientCert: certificateTestPEM(leaf), paths.ClientKey: certificateTestKeyPEM(t, key), paths.CACert: caPEM}
			for path, raw := range files {
				if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
			}
			claims := homeJWTClaims{CertificateID: "test-node", CAFingerprint: certificateTestFingerprint(t, ca)}
			if name == "wrong pin" {
				claims.CAFingerprint = certificateTestFingerprint(t, otherCA)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			errEnsure := ensureHomeCertificateFiles(ctx, claims, paths)
			if name == "valid" {
				if errEnsure != nil {
					t.Fatalf("valid cache should not re-enroll: %v", errEnsure)
				}
			} else if errEnsure == nil || !strings.Contains(errEnsure.Error(), "cached certificate is invalid") {
				t.Fatalf("invalid cache must fail closed, got %v", errEnsure)
			}
			for path, expected := range files {
				actual, errRead := os.ReadFile(path)
				if errRead != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("existing cache was overwritten: %s", path)
				}
			}
		})
	}
}

func certificateTestPaths(t *testing.T) certificatePaths {
	t.Helper()
	dir := t.TempDir()
	return certificatePaths{Dir: dir, ClientCert: filepath.Join(dir, "client.crt"), ClientKey: filepath.Join(dir, "client.key"), CACert: filepath.Join(dir, "ca.crt")}
}

func TestEnsureHomeCertificateFilesValidatesIssuedIdentity(t *testing.T) {
	ca := newCertificateTestCA(t)
	template := certificateTestTemplate("home enrollment server")
	template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	server := newCertificateTestIdentity(t, template, &ca)
	server.Certificate = append(server.Certificate, ca.Certificate[0])
	for _, wrongIdentity := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrong_identity_%t", wrongIdentity), func(t *testing.T) {
			paths := certificateTestPaths(t)
			addr, _, done := startCertificateTestServer(t, server, func(args []string) (certificateRequestResponse, error) {
				block, _ := pem.Decode([]byte(args[4]))
				if block == nil {
					return certificateRequestResponse{}, fmt.Errorf("missing CSR")
				}
				csr, errCSR := x509.ParseCertificateRequest(block.Bytes)
				if errCSR != nil {
					return certificateRequestResponse{}, errCSR
				}
				if errSignature := csr.CheckSignature(); errSignature != nil {
					return certificateRequestResponse{}, errSignature
				}
				leafTemplate := certificateTestTemplate(args[2])
				leafTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				if wrongIdentity {
					leafTemplate.Subject.CommonName = "another-node"
				}
				der, errCreate := x509.CreateCertificate(rand.Reader, leafTemplate, ca.Leaf, csr.PublicKey, ca.PrivateKey)
				if errCreate != nil {
					return certificateRequestResponse{}, errCreate
				}
				return certificateRequestResponse{OK: true, Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), CA: string(certificateTestPEM(ca))}, nil
			})
			claims := certificateTestClaims(t, addr, ca)
			errEnsure := ensureHomeCertificateFiles(context.Background(), claims, paths)
			if errServer := waitCertificateTestResult(t, done); errServer != nil {
				t.Fatal(errServer)
			}
			if wrongIdentity {
				if errEnsure == nil || fileExists(paths.ClientCert) || fileExists(paths.CACert) {
					t.Fatalf("issued wrong identity was persisted: %v", errEnsure)
				}
				return
			}
			if errEnsure != nil {
				t.Fatal(errEnsure)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if errReuse := ensureHomeCertificateFiles(ctx, claims, paths); errReuse != nil {
				t.Fatalf("issued certificate cannot be reused offline: %v", errReuse)
			}
			key, errLoad := loadOrCreateClientKey(paths.ClientKey)
			if errLoad != nil || key == nil {
				t.Fatalf("persisted RSA key unavailable: %v", errLoad)
			}
			if errValidate := key.Validate(); errValidate != nil {
				t.Fatal(errValidate)
			}
		})
	}
}

func TestRequestClientCertificateCancellationInterruptsCredentialRead(t *testing.T) {
	ca := newCertificateTestCA(t)
	template := certificateTestTemplate("home enrollment server")
	template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	server := newCertificateTestIdentity(t, template, &ca)
	server.Certificate = append(server.Certificate, ca.Certificate[0])
	release := make(chan struct{})
	addr, requests, serverDone := startCertificateTestServer(t, server, func([]string) (certificateRequestResponse, error) {
		<-release
		return certificateRequestResponse{}, fmt.Errorf("test peer stopped")
	})
	defer func() { close(release); _ = waitCertificateTestResult(t, serverDone) }()
	claims := certificateTestClaims(t, addr, ca)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientDone := make(chan error, 1)
	go func() {
		_, errRequest := requestClientCertificate(ctx, claims, []byte("synthetic-csr"))
		clientDone <- errRequest
	}()
	_ = waitCertificateTestResult(t, requests)
	cancel()
	if errRequest := waitCertificateTestResult(t, clientDone); errRequest == nil {
		t.Fatal("canceled credential read succeeded")
	}
}
