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
// subject (vault ops 409; policy enforcement unchanged). Import validates the
// bundle file (format v: 1), re-stamps it with the restored runtime fence —
// the only authorized incarnation transfer — and lands it through the same
// commit protocol as the relay writes, so an interrupted import cannot
// half-land; the vault activates at the same data-plane-ready barrier as
// policy. The invocation side (driver/Fastlet hooks, snapshot-manifest entry)
// is platform work: until it lands this slice is exercised by unit tests.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// applyVaultExport copies the subject's vault file into the bundle target
// provided by the driver and marks the subject sealed. A successful export of
// a subject without a vault file just seals (nothing to carry). Any failure —
// unreadable source, unwritable target — leaves the subject unsealed and
// active (the pause proceeds without the vault; the driver surfaces it, and a
// missing bundle file fails closed at restore). Repeated export (a retry
// after a lost ack) re-copies and re-seals: while sealed no vault op can
// change the source, so the copy stays current.
func (s *fastSandboxPolicyServer) applyVaultExport(env *actionhandler.Envelope) (int, error) {
	if s.vaultFiles == nil {
		return http.StatusInternalServerError, errors.New("vault export requires vault persistence (OPENSANDBOX_EGRESS_VAULT_DIR)")
	}
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	target := strings.TrimSpace(env.Snapshot.Path)

	s.mu.Lock()
	defer s.mu.Unlock()
	regFence, ok := s.reg.Fence(subj)
	if !ok {
		return http.StatusNotFound, fmt.Errorf("vault export for unknown subject %s", subj)
	}
	if !regFence.Matches(fence) {
		return http.StatusConflict, fmt.Errorf("stale vault export (fence mismatch) for subject %s", subj)
	}
	// A registered subject always has a slice here: export must find (or
	// create) the entry it seals. A stale entry left under a different fence
	// is replaced by vaultForLocked — the seal never lands on a predecessor
	// slice.
	s.vaultForLocked(subj, regFence)
	e := s.vaults[subj]
	fs, err := s.vaultFiles.storeFor(subj)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault file store unavailable: %w", err)
	}
	snap, err := fs.Snapshot()
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault file snapshot: %w", err)
	}
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
	e.sealed = true
	s.vaults[subj] = e
	if snap.Exists() {
		log.Infof("subject %s: vault exported to snapshot bundle, subject sealed", subj)
	} else {
		log.Infof("subject %s: no vault file to export, subject sealed", subj)
	}
	return http.StatusOK, nil
}

// applyVaultImport lands the bundle file as the subject's vault file,
// re-stamped with the restored runtime fence from the envelope. Restore may
// land on any Pod, so unlike every other path the recorded incarnation here
// is replaced, not compared. The file goes through the commit protocol
// (persist → apply → ack), so the ack means durable AND applied; the vault
// activates at the same data-plane-ready barrier as policy.
//
// Ordering: the bundle is read and decoded before the server lock (a stalled
// or huge source must never hold s.mu), then the landing holds s.mu for the
// whole transaction like every other vault writer.
//
// When the subject is not (yet) registered — import before SET_BINDING — only
// the file lands; the registration-time reload installs and validates the
// memory slice against the real binding policy. When the subject is already
// registered under the envelope fence (import after SET_BINDING, before
// data-plane-ready), the content is validated against the current validation
// policy on a scratch store before any durable effect, and a rejected bundle
// answers 400 with last-good kept on both sides.
func (s *fastSandboxPolicyServer) applyVaultImport(env *actionhandler.Envelope) (int, error) {
	if s.vaultFiles == nil {
		return http.StatusInternalServerError, errors.New("vault import requires vault persistence (OPENSANDBOX_EGRESS_VAULT_DIR)")
	}
	subj := subject.FromSandboxUID(env.Sandbox.UID)
	fence := subject.FromRevision(env)
	source := strings.TrimSpace(env.Snapshot.Path)

	// Read + validate the bundle before locking (see above). A missing or
	// unreadable source is permanent: the fastlet retry cannot repair it.
	bundle, err := s.vaultFiles.newFile(source)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault bundle source unavailable: %w", err)
	}
	snap, err := bundle.Snapshot()
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("vault bundle read failed: %w", err)
	}
	if !snap.Exists() {
		return http.StatusNotFound, fmt.Errorf("vault bundle file %q does not exist", source)
	}
	vf, body, err := credentialvault.DecodeVaultFile(snap.Data())
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
