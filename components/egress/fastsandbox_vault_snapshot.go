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

// Vault snapshot transfer across pause/resume (OSEP-0022 "Vault Lifecycle"):
// the driver invokes VAULT_EXPORT inside the pause flow — strictly before the
// runtime teardown, because REMOVE_BINDING purges the local file — and
// VAULT_IMPORT at restore, before data-plane-ready, possibly on a different
// Pod. Export copies vault/<sandboxUid>.json into the bundle and seals the
// subject (vault ops 409; policy enforcement unchanged); the copy runs
// WITHOUT the server lock (a slow or stuck driver-owned target must never
// block the shared data plane) and is sealed only after re-checking under
// the lock that the source is still exactly the copied image — a concurrent
// relay write abandons the seal, and the recovery latch (uncertain on-disk
// state) rejects the export outright so uncertain bytes can never become the
// only surviving copy. Import validates the bundle file (format v: 1,
// size-capped), re-stamps it with the restored runtime fence — the only
// authorized incarnation transfer — and lands it through the same commit
// protocol as the relay writes, so an interrupted import cannot half-land;
// a local revision newer than the bundle means relay writes landed after the
// first import, so the retry acks idempotently instead of rolling back. The
// vault activates at the same data-plane-ready barrier as policy. The
// invocation side (driver/Fastlet hooks, snapshot-manifest entry) is platform
// work: until it lands this slice is exercised by unit tests.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// maxVaultSnapshotBytes bounds a bundle file read: a vault file is a
// relay-body-sized artifact, so anything over the actions body limit is not
// a v1 vault file. The cap keeps a huge or growing driver-provided source
// from being pulled into memory unbounded.
const maxVaultSnapshotBytes = maxPolicyBodyBytes

// errVaultSnapshotTooLarge marks a bundle over maxVaultSnapshotBytes —
// permanently invalid for this protocol version.
var errVaultSnapshotTooLarge = fmt.Errorf("vault bundle file exceeds %d bytes", maxVaultSnapshotBytes)

// reservesPath reports whether path falls inside the vault persistence
// directory (or is the directory itself). That subtree is reserved for the
// per-subject vault files: driver-provided snapshot paths must never land
// there, or an export target could clobber another subject's vault file and
// an import source could read one subject's vault into another's.
func (p *vaultPersistence) reservesPath(path string) bool {
	rel, err := filepath.Rel(filepath.Clean(p.dir), filepath.Clean(path))
	if err != nil {
		// Uncomparable — fail closed.
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

// applyVaultExport copies the subject's vault file into the bundle target
// provided by the driver and marks the subject sealed. A successful export of
// a subject without a vault file just seals (nothing to carry). Any failure —
// unreadable source, unwritable target, source changed mid-copy — leaves the
// subject unsealed and active (the pause proceeds without the vault; the
// driver surfaces it, and a missing bundle file fails closed at restore).
// Repeated export (a retry after a lost ack) re-copies and re-seals: while
// sealed no vault op can change the source, so the copy stays current.
//
// The copy runs in three phases. Phase 1 takes the guards and a consistent
// image of the source under s.mu; phase 2 writes the driver-owned target
// WITHOUT the lock (it may be slow or stuck — every action, vault op, and
// active-vault resolution goes through s.mu); phase 3 re-checks under the
// lock and seals only if the source is still exactly the copied image. A
// relay write that lands in between makes the copy stale: 409, unsealed, the
// driver retries against the newer state. The recovery latch rejects the
// export in both guarded phases — with on-disk state unconfirmed, the bundle
// copy plus the teardown purge could otherwise make uncertain bytes the only
// surviving vault content, invisibly.
func (s *fastSandboxPolicyServer) applyVaultExport(env *actionhandler.Envelope) (int, error) {
	if s.vaultFiles == nil {
		return http.StatusInternalServerError, errors.New("vault export requires vault persistence (OPENSANDBOX_EGRESS_VAULT_DIR)")
	}
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	target := strings.TrimSpace(env.Snapshot.Path)
	if s.vaultFiles.reservesPath(target) {
		return http.StatusBadRequest, fmt.Errorf("vault bundle target %q must not live under the vault persistence directory", target)
	}

	// Phase 1: guards + consistent source image.
	s.mu.Lock()
	snap, status, err := s.exportSourceImageLocked(subj, fence)
	if err != nil {
		s.mu.Unlock()
		return status, err
	}
	s.mu.Unlock()

	// Phase 2: no lock held across the driver-owned write.
	if snap.Exists() {
		bundle, err := s.vaultFiles.newFile(target)
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("vault bundle target unavailable: %w", err)
		}
		state, err := bundle.SaveRaw(snap.Data())
		if err != nil || state != policy.FileCommitted {
			return http.StatusInternalServerError, fmt.Errorf("vault bundle copy failed (state %v): %v", state, err)
		}
	}

	// Phase 3: seal only against the exact copied image.
	s.mu.Lock()
	defer s.mu.Unlock()
	live, status, err := s.exportSourceImageLocked(subj, fence)
	if err != nil {
		return status, err
	}
	if live.Exists() != snap.Exists() || (snap.Exists() && !bytes.Equal(live.Data(), snap.Data())) {
		return http.StatusConflict, fmt.Errorf("vault file changed during export for subject %s; bundle copy not sealed", subj)
	}
	s.vaultForLocked(subj, fence)
	e := s.vaults[subj]
	e.sealed = true
	s.vaults[subj] = e
	if snap.Exists() {
		log.Infof("subject %s: vault exported to snapshot bundle, subject sealed", subj)
	} else {
		log.Infof("subject %s: no vault file to export, subject sealed", subj)
	}
	return http.StatusOK, nil
}

// exportSourceImageLocked runs the export guards and returns a consistent
// image of the subject's vault file. Callers hold s.mu.
func (s *fastSandboxPolicyServer) exportSourceImageLocked(subj subject.Subject, fence subject.Fencing) (*policy.PolicyFileSnapshot, int, error) {
	regFence, ok := s.reg.Fence(subj)
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("vault export for unknown subject %s", subj)
	}
	if !regFence.Matches(fence) {
		return nil, http.StatusConflict, fmt.Errorf("stale vault export (fence mismatch) for subject %s", subj)
	}
	if recovery := s.vaultRecovery.Load(); recovery != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("vault persistence recovery required: %s", recovery.reason)
	}
	fs, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("vault file store unavailable: %w", err)
	}
	snap, err := fs.Snapshot()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("vault file snapshot: %w", err)
	}
	return snap, 0, nil
}

// applyVaultImport lands the bundle file as the subject's vault file,
// re-stamped with the restored runtime fence from the envelope. Restore may
// land on any Pod, so unlike every other path the recorded incarnation here
// is replaced, not compared. The file goes through the commit protocol
// (persist → apply → ack), so the ack means durable AND applied; the vault
// activates at the same data-plane-ready barrier as policy.
//
// Delivery is at-least-once: before any durable effect the local vault file
// is compared under the lock, and a local revision newer than the bundle acks
// idempotently — relay writes that landed after the first import (or an
// out-of-order import) must never be rolled back. When the subject is not
// (yet) registered — import before SET_BINDING — the guard runs against the
// file alone; the registration-time reload installs and validates the memory
// slice against the real binding policy. When the subject is already
// registered under the envelope fence (import after SET_BINDING, before
// data-plane-ready), the content is additionally validated against the
// current validation policy on a scratch store before anything durable
// happens, and a rejected bundle answers 400 with last-good kept.
func (s *fastSandboxPolicyServer) applyVaultImport(env *actionhandler.Envelope) (int, error) {
	if s.vaultFiles == nil {
		return http.StatusInternalServerError, errors.New("vault import requires vault persistence (OPENSANDBOX_EGRESS_VAULT_DIR)")
	}
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	source := strings.TrimSpace(env.Snapshot.Path)
	if s.vaultFiles.reservesPath(source) {
		return http.StatusBadRequest, fmt.Errorf("vault bundle source %q must not live under the vault persistence directory", source)
	}

	// Read + validate the bundle before locking (a stalled or huge source
	// must never hold s.mu). A missing, oversized, or unreadable source is
	// permanent: the fastlet retry cannot repair it.
	data, err := readVaultBundle(source)
	if errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound, fmt.Errorf("vault bundle file %q does not exist", source)
	}
	if errors.Is(err, errVaultSnapshotTooLarge) {
		return http.StatusBadRequest, err
	}
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault bundle read failed: %w", err)
	}
	vf, body, err := credentialvault.DecodeVaultFile(data)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid vault bundle file: %v", err)
	}
	// The authorized incarnation transfer: re-stamp the full runtime fence.
	payload, err := credentialvault.EncodeVaultFile(fence.RuntimeInstanceID, fence.AttachmentID, vf.Revision, body)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault bundle re-stamp failed: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// The recovery latch freezes every vault file mutation, import included:
	// only a restart reconciles the committed files (relay-write parity).
	if recovery := s.vaultRecovery.Load(); recovery != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("vault persistence recovery required: %s", recovery.reason)
	}
	// At-least-once delivery guard (see above): never roll a newer local
	// revision back to the bundle.
	if fs, fsErr := s.vaultFiles.storeFor(subj); fsErr == nil {
		if cur, snapErr := fs.Snapshot(); snapErr == nil && cur.Exists() {
			if curVF, _, decErr := credentialvault.DecodeVaultFile(cur.Data()); decErr == nil && curVF.Revision > vf.Revision {
				log.Infof("subject %s: vault import superseded (local revision %d > bundle revision %d), idempotent ack", subj, curVF.Revision, vf.Revision)
				return http.StatusOK, nil
			}
		}
	}
	regFence, registered := s.reg.Fence(subj)
	if registered {
		if !regFence.Matches(fence) {
			return http.StatusConflict, fmt.Errorf("stale vault import (fence mismatch) for subject %s", subj)
		}
		// Validate before any durable effect: a bundle the current validation
		// policy rejects — including "bindings require an egress policy" for
		// a policy-less subject — is permanently broken. NACK with last-good
		// kept, never half-open.
		if err := validateImportedVault(vf, body, s.vaultValidationPolicyLocked(subj)); err != nil {
			return http.StatusBadRequest, fmt.Errorf("vault bundle rejected: %v", err)
		}
	}
	err = s.persistVaultCandidateLocked(
		subj,
		func(fs vaultFileStore) (policy.FileMutationState, error) {
			return fs.SaveRaw(payload)
		},
		func() error {
			if !registered {
				// Import before SET_BINDING: the registration-time reload
				// (rehydrateVaultReload) installs and validates the slice.
				return nil
			}
			return s.installImportedVaultLocked(subj, fence, vf, body)
		},
	)
	if err != nil {
		log.Errorf("subject %s: vault import failed: %v", subj, err)
		status := http.StatusInternalServerError
		if recovery := s.vaultRecovery.Load(); recovery != nil {
			status = http.StatusServiceUnavailable
		}
		return status, errors.New("vault import persistence failure")
	}
	if registered {
		log.Infof("subject %s: vault imported from snapshot bundle (revision %d, re-stamped to %s/%s)",
			subj, vf.Revision, fence.RuntimeInstanceID, fence.AttachmentID)
	} else {
		log.Infof("subject %s: vault bundle landed for the next registration (revision %d, re-stamped to %s/%s)",
			subj, vf.Revision, fence.RuntimeInstanceID, fence.AttachmentID)
	}
	return http.StatusOK, nil
}

// installImportedVaultLocked binds the imported revision into the live
// per-subject slice under the restored fence, replacing any stale entry.
// Callers hold s.mu. The slice is only replaced once Reload has accepted the
// content — a failure here leaves the live entry untouched, and the commit
// protocol rolls the file back (last-good kept on both sides).
func (s *fastSandboxPolicyServer) installImportedVaultLocked(subj subject.Subject, fence subject.Fencing, vf credentialvault.VaultFile, body credentialvault.VaultFileBody) error {
	pol := s.vaultValidationPolicyLocked(subj)
	entry, ok := s.vaults[subj]
	if !ok || entry.fence != fence {
		entry = vaultEntry{store: credentialvault.NewStore(nil, func() bool { return true }), fence: fence}
	}
	if err := entry.store.Reload(vf.Revision, credentialvault.CreateRequest(body), pol); err != nil {
		return err
	}
	entry.sealed = false
	s.vaults[subj] = entry
	return nil
}

// validateImportedVault runs the full validation pipeline for the imported
// revision on a scratch store — the exact Reload the live install will run,
// so a bundle accepted here cannot fail the apply.
func validateImportedVault(vf credentialvault.VaultFile, body credentialvault.VaultFileBody, pol *policy.NetworkPolicy) error {
	scratch := credentialvault.NewStore(nil, func() bool { return true })
	return scratch.Reload(vf.Revision, credentialvault.CreateRequest(body), pol)
}

// readVaultBundle reads the driver-provided bundle source with a hard size
// cap. The source lives in the driver's staging area (a read-only lookup in
// the same trust domain — the write paths keep the hardened atomic
// machinery), so plain bounded I/O is sufficient here; what matters is that
// a huge or growing file can never be pulled into memory unbounded.
func readVaultBundle(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxVaultSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxVaultSnapshotBytes {
		return nil, errVaultSnapshotTooLarge
	}
	return data, nil
}
