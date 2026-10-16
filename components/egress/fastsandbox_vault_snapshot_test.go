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

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// Vault snapshot transfer across pause/resume (OSEP-0022 "Vault Lifecycle").
// The driver side (pause/resume hooks, snapshot manifest) is platform work —
// these tests drive the egress hooks directly through the action protocol.

const (
	snapshotTestUID  = "snapshot-u"
	snapshotTestIP   = "10.9.1.5"
	snapshotResumeR  = "runtime-2"
	snapshotResumeA  = "attachment-9"
	bundleTestOldRev = 3
)

// snapshotBody builds a VAULT_EXPORT / VAULT_IMPORT envelope.
func snapshotBody(t *testing.T, uid, runtimeID, attID, op, path string) string {
	t.Helper()
	env := actionhandler.Envelope{
		APIVersion:   constants.ActionsAPIVersion,
		Operation:    actionhandler.Operation(op),
		InvocationID: fmt.Sprintf("inv-%s-snapshot", uid),
		Sandbox:      actionhandler.SandboxRef{UID: uid, Name: "sb", Namespace: "ns"},
		Revision: actionhandler.Revision{
			SpecGeneration:    1,
			RuntimeInstanceID: runtimeID,
			AttachmentID:      attID,
			RouteGeneration:   1,
		},
		Snapshot: &actionhandler.SnapshotTransfer{Path: path},
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return string(raw)
}

// fakeForPath returns (creating if needed) the fake file store the harness
// hands out for an arbitrary path — vault files and bundle staging files
// share the newFile seam.
func (h *vaultPersistHarness) fakeForPath(path string) *fakeVaultFile {
	h.mapMu.Lock()
	defer h.mapMu.Unlock()
	f, ok := h.files[path]
	if !ok {
		f = newFakeVaultFile(path)
		h.files[path] = f
	}
	return f
}

// bundleVaultBody renders the canonical bundle body for the shared test
// credential vault (the exact shape a relay write would persist).
func bundleVaultBody(t *testing.T) credentialvault.VaultFileBody {
	t.Helper()
	var req credentialvault.CreateRequest
	require.NoError(t, json.Unmarshal([]byte(vaultCredential()), &req))
	return credentialvault.VaultFileBody{Credentials: req.Credentials, Bindings: req.Bindings}
}

// seedBundle writes simulated committed bundle bytes carrying the OLD runtime
// fence — what an export on the previous Pod left behind.
func (h *vaultPersistHarness) seedBundle(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := credentialvault.EncodeVaultFile(testFenceR, testFenceA, bundleTestOldRev, bundleVaultBody(t))
	require.NoError(t, err)
	f := h.fakeForPath(path)
	f.data = raw
	f.exists = true
	return raw
}

// snapshotSubject drives the shared test subject to active with a vault
// revision 1 persisted.
func (h *vaultPersistHarness) snapshotSubject(t *testing.T) subject.Subject {
	t.Helper()
	s := subject.FromSandboxUID(snapshotTestUID)
	rec := doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodPost, snapshotTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	return s
}

// ---------------------------------------------------------------------------
// Export: copy + seal
// ---------------------------------------------------------------------------

func TestVaultSnapshotExportCopiesAndSeals(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// The bundle copy is byte-identical to the subject's vault file.
	bundle := h.fakeForPath(bundlePath)
	vaultFile := h.fileFor(snapshotTestUID)
	require.True(t, bundle.exists, "export must publish the bundle file")
	require.True(t, vaultFile.exists)
	assert.Equal(t, vaultFile.data, bundle.data)

	// Sealed: every vault op is a 409 — the bundle copy is already taken.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doVault(t, h.srv, method, snapshotTestUID, vaultCredential(), "")
		assert.Equal(t, http.StatusConflict, rec.Code, method)
	}
	// Policy enforcement is unchanged while the sandbox is paused.
	state, ok := h.reg.Get(subject.FromSandboxUID(snapshotTestUID))
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)

	// A retried export (lost ack) re-copies and re-seals idempotently.
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, vaultFile.data, h.fakeForPath(bundlePath).data)
}

func TestVaultSnapshotExportWithoutVaultFileStillSeals(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.readySubject(t, snapshotTestUID, snapshotTestIP)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, h.fakeForPath(bundlePath).exists, "nothing to carry: no vault file, no bundle write")
	assert.Equal(t, http.StatusConflict, doVault(t, h.srv, http.MethodPost, snapshotTestUID, vaultCredential(), "").Code)
}

func TestVaultSnapshotExportFailureStaysActive(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	// The bundle target rejects the copy (pre-rename failure).
	bundle := h.fakeForPath(bundlePath)
	bundle.saveErr = fmt.Errorf("disk on fire")
	bundle.saveState = policy.FileUnchanged

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	// The subject stays active and unsealed: the pause proceeds without the
	// vault (surfaced to the driver; fail-closed at restore by the missing
	// bundle file), and live vault ops keep working.
	assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
	rec = doVault(t, h.srv, http.MethodPost, snapshotTestUID, vaultCredential(), "")
	assert.Equal(t, http.StatusConflict, rec.Code, "POST on an existing vault is 409 (create-only), proving the surface still serves")
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestVaultSnapshotExportGuards(t *testing.T) {
	t.Run("persistence disabled", func(t *testing.T) {
		srv, reg, _ := fastSandboxTestServer(t)
		setBindingAndReady(t, srv, reg, snapshotTestUID, snapshotTestIP, vaultAllowPolicy())
		rec := doAction(t, srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", "/tmp/bundle.json"))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		rec = doAction(t, srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_IMPORT", "/tmp/bundle.json"))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
	t.Run("unknown subject", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		rec := doAction(t, h.srv, snapshotBody(t, "ghost", testFenceR, testFenceA, "VAULT_EXPORT", "/tmp/bundle.json"))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("stale fence", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		h.snapshotSubject(t)
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, "old-runtime", testFenceA, "VAULT_EXPORT", "/tmp/bundle.json"))
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.False(t, h.srv.vaultSealedLocked(subject.FromSandboxUID(snapshotTestUID)))
	})
}

// ---------------------------------------------------------------------------
// Import: re-stamp + commit protocol
// ---------------------------------------------------------------------------

func TestVaultSnapshotImportLandsFileBeforeRegistration(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	oldBytes := h.seedBundle(t, bundlePath)

	// Resume on a fresh Pod: no subject registered yet; the driver imports
	// before SET_BINDING.
	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// Landed via the commit protocol, re-stamped with the restored fence —
	// the only authorized incarnation transfer.
	vf, body := decodeFile(t, h.fileFor(snapshotTestUID))
	assert.Equal(t, 1, vf.Version)
	assert.Equal(t, snapshotResumeR, vf.Incarnation)
	assert.Equal(t, snapshotResumeA, vf.AttachmentID)
	assert.Equal(t, int64(bundleTestOldRev), vf.Revision)
	require.Len(t, body.Credentials, 1)
	require.Len(t, body.Bindings, 1)
	assert.Equal(t, oldBytes, h.fakeForPath(bundlePath).data, "the bundle source is read, never consumed")

	// No memory slice before registration (the reload happens at
	// SET_BINDING, fenced by the same identity).
	srv := h.srv
	srv.mu.Lock()
	_, live := srv.vaults[subject.FromSandboxUID(snapshotTestUID)]
	srv.mu.Unlock()
	assert.False(t, live)

	// At registration the file reloads under the restored fence; the vault
	// is held (served to ops) from then on and activates with policy at the
	// data-plane-ready barrier.
	rec = doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, snapshotResumeR, snapshotResumeA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), fmt.Sprintf(`"revision":%d`, bundleTestOldRev))
	rec = doAction(t, h.srv, hookBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	state, ok := h.reg.Get(subject.FromSandboxUID(snapshotTestUID))
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestVaultSnapshotImportAfterSetBindingInstallsMemory(t *testing.T) {
	h := newVaultPersistHarness(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	h.seedBundle(t, bundlePath)

	// Import while denying (after SET_BINDING, before data-plane-ready).
	rec := doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, snapshotResumeR, snapshotResumeA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// The memory slice is installed immediately (validated against the
	// pending policy) and serves ops while the subject is held at the
	// barrier.
	rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), fmt.Sprintf(`"revision":%d`, bundleTestOldRev))

	// A relay write on top lands the successor revision (not sealed).
	rec = doVault(t, h.srv, http.MethodPut, snapshotTestUID, vaultPut(fmt.Sprint(bundleTestOldRev)), "")
	require.Equal(t, http.StatusOK, rec.Code)
	vf, _ := decodeFile(t, h.fileFor(snapshotTestUID))
	assert.Equal(t, snapshotResumeR, vf.Incarnation)
	assert.Equal(t, int64(bundleTestOldRev+1), vf.Revision)
}

func TestVaultSnapshotImportRejects(t *testing.T) {
	newHarness := func(t *testing.T) (*vaultPersistHarness, string) {
		h := newVaultPersistHarness(t)
		return h, filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	}
	t.Run("missing bundle file", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("corrupt bundle", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		f := h.fakeForPath(bundlePath)
		f.data = []byte(`{"v":1,`)
		f.exists = true
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("newer file version", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		f := h.fakeForPath(bundlePath)
		f.data = []byte(`{"v":99,"incarnation":"old","attachmentId":"old","revision":1,"vault":{"credentials":[],"bindings":[]}}` + "\n")
		f.exists = true
		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("stale fence while registered", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		h.seedBundle(t, bundlePath)
		rec := doAction(t, h.srv, bindBody(t, snapshotTestUID, snapshotTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
		require.Equal(t, http.StatusOK, rec.Code)
		rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusConflict, rec.Code)
	})
	t.Run("bundle rejected by policy keeps last-good", func(t *testing.T) {
		h, bundlePath := newHarness(t)
		// A live vault (revision 1) under the current policy.
		h.snapshotSubject(t)
		// A bundle whose binding host the policy does not allow.
		denied := credentialvault.VaultFileBody{
			Credentials: bundleVaultBody(t).Credentials,
			Bindings: []credentialvault.Binding{{
				Name:  "other",
				Match: credentialvault.Match{Hosts: []string{"denied.example.com"}},
				Auth:  credentialvault.Auth{Type: "bearer", Credential: "tok"},
			}},
		}
		raw, err := credentialvault.EncodeVaultFile(testFenceR, testFenceA, bundleTestOldRev, denied)
		require.NoError(t, err)
		f := h.fakeForPath(bundlePath)
		f.data = raw
		f.exists = true

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusBadRequest, rec.Code)

		// Last-good on both sides: the live vault and the file keep revision 1.
		vf, _ := decodeFile(t, h.fileFor(snapshotTestUID))
		assert.Equal(t, int64(1), vf.Revision)
		rec = doVault(t, h.srv, http.MethodGet, snapshotTestUID, "", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"revision":1`)
		assert.False(t, h.srv.vaultRecoveryActive(), "a pre-rename NACK never latches recovery")
	})
}

func TestVaultSnapshotImportCommitProtocolOutcomes(t *testing.T) {
	t.Run("uncertain persist latches recovery", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		h.seedBundle(t, bundlePath)
		h.fileFor(snapshotTestUID).saveState = policy.FileUnknown
		h.fileFor(snapshotTestUID).saveErr = fmt.Errorf("rename lied")

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.True(t, h.srv.vaultRecoveryActive(), "an uncertain outcome freezes vault writes until restart")
	})
	t.Run("pre-rename failure nacks without latch", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		h.seedBundle(t, bundlePath)
		h.fileFor(snapshotTestUID).saveState = policy.FileUnchanged
		h.fileFor(snapshotTestUID).saveErr = fmt.Errorf("temp create failed")

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.False(t, h.srv.vaultRecoveryActive())
		assert.False(t, h.fileFor(snapshotTestUID).exists, "a rejected import lands nothing")
	})
	t.Run("latch freezes unregistered imports too", func(t *testing.T) {
		h := newVaultPersistHarness(t)
		bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
		h.seedBundle(t, bundlePath)
		// An earlier subject's uncertain outcome latched recovery.
		h.srv.requireVaultRecoveryLocked("simulated latch")

		rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.False(t, h.fileFor(snapshotTestUID).exists, "no vault file mutation while latched")
	})
}

// ---------------------------------------------------------------------------
// REMOVE_BINDING on a sealed subject
// ---------------------------------------------------------------------------

func TestVaultSnapshotRemoveBindingPurgesSealed(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.snapshotSubject(t)
	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")

	rec := doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, h.fakeForPath(bundlePath).exists, "precondition: the bundle copy is intact")

	rec = doAction(t, h.srv, removeBody(t, snapshotTestUID, testFenceR, testFenceA))
	require.Equal(t, http.StatusOK, rec.Code)

	// Terminal purge: memory + file gone; every vault op 404s. The bundle
	// copy survives (export strictly precedes the teardown).
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doVault(t, h.srv, method, snapshotTestUID, vaultCredential(), "")
		assert.Equal(t, http.StatusNotFound, rec.Code, method)
	}
	assert.False(t, h.fileFor(snapshotTestUID).exists, "REMOVE_BINDING purges the vault file")
	assert.True(t, h.fakeForPath(bundlePath).exists, "the bundle copy is untouched by the teardown")
}
