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

if TYPE_CHECKING:
    from ..models.diagnostic_backend_identifiers import DiagnosticBackendIdentifiers


T = TypeVar("T", bound="DiagnosticBackend")


@_attrs_define
class DiagnosticBackend:
    """Runtime backend that currently owns the sandbox.

    Attributes:
        type_ (str): Implementation-defined backend type.
        identifiers (DiagnosticBackendIdentifiers): Backend-defined identifiers used to correlate this sandbox with
            backend resources. Keys are scoped to `type`; values are strings.
    """

    type_: str
    identifiers: DiagnosticBackendIdentifiers

    def to_dict(self) -> dict[str, Any]:
        type_ = self.type_

        identifiers = self.identifiers.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "type": type_,
                "identifiers": identifiers,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.diagnostic_backend_identifiers import DiagnosticBackendIdentifiers

        d = dict(src_dict)
        type_ = d.pop("type")

        identifiers = DiagnosticBackendIdentifiers.from_dict(d.pop("identifiers"))

        diagnostic_backend = cls(
            type_=type_,
            identifiers=identifiers,
        )

        return diagnostic_backend
