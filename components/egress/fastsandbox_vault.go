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

// Relay-only vault surface (OSEP-0022): /vault/{sandboxUid} terminates the
// FastPath handler-relay (server → FastPath → Fastlet → this loopback
// listener); the Fastlet's relay hop is the only caller. Internal, versioned
// contract — specs/egress-api.yaml (sidecar) is unchanged.
//
// Sidecar-parity semantics: POST create-only (409 on exists), PUT full
// replace with optional expectedRevision (412 on mismatch, creates when
// absent), GET the full revision INCLUDING values (server-side PATCH merges
// against it), DELETE keeps the policy. Unregistered subject → 404, never
// cached; expectedIncarnation mismatch → 412; validation failure → 400 NACK
// keeping last-good. Shares the per-subject store with the mitm addon's
// active-vault socket.
package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// validVaultSubjectID keeps the path-derived UID opaque: bounded, no
// separators, dot-segments, or control characters — it names the subject's
// vault file once persistence lands.
func validVaultSubjectID(uid string) bool {
	if uid == "" || len(uid) > 128 || uid == "." || uid == ".." {
		return false
	}
	for _, r := range uid {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// handleVaultRelay serves /vault/{sandboxUid} for all four relay verbs.
func (s *fastSandboxPolicyServer) handleVaultRelay(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.URL.Path, constants.VaultRelayPathPrefix)
	if !validVaultSubjectID(uid) {
		http.Error(w, "missing or invalid sandbox uid in path", http.StatusBadRequest)
		return
	}
	subj := subject.FromSandboxUID(uid)
	vault, status, msg := s.selectRelayVault(r, subj)
	if vault == nil {
		http.Error(w, msg, status)
		return
	}
	pol := s.vaultValidationPolicy(subj)
	switch r.Method {
	case http.MethodGet:
		full, err := vault.Full()
		if err != nil {
			writeVaultRelayError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, full)
	case http.MethodPost:
		var req credentialvault.CreateRequest
		if err := credentialvault.ReadJSON(r, &req); err != nil {
			http.Error(w, "invalid credential vault request: "+err.Error(), http.StatusBadRequest)
			return
		}
		state, err := vault.Create(req, pol)
		if err != nil {
			writeVaultRelayError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, state)
	case http.MethodPut:
		var req credentialvault.ReplaceRequest
		if err := credentialvault.ReadJSON(r, &req); err != nil {
			http.Error(w, "invalid credential vault replace request: "+err.Error(), http.StatusBadRequest)
			return
		}
		state, created, err := vault.Replace(req, pol)
		if err != nil {
			writeVaultRelayError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, state)
	case http.MethodDelete:
		if err := vault.Delete(); err != nil {
			writeVaultRelayError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// selectRelayVault decides registration, the expectedIncarnation
// precondition, and the fence-bound store selection under one s.mu hold
// (see vaultForLocked). A concurrent rebind then either purges before
// selection — the stale incarnation mismatches the successor's fresh fence —
// or purges after — the stale request holds the old store, which the purge
// discards. Either way a stale writer never reaches the successor's store.
// A nil store means the caller must write (status, msg).
func (s *fastSandboxPolicyServer) selectRelayVault(r *http.Request, subj subject.Subject) (*credentialvault.Store, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fence, ok := s.reg.Fence(subj)
	if !ok {
		return nil, http.StatusNotFound, "unknown subject"
	}
	if inc := strings.TrimSpace(r.Header.Get(constants.EgressVaultIncarnationHeader)); inc != "" && inc != fence.RuntimeInstanceID {
		return nil, http.StatusPreconditionFailed, "expectedIncarnation mismatch"
	}
	return s.vaultForLocked(subj, fence), 0, ""
}

// vaultValidationPolicy returns the policy a vault op validates against: the
// effective policy of an active subject, or — while the subject is still
// denying — its pending binding policy (the op is held to the
// data-plane-ready barrier, so validation must not depend on hook timing).
func (s *fastSandboxPolicyServer) vaultValidationPolicy(subj subject.Subject) *policy.NetworkPolicy {
	if pol := s.reg.EffectivePolicy(subj); pol != nil {
		return pol
	}
	if pending := s.pendingPolicy(subj); pending != nil {
		return s.reg.EffectiveOf(pending)
	}
	return nil
}

// writeVaultRelayError maps store errors onto the relay contract: revision
// conflicts are 412 (sidecar PreconditionFailed parity), unlike the sidecar
// PATCH mapper which keeps 409.
func writeVaultRelayError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, credentialvault.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, credentialvault.ErrExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, credentialvault.ErrRevisionConflict):
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
