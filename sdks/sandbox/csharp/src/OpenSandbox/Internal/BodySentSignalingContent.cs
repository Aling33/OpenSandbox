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

using System.Net;

namespace OpenSandbox.Internal;

/// <summary>
/// Wraps request content to signal when the request body has been fully written to the wire.
/// <see cref="HttpClientWrapper.SendTransferAsync"/> uses the signal to start the
/// response-header deadline of a file upload only after the body has been sent, so a slow
/// upload is not aborted by the time its own body takes.
/// </summary>
internal sealed class BodySentSignalingContent : HttpContent
{
    private readonly HttpContent _inner;
    private readonly Action _onBodySent;

    public BodySentSignalingContent(HttpContent inner, Action onBodySent)
    {
        _inner = inner ?? throw new ArgumentNullException(nameof(inner));
        _onBodySent = onBodySent ?? throw new ArgumentNullException(nameof(onBodySent));

        foreach (var header in inner.Headers)
        {
            Headers.TryAddWithoutValidation(header.Key, header.Value);
        }
    }

    protected override async Task SerializeToStreamAsync(Stream stream, TransportContext? context)
    {
#if NET5_0_OR_GREATER
        await _inner.CopyToAsync(stream, context, CancellationToken.None).ConfigureAwait(false);
#else
        await _inner.CopyToAsync(stream).ConfigureAwait(false);
#endif
        _onBodySent();
    }

    protected override bool TryComputeLength(out long length)
    {
        // HttpContent.TryComputeLength is protected and cannot be forwarded to another instance,
        // so go through the length the inner content reports in its headers; MultipartFormDataContent
        // and friends compute it when the request is prepared.
        var contentLength = _inner.Headers.ContentLength;
        length = contentLength ?? -1L;
        return contentLength.HasValue;
    }

    protected override Task<Stream> CreateContentReadStreamAsync() => _inner.ReadAsStreamAsync();

    protected override void Dispose(bool disposing)
    {
        if (disposing)
        {
            _inner.Dispose();
        }

        base.Dispose(disposing);
    }
}
