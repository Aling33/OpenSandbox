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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

// testCA materializes one self-signed CA keypair as PEM bytes, mirroring the
// pinned-CA Secret entries on the seed mount.
type testCA struct {
	certPEM []byte
	keyPEM  []byte
}

func generateTestCA(t *testing.T, cn string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return testCA{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

// writeSeedDir writes a seed mount layout: tls.crt + tls.key.
func writeSeedDir(t *testing.T, ca testCA) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, SeedCACertName), ca.certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, SeedCAKeyName), ca.keyPEM, 0o600))
	return dir
}

func TestSeedPinnedCAMaterializesConfdirPair(t *testing.T) {
	ca := generateTestCA(t, "pinned-ca")
	seedDir := writeSeedDir(t, ca)
	confDir := filepath.Join(t.TempDir(), ".mitmproxy")

	fp, err := SeedPinnedCA(seedDir, confDir, os.Getuid(), os.Getgid())
	require.NoError(t, err)

	// Fingerprint must equal the SHA-256 over the certificate DER.
	require.Equal(t, ca.fingerprint(t), fp)

	// The confdir pair materialized under the names mitmdump loads.
	cert, err := os.ReadFile(filepath.Join(confDir, mitmCACertName))
	require.NoError(t, err)
	require.Equal(t, string(ca.certPEM), string(cert))
	key, err := os.ReadFile(filepath.Join(confDir, confCAKeyName))
	require.NoError(t, err)
	require.Equal(t, string(ca.keyPEM), string(key))

	// Private key mode 0600; ownership handed to the mitmdump user.
	keyInfo, err := os.Stat(filepath.Join(confDir, confCAKeyName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), keyInfo.Mode().Perm())
	if stat, ok := keyInfo.Sys().(*syscall.Stat_t); ok {
		require.EqualValues(t, os.Getuid(), stat.Uid)
		require.EqualValues(t, os.Getgid(), stat.Gid)
	}
	certInfo, err := os.Stat(filepath.Join(confDir, mitmCACertName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), certInfo.Mode().Perm())
}

func TestSeedPinnedCAIdempotentRestartStable(t *testing.T) {
	ca := generateTestCA(t, "pinned-ca")
	seedDir := writeSeedDir(t, ca)
	confDir := filepath.Join(t.TempDir(), ".mitmproxy")

	fp1, err := SeedPinnedCA(seedDir, confDir, os.Getuid(), os.Getgid())
	require.NoError(t, err)

	keyPath := filepath.Join(confDir, confCAKeyName)
	before, err := os.Stat(keyPath)
	require.NoError(t, err)

	fp2, err := SeedPinnedCA(seedDir, confDir, os.Getuid(), os.Getgid())
	require.NoError(t, err)
	require.Equal(t, fp1, fp2)

	// A restart over an already-seeded confdir must not rewrite the files.
	after, err := os.Stat(keyPath)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "idempotent seed must leave the materialized key untouched")
}

func TestSeedPinnedCAReplacesForeignConfdirCA(t *testing.T) {
	ca := generateTestCA(t, "pinned-ca")
	seedDir := writeSeedDir(t, ca)
	confDir := filepath.Join(t.TempDir(), ".mitmproxy")
	require.NoError(t, os.MkdirAll(confDir, 0o755))

	// A previously generated (foreign) CA occupies the confdir; the pinned
	// copy must win — mitmdump would otherwise load the wrong keypair.
	foreign := generateTestCA(t, "generated-ca")
	require.NoError(t, os.WriteFile(filepath.Join(confDir, mitmCACertName), foreign.certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(confDir, confCAKeyName), foreign.keyPEM, 0o600))

	fp, err := SeedPinnedCA(seedDir, confDir, os.Getuid(), os.Getgid())
	require.NoError(t, err)
	require.Equal(t, ca.fingerprint(t), fp)

	got, err := ConfdirCAFingerprint(confDir)
	require.NoError(t, err)
	require.Equal(t, fp, got, "confdir must hold the pinned CA after seeding")
}

func TestSeedPinnedCARejectsMissingOrUnloadableSeed(t *testing.T) {
	confDir := filepath.Join(t.TempDir(), ".mitmproxy")

	// Empty seed dir: the error must point at the env knob.
	_, err := SeedPinnedCA("", confDir, os.Getuid(), os.Getgid())
	require.ErrorContains(t, err, constants.EnvMitmCaSeedDir)

	// Missing seed mount.
	_, err = SeedPinnedCA(filepath.Join(t.TempDir(), "nope"), confDir, os.Getuid(), os.Getgid())
	require.ErrorIs(t, err, os.ErrNotExist)

	// Missing key entry (certificate-only mount: the private key must never
	// be required on the template path, only here).
	seedDir := t.TempDir()
	ca := generateTestCA(t, "pinned-ca")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, SeedCACertName), ca.certPEM, 0o644))
	_, err = SeedPinnedCA(seedDir, confDir, os.Getuid(), os.Getgid())
	require.ErrorIs(t, err, os.ErrNotExist)

	// Unloadable certificate.
	badDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(badDir, SeedCACertName), []byte("not a pem"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, SeedCAKeyName), ca.keyPEM, 0o600))
	_, err = SeedPinnedCA(badDir, confDir, os.Getuid(), os.Getgid())
	require.Error(t, err)

	// Keypair mismatch: certificate and key from different CAs.
	other := generateTestCA(t, "other-ca")
	mixedDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(mixedDir, SeedCACertName), ca.certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mixedDir, SeedCAKeyName), other.keyPEM, 0o600))
	_, err = SeedPinnedCA(mixedDir, confDir, os.Getuid(), os.Getgid())
	require.Error(t, err)

	// Nothing may be materialized on any failure path.
	_, statErr := os.Stat(confDir)
	require.True(t, os.IsNotExist(statErr), "failed seed must not create the confdir")
}

func TestConfdirCAFingerprint(t *testing.T) {
	confDir := t.TempDir()

	_, err := ConfdirCAFingerprint(confDir)
	require.Error(t, err, "missing confdir cert must error")

	ca := generateTestCA(t, "pinned-ca")
	require.NoError(t, os.WriteFile(filepath.Join(confDir, mitmCACertName), ca.certPEM, 0o644))
	fp, err := ConfdirCAFingerprint(confDir)
	require.NoError(t, err)
	require.Equal(t, ca.fingerprint(t), fp)

	require.NoError(t, os.WriteFile(filepath.Join(confDir, mitmCACertName), []byte("junk"), 0o644))
	_, err = ConfdirCAFingerprint(confDir)
	require.Error(t, err, "unloadable confdir cert must error")
}

func (ca testCA) fingerprint(t *testing.T) string {
	t.Helper()
	fp, err := pemCertFingerprint(ca.certPEM)
	require.NoError(t, err)
	return fp
}
