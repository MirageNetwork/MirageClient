// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !js

package ipnlocal

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/exp/slices"
	"tailscale.com/atomicfile"
	"tailscale.com/envknob"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/types/logger"
	"tailscale.com/version"
	"tailscale.com/version/distro"
)

// Process-wide cache. (A new *Handler is created per connection,
// effectively per request)
var (
	// acmeMu guards all ACME operations, so concurrent requests
	// for certs don't slam ACME. The first will go through and
	// populate the on-disk cache and the rest should use that.
	acmeMu sync.Mutex

	renewMu        sync.Mutex // lock order: don't hold acmeMu and renewMu at the same time
	lastRenewCheck = map[string]time.Time{}
)

// certDir returns (creating if needed) the directory in which cached
// cert keypairs are stored.
func (b *LocalBackend) certDir() (string, error) {
	d := b.TailscaleVarRoot()

	// As a workaround for Synology DSM6 not having a "var" directory, use the
	// app's "etc" directory (on a small partition) to hold certs at least.
	// See https://github.com/tailscale/tailscale/issues/4060#issuecomment-1186592251
	if d == "" && runtime.GOOS == "linux" && distro.Get() == distro.Synology && distro.DSMVersion() == 6 {
		d = "/var/packages/Tailscale/etc" // base; we append "certs" below
	}
	if d == "" {
		return "", errors.New("no TailscaleVarRoot")
	}
	full := filepath.Join(d, "certs")
	if err := os.MkdirAll(full, 0700); err != nil {
		return "", err
	}
	return full, nil
}

var acmeDebug = envknob.RegisterBool("TS_DEBUG_ACME")

// getCertPEM gets the KeyPair for domain, either from cache, via the ACME
// process, or from cache and kicking off an async ACME renewal.
func (b *LocalBackend) GetCertPEM(ctx context.Context, domain string) (*TLSCertKeyPair, error) {
	if !validLookingCertDomain(domain) {
		return nil, errors.New("invalid domain")
	}
	logf := logger.WithPrefix(b.logf, fmt.Sprintf("cert(%q): ", domain))
	now := time.Now()
	traceACME := func(v any) {
		if !acmeDebug() {
			return
		}
		j, _ := json.MarshalIndent(v, "", "\t")
		log.Printf("acme %T: %s", v, j)
	}

	cs, err := b.getCertStore()
	if err != nil {
		return nil, err
	}

	if pair, err := getCertPEMCached(cs, domain, now); err == nil {
		future := now.AddDate(0, 0, 14)
		if b.shouldStartDomainRenewal(cs, domain, future) {
			logf("starting async renewal")
			// Start renewal in the background.
			go b.getCertPEM(context.Background(), cs, logf, traceACME, domain, future)
		}
		return pair, nil
	}

	pair, err := b.getCertPEM(ctx, cs, logf, traceACME, domain, now)
	if err != nil {
		logf("getCertPEM: %v", err)
		return nil, err
	}
	return pair, nil
}

func (b *LocalBackend) shouldStartDomainRenewal(cs certStore, domain string, future time.Time) bool {
	renewMu.Lock()
	defer renewMu.Unlock()
	now := time.Now()
	if last, ok := lastRenewCheck[domain]; ok && now.Sub(last) < time.Minute {
		// We checked very recently. Don't bother reparsing &
		// validating the x509 cert.
		return false
	}
	lastRenewCheck[domain] = now
	_, err := getCertPEMCached(cs, domain, future)
	return errors.Is(err, errCertExpired)
}

// certStore provides a way to perist and retrieve TLS certificates.
// As of 2023-02-01, we use store certs in directories on disk everywhere
// except on Kubernetes, where we use the state store.
type certStore interface {
	// Read returns the cert and key for domain, if they exist and are valid
	// for now. If they're expired, it returns errCertExpired.
	// If they don't exist, it returns ipn.ErrStateNotExist.
	Read(domain string, now time.Time) (*TLSCertKeyPair, error)
	// WriteCert writes the cert for domain.
	WriteCert(domain string, cert []byte) error
	// WriteKey writes the key for domain.
	WriteKey(domain string, key []byte) error
	// ACMEKey returns the value previously stored via WriteACMEKey.
	// It is a PEM encoded ECDSA key.
	ACMEKey() ([]byte, error)
	// WriteACMEKey stores the provided PEM encoded ECDSA key.
	WriteACMEKey([]byte) error
}

var errCertExpired = errors.New("cert expired")

func (b *LocalBackend) getCertStore() (certStore, error) {
	switch b.store.(type) {
	case *store.FileStore:
	case *mem.Store:
	default:
		if hostinfo.GetEnvType() == hostinfo.Kubernetes {
			// We're running in Kubernetes with a custom StateStore,
			// use that instead of the cert directory.
			// TODO(maisem): expand this to other environments?
			return certStateStore{StateStore: b.store}, nil
		}
	}
	dir, err := b.certDir()
	if err != nil {
		return nil, err
	}
	return certFileStore{dir: dir}, nil
}

// certFileStore implements certStore by storing the cert & key files in the named directory.
type certFileStore struct {
	dir string

	// This field allows a test to override the CA root(s) for certificate
	// verification. If nil the default system pool is used.
	testRoots *x509.CertPool
}

const acmePEMName = "acme-account.key.pem"

func (f certFileStore) ACMEKey() ([]byte, error) {
	pemName := filepath.Join(f.dir, acmePEMName)
	v, err := os.ReadFile(pemName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ipn.ErrStateNotExist
		}
		return nil, err
	}
	return v, nil
}

func (f certFileStore) WriteACMEKey(b []byte) error {
	pemName := filepath.Join(f.dir, acmePEMName)
	return atomicfile.WriteFile(pemName, b, 0600)
}

func (f certFileStore) Read(domain string, now time.Time) (*TLSCertKeyPair, error) {
	certPEM, err := os.ReadFile(certFile(f.dir, domain))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ipn.ErrStateNotExist
		}
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyFile(f.dir, domain))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ipn.ErrStateNotExist
		}
		return nil, err
	}
	if !validCertPEM(domain, keyPEM, certPEM, f.testRoots, now) {
		return nil, errCertExpired
	}
	return &TLSCertKeyPair{CertPEM: certPEM, KeyPEM: keyPEM, Cached: true}, nil
}

func (f certFileStore) WriteCert(domain string, cert []byte) error {
	return atomicfile.WriteFile(certFile(f.dir, domain), cert, 0644)
}

func (f certFileStore) WriteKey(domain string, key []byte) error {
	return atomicfile.WriteFile(keyFile(f.dir, domain), key, 0600)
}

// certStateStore implements certStore by storing the cert & key files in an ipn.StateStore.
type certStateStore struct {
	ipn.StateStore

	// This field allows a test to override the CA root(s) for certificate
	// verification. If nil the default system pool is used.
	testRoots *x509.CertPool
}

func (s certStateStore) Read(domain string, now time.Time) (*TLSCertKeyPair, error) {
	certPEM, err := s.ReadState(ipn.StateKey(domain + ".crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := s.ReadState(ipn.StateKey(domain + ".key"))
	if err != nil {
		return nil, err
	}
	if !validCertPEM(domain, keyPEM, certPEM, s.testRoots, now) {
		return nil, errCertExpired
	}
	return &TLSCertKeyPair{CertPEM: certPEM, KeyPEM: keyPEM, Cached: true}, nil
}

func (s certStateStore) WriteCert(domain string, cert []byte) error {
	return s.WriteState(ipn.StateKey(domain+".crt"), cert)
}

func (s certStateStore) WriteKey(domain string, key []byte) error {
	return s.WriteState(ipn.StateKey(domain+".key"), key)
}

func (s certStateStore) ACMEKey() ([]byte, error) {
	return s.ReadState(ipn.StateKey(acmePEMName))
}

func (s certStateStore) WriteACMEKey(key []byte) error {
	return s.WriteState(ipn.StateKey(acmePEMName), key)
}

// TLSCertKeyPair is a TLS public and private key, and whether they were obtained
// from cache or freshly obtained.
type TLSCertKeyPair struct {
	CertPEM []byte // public key, in PEM form
	KeyPEM  []byte // private key, in PEM form
	Cached  bool   // whether result came from cache
}

func keyFile(dir, domain string) string  { return filepath.Join(dir, domain+".key") }
func certFile(dir, domain string) string { return filepath.Join(dir, domain+".crt") }

// getCertPEMCached returns a non-nil keyPair and true if a cached keypair for
// domain exists on disk in dir that is valid at the provided now time.
// If the keypair is expired, it returns errCertExpired.
// If the keypair doesn't exist, it returns ipn.ErrStateNotExist.
func getCertPEMCached(cs certStore, domain string, now time.Time) (p *TLSCertKeyPair, err error) {
	if !validLookingCertDomain(domain) {
		// Before we read files from disk using it, validate it's halfway
		// reasonable looking.
		return nil, fmt.Errorf("invalid domain %q", domain)
	}
	return cs.Read(domain, now)
}

func (b *LocalBackend) getCertPEM(ctx context.Context, cs certStore, logf logger.Logf, traceACME func(any), domain string, now time.Time) (*TLSCertKeyPair, error) {
	acmeMu.Lock()
	defer acmeMu.Unlock()

	if p, err := getCertPEMCached(cs, domain, now); err == nil {
		return p, nil
	} else if !errors.Is(err, ipn.ErrStateNotExist) && !errors.Is(err, errCertExpired) {
		return nil, err
	}

	key, err := acmeKey(cs)
	if err != nil {
		return nil, fmt.Errorf("acmeKey: %w", err)
	}
	ac := &acme.Client{
		Key:       key,
		UserAgent: "tailscaled/" + version.Long(),
	}

	a, err := ac.GetReg(ctx, "" /* pre-RFC param */)
	switch {
	case err == nil:
		// Great, already registered.
		logf("already had ACME account.")
	case err == acme.ErrNoAccount:
		a, err = ac.Register(ctx, new(acme.Account), acme.AcceptTOS)
		if err == acme.ErrAccountAlreadyExists {
			// Potential race. Double check.
			a, err = ac.GetReg(ctx, "" /* pre-RFC param */)
		}
		if err != nil {
			return nil, fmt.Errorf("acme.Register: %w", err)
		}
		logf("registered ACME account.")
		traceACME(a)
	default:
		return nil, fmt.Errorf("acme.GetReg: %w", err)

	}
	if a.Status != acme.StatusValid {
		return nil, fmt.Errorf("unexpected ACME account status %q", a.Status)
	}

	// Before hitting LetsEncrypt, see if this is a domain that Tailscale will do DNS challenges for.
	st := b.StatusWithoutPeers()
	if err := checkCertDomain(st, domain); err != nil {
		return nil, err
	}

	order, err := ac.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: domain}})
	if err != nil {
		return nil, err
	}
	traceACME(order)

	for _, aurl := range order.AuthzURLs {
		az, err := ac.GetAuthorization(ctx, aurl)
		if err != nil {
			return nil, err
		}
		traceACME(az)
		for _, ch := range az.Challenges {
			if ch.Type == "dns-01" {
				rec, err := ac.DNS01ChallengeRecord(ch.Token)
				if err != nil {
					return nil, err
				}
				key := "_acme-challenge." + domain

				// Do a best-effort lookup to see if we've already created this DNS name
				// in a previous attempt. Don't burn too much time on it, though. Worst
				// case we ask the server to create something that already exists.
				var resolver net.Resolver
				lookupCtx, lookupCancel := context.WithTimeout(ctx, 500*time.Millisecond)
				txts, _ := resolver.LookupTXT(lookupCtx, key)
				lookupCancel()
				if slices.Contains(txts, rec) {
					logf("TXT record already existed")
				} else {
					logf("starting SetDNS call...")
					err = b.SetDNS(ctx, key, rec)
					if err != nil {
						return nil, fmt.Errorf("SetDNS %q => %q: %w", key, rec, err)
					}
					logf("did SetDNS")
				}

				chal, err := ac.Accept(ctx, ch)
				if err != nil {
					return nil, fmt.Errorf("Accept: %v", err)
				}
				traceACME(chal)
				break
			}
		}
	}

	orderURI := order.URI
	order, err = ac.WaitOrder(ctx, orderURI)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if oe, ok := err.(*acme.OrderError); ok {
			logf("acme: WaitOrder: OrderError status %q", oe.Status)
		} else {
			logf("acme: WaitOrder error: %v", err)
		}
		return nil, err
	}
	traceACME(order)

	certPrivKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	var privPEM bytes.Buffer
	if err := encodeECDSAKey(&privPEM, certPrivKey); err != nil {
		return nil, err
	}
	if err := cs.WriteKey(domain, privPEM.Bytes()); err != nil {
		return nil, err
	}

	csr, err := certRequest(certPrivKey, domain, nil)
	if err != nil {
		return nil, err
	}

	logf("requesting cert...")
	der, _, err := ac.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("CreateOrder: %v", err)
	}
	logf("got cert")

	var certPEM bytes.Buffer
	for _, b := range der {
		pb := &pem.Block{Type: "CERTIFICATE", Bytes: b}
		if err := pem.Encode(&certPEM, pb); err != nil {
			return nil, err
		}
	}
	if err := cs.WriteCert(domain, certPEM.Bytes()); err != nil {
		return nil, err
	}

	return &TLSCertKeyPair{CertPEM: certPEM.Bytes(), KeyPEM: privPEM.Bytes()}, nil
}

// certRequest generates a CSR for the given common name cn and optional SANs.
func certRequest(key crypto.Signer, cn string, ext []pkix.Extension, san ...string) ([]byte, error) {
	req := &x509.CertificateRequest{
		Subject:         pkix.Name{CommonName: cn},
		DNSNames:        san,
		ExtraExtensions: ext,
	}
	return x509.CreateCertificateRequest(rand.Reader, req, key)
}

func encodeECDSAKey(w io.Writer, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	pb := &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	return pem.Encode(w, pb)
}

// parsePrivateKey is a copy of x/crypto/acme's parsePrivateKey.
//
// Attempt to parse the given private key DER block. OpenSSL 0.9.8 generates
// PKCS#1 private keys by default, while OpenSSL 1.0.0 generates PKCS#8 keys.
// OpenSSL ecparam generates SEC1 EC private keys for ECDSA. We try all three.
//
// Inspired by parsePrivateKey in crypto/tls/tls.go.
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		switch key := key.(type) {
		case *rsa.PrivateKey:
			return key, nil
		case *ecdsa.PrivateKey:
			return key, nil
		default:
			return nil, errors.New("acme/autocert: unknown private key type in PKCS#8 wrapping")
		}
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}

	return nil, errors.New("acme/autocert: failed to parse private key")
}

func acmeKey(cs certStore) (crypto.Signer, error) {
	if v, err := cs.ACMEKey(); err == nil {
		priv, _ := pem.Decode(v)
		if priv == nil || !strings.Contains(priv.Type, "PRIVATE") {
			return nil, errors.New("acme/autocert: invalid account key found in cache")
		}
		return parsePrivateKey(priv.Bytes)
	} else if err != nil && !errors.Is(err, ipn.ErrStateNotExist) {
		return nil, err
	}

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	var pemBuf bytes.Buffer
	if err := encodeECDSAKey(&pemBuf, privKey); err != nil {
		return nil, err
	}
	if err := cs.WriteACMEKey(pemBuf.Bytes()); err != nil {
		return nil, err
	}
	return privKey, nil
}

// validCertPEM reports whether the given certificate is valid for domain at now.
//
// If roots != nil, it is used instead of the system root pool. This is meant
// to support testing, and production code should pass roots == nil.
func validCertPEM(domain string, keyPEM, certPEM []byte, roots *x509.CertPool, now time.Time) bool {
	if len(keyPEM) == 0 || len(certPEM) == 0 {
		return false
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false
	}

	var leaf *x509.Certificate
	intermediates := x509.NewCertPool()
	for i, certDER := range tlsCert.Certificate {
		cert, err := x509.ParseCertificate(certDER)
		if err != nil {
			return false
		}
		if i == 0 {
			leaf = cert
		} else {
			intermediates.AddCert(cert)
		}
	}
	if leaf == nil {
		return false
	}
	_, err = leaf.Verify(x509.VerifyOptions{
		DNSName:       domain,
		CurrentTime:   now,
		Roots:         roots,
		Intermediates: intermediates,
	})
	return err == nil
}

// validLookingCertDomain reports whether name looks like a valid domain name that
// we might be able to get a cert for.
//
// It's a light check primarily for double checking before it's used
// as part of a filesystem path. The actual validation happens in checkCertDomain.
func validLookingCertDomain(name string) bool {
	if name == "" ||
		strings.Contains(name, "..") ||
		strings.ContainsAny(name, ":/\\\x00") ||
		!strings.Contains(name, ".") {
		return false
	}
	return true
}

func checkCertDomain(st *ipnstate.Status, domain string) error {
	if domain == "" {
		return errors.New("missing domain name")
	}
	for _, d := range st.CertDomains {
		if d == domain {
			return nil
		}
	}
	// Transitional way while server doesn't yet populate CertDomains: also permit the client
	// attempting Self.DNSName.
	okay := st.CertDomains[:len(st.CertDomains):len(st.CertDomains)]
	if st.Self != nil {
		if v := strings.Trim(st.Self.DNSName, "."); v != "" {
			if v == domain {
				return nil
			}
			okay = append(okay, v)
		}
	}
	switch len(okay) {
	case 0:
		return errors.New("your Tailscale account does not support getting TLS certs")
	case 1:
		return fmt.Errorf("invalid domain %q; only %q is permitted", domain, okay[0])
	default:
		return fmt.Errorf("invalid domain %q; must be one of %q", domain, okay)
	}
}
