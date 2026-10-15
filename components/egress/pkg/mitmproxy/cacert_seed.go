// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mitmproxy

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// Pinned-CA seeding (OSEP-0022 "MITM Trust Anchor", fast-sandbox profile):
// the pool-pinned CA keypair arrives on a read-only seed mount
// (constants.EnvMitmCaSeedDir, `tls.crt` + `tls.key` entries) and is copied
// idempotently into the writable confdir under the names mitmdump actually
// loads — confCAKeyName (private key) + mitmCACertName (certificate).
// mitmdump loads a confdir CA when present and only generates when missing;
// in this profile it must never generate.

const (
	// SeedCACertName / SeedCAKeyName are the pinned-CA Secret entries on the
	// read-only seed mount (Kubernetes tls-Secret conventions).
	SeedCACertName = "tls.crt"
	SeedCAKeyName  = "tls.key"
	// confCAKeyName is the private-key name mitmdump loads from the confdir.
	confCAKeyName = "mitmproxy-ca.pem"
)

// SeedPinnedCA copies the pinned CA keypair from the read-only seed mount into
// the writable confDir and returns the pinned certificate's SHA-256
// fingerprint (lowercase hex over the DER). A missing or unloadable pinned CA
// is an error: the caller fails the MITM addon closed — it never falls back to
// generation. uid/gid receive ownership of the confdir and the copied files so
// the unprivileged mitmdump user can read them.
func SeedPinnedCA(seedDir, confDir string, uid, gid int) (string, error) {
	seedDir = strings.TrimSpace(seedDir)
	if seedDir == "" {
		return "", fmt.Errorf("pinned CA seed mount is empty (set %s to the read-only secret mount)", constants.EnvMitmCaSeedDir)
	}
	certPEM, err := os.ReadFile(filepath.Join(seedDir, SeedCACertName))
	if err != nil {
		return "", fmt.Errorf("read pinned CA certificate %s: %w", filepath.Join(seedDir, SeedCACertName), err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(seedDir, SeedCAKeyName))
	if err != nil {
		return "", fmt.Errorf("read pinned CA key %s: %w", filepath.Join(seedDir, SeedCAKeyName), err)
	}
	return seedPinnedCA(certPEM, keyPEM, confDir, uid, gid)
}

// seedPinnedCA is the testable core of SeedPinnedCA.
func seedPinnedCA(certPEM, keyPEM []byte, confDir string, uid, gid int) (string, error) {
	// Validate the pair BEFORE touching the confdir: an unloadable pinned CA
	// must fail the addon closed, never leave a half-seeded confdir for
	// mitmdump to silently replace with a generated CA.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return "", fmt.Errorf("pinned CA keypair (%s + %s): %w", SeedCACertName, SeedCAKeyName, err)
	}
	fp, err := pemCertFingerprint(certPEM)
	if err != nil {
		return "", fmt.Errorf("pinned CA certificate: %w", err)
	}

	// Idempotent: a confdir already holding the exact pinned pair is left
	// untouched, so restarts keep the files (and their inodes) stable.
	if samePinnedPair(confDir, certPEM, keyPEM, fp) {
		return fp, nil
	}

	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir confdir %s: %w", confDir, err)
	}
	// mitmdump runs as an unprivileged user; files written here by root must
	// be readable by it (the key is mode 0600, so ownership is not optional).
	if err := os.Chown(confDir, uid, gid); err != nil {
		return "", fmt.Errorf("chown confdir %s: %w", confDir, err)
	}
	if err := writeConfCAFile(confDir, confCAKeyName, keyPEM, 0o600, uid, gid); err != nil {
		return "", fmt.Errorf("seed %s: %w", confCAKeyName, err)
	}
	if err := writeConfCAFile(confDir, mitmCACertName, certPEM, 0o644, uid, gid); err != nil {
		return "", fmt.Errorf("seed %s: %w", mitmCACertName, err)
	}
	log.Infof("[mitmproxy] pinned CA seeded into confdir %s (fingerprint %s)", confDir, fp)
	return fp, nil
}

// samePinnedPair reports whether the confdir already materializes the pinned
// pair (cert fingerprint equal AND key bytes identical).
func samePinnedPair(confDir string, certPEM, keyPEM []byte, fp string) bool {
	existingCert, err := os.ReadFile(filepath.Join(confDir, mitmCACertName))
	if err != nil {
		return false
	}
	existingKey, err := os.ReadFile(filepath.Join(confDir, confCAKeyName))
	if err != nil {
		return false
	}
	if !bytes.Equal(existingKey, keyPEM) {
		return false
	}
	existingFP, err := pemCertFingerprint(existingCert)
	return err == nil && existingFP == fp
}

// ConfdirCAFingerprint returns the SHA-256 fingerprint of the certificate in
// the confdir — the CA mitmdump actually loaded. The fast-sandbox startup
// consistency check (superseded SyncRootCAFastSandbox export) compares it
// against the pinned fingerprint.
func ConfdirCAFingerprint(confDir string) (string, error) {
	certPEM, err := os.ReadFile(filepath.Join(confDir, mitmCACertName))
	if err != nil {
		return "", fmt.Errorf("read confdir CA certificate %s: %w", filepath.Join(confDir, mitmCACertName), err)
	}
	fp, err := pemCertFingerprint(certPEM)
	if err != nil {
		return "", fmt.Errorf("confdir CA certificate %s: %w", filepath.Join(confDir, mitmCACertName), err)
	}
	return fp, nil
}

// pemCertFingerprint returns the lowercase hex SHA-256 fingerprint of the
// first certificate in a PEM bundle.
func pemCertFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}

// writeConfCAFile atomically materializes one confdir file (temp + rename so
// a reader never observes a partial file).
func writeConfCAFile(dir, name string, data []byte, mode os.FileMode, uid, gid int) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chown(tmpPath, uid, gid); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(dir, name))
}
