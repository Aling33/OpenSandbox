#
# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.diagnostic_backend import DiagnosticBackend


T = TypeVar("T", bound="DiagnosticContextResponse")


@_attrs_define
class DiagnosticContextResponse:
    """Structured context for routing sandbox diagnostics.

    Attributes:
        sandbox_id (str): Unique sandbox identifier accepted by the OpenSandbox API.
        backend (DiagnosticBackend): Runtime backend that currently owns the sandbox.
        state (str | Unset): Current OpenSandbox lifecycle state, when available.
    """

    sandbox_id: str
    backend: DiagnosticBackend
    state: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        sandbox_id = self.sandbox_id

        backend = self.backend.to_dict()

        state = self.state

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "sandboxId": sandbox_id,
                "backend": backend,
            }
        )
        if state is not UNSET:
            field_dict["state"] = state

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.diagnostic_backend import DiagnosticBackend

        d = dict(src_dict)
        sandbox_id = d.pop("sandboxId")

        backend = DiagnosticBackend.from_dict(d.pop("backend"))

        state = d.pop("state", UNSET)

        diagnostic_context_response = cls(
            sandbox_id=sandbox_id,
            backend=backend,
            state=state,
        )

        return diagnostic_context_response
