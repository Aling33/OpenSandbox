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

using System.Text;
using System.Text.Json;
using OpenSandbox.Core;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;

namespace OpenSandbox.Internal;

/// <summary>
/// Internal HTTP client wrapper for making API requests.
/// </summary>
internal sealed class HttpClientWrapper
{
    private static readonly HttpMethod PatchMethod = new("PATCH");

    private readonly HttpClient _httpClient;
    private readonly HttpClient? _transferHttpClient;
    private readonly TimeSpan? _transferHeaderTimeout;
    private readonly string _baseUrl;
    private readonly IReadOnlyDictionary<string, string> _defaultHeaders;
    private readonly ILogger _logger;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        PropertyNameCaseInsensitive = true,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingNull
    };

    public HttpClientWrapper(
        HttpClient httpClient,
        string baseUrl,
        IReadOnlyDictionary<string, string>? defaultHeaders = null,
        ILogger? logger = null,
        HttpClient? transferHttpClient = null,
        TimeSpan? transferHeaderTimeout = null)
    {
        _httpClient = httpClient ?? throw new ArgumentNullException(nameof(httpClient));
        _transferHttpClient = transferHttpClient;
        _transferHeaderTimeout = transferHeaderTimeout;
        _baseUrl = baseUrl?.TrimEnd('/') ?? throw new ArgumentNullException(nameof(baseUrl));
        _defaultHeaders = defaultHeaders ?? new Dictionary<string, string>();
        _logger = logger ?? NullLogger.Instance;
    }

    public string BaseUrl => _baseUrl;

    public HttpClientWrapper WithBaseUrl(string baseUrl) =>
        new(_httpClient, baseUrl, _defaultHeaders, _logger, _transferHttpClient, _transferHeaderTimeout);

    public async Task<T> GetAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Gets a JSON resource and also returns response headers, e.g. to read
    /// metadata such as the OPEN-SANDBOX-ORIGIN header.
    /// </summary>
    public async Task<(T Body, IReadOnlyDictionary<string, string> Headers)> GetWithHeadersAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        var headers = response.Headers.ToDictionary(h => h.Key, h => h.Value.FirstOrDefault() ?? string.Empty, StringComparer.OrdinalIgnoreCase);
        var body = await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
        return (body, headers);
    }

    public async Task GetAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> PostAsync<T>(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP POST {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Post, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task PostAsync(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP POST {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Post, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> PatchAsync<T>(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP PATCH {Url}", url);
        using var request = new HttpRequestMessage(PatchMethod, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task PatchAsync(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP PATCH {Url}", url);
        using var request = new HttpRequestMessage(PatchMethod, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> DeleteAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Delete, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Delete, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteAsync(
        string path,
        object body,
        CancellationToken cancellationToken)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Delete, url);
        ApplyDefaultHeaders(request);

        var json = JsonSerializer.Serialize(body, JsonOptions);
        request.Content = new StringContent(json, Encoding.UTF8, "application/json");

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<HttpResponseMessage> SendAsync(
        HttpRequestMessage request,
        CancellationToken cancellationToken = default)
    {
        _logger.LogDebug("HTTP {Method} {Url}", request.Method, request.RequestUri);
        ApplyDefaultHeaders(request);
        return await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
    }

    public async Task<byte[]> GetBytesAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        Dictionary<string, string>? headers = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        if (headers != null)
        {
            foreach (var header in headers)
            {
                request.Headers.TryAddWithoutValidation(header.Key, header.Value);
            }
        }

        using var response = await SendTransferAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
        return await ReadContentBytesAsync(response.Content, cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Sends a file transfer request (upload or download).
    /// When a transfer client is configured, the transfer is not capped by the connection request
    /// timeout, so a large file or a slow link is not aborted while data keeps flowing. A positive
    /// header timeout still bounds the wait for response headers: for a request without a body the
    /// deadline starts with the request, and for an upload it starts once the body of the current
    /// hop has been sent (a redirect re-arms it per hop), so a server that stops responding fails
    /// the transfer instead of hanging it. Without a header timeout the transfer is bounded only
    /// by the caller token. The response body is read with the caller token; a caller that wants
    /// the whole transfer bounded passes a CancellationToken.
    /// </summary>
    public async Task<HttpResponseMessage> SendTransferAsync(
        HttpRequestMessage request,
        CancellationToken cancellationToken = default)
    {
        if (request is null)
        {
            throw new ArgumentNullException(nameof(request));
        }

        ApplyDefaultHeaders(request);

        var transferClient = _transferHttpClient;
        if (transferClient is null)
        {
            _logger.LogDebug("HTTP {Method} {Url} (transfer, shared client)", request.Method, request.RequestUri);
            return await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
        }

        if (_transferHeaderTimeout is not { } headerTimeout || headerTimeout <= TimeSpan.Zero)
        {
            // A configured transfer client never falls back to the shared client's whole-call
            // request timeout; without a positive header timeout there is no deadline at all and
            // the caller token is the only bound.
            _logger.LogDebug("HTTP {Method} {Url} (transfer, no header timeout)", request.Method, request.RequestUri);
            return await transferClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
        }

        var headerDeadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        if (request.Content is null)
        {
            headerDeadline.CancelAfter(headerTimeout);
        }
        else
        {
            // The header deadline stays off while the body is being written - a large file or a
            // slow link must not count its own upload time against it - and is cleared again
            // before each hop of a redirect re-serializes the body.
            request.Content = new BodySentSignalingContent(
                request.Content,
                onBodySendStarted: () => DisarmHeaderDeadline(headerDeadline),
                onBodySent: () => ArmHeaderDeadline(headerDeadline, headerTimeout));
        }

        try
        {
            _logger.LogDebug("HTTP {Method} {Url} (transfer)", request.Method, request.RequestUri);
            return await transferClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, headerDeadline.Token).ConfigureAwait(false);
        }
        finally
        {
            // Response headers have arrived (or the send failed): stop the header deadline and
            // release the linked registration. Body reads use the caller token, so they stay
            // cancellable and are never bounded by the header deadline. A duplex transport can
            // still be writing the request body here, but HttpClient seals the request token when
            // the send returns, so a caller cancellation after this point cannot reach that write
            // whether or not the deadline is still alive.
            headerDeadline.Dispose();
        }
    }

    private static void ArmHeaderDeadline(CancellationTokenSource headerDeadline, TimeSpan headerTimeout)
    {
        try
        {
            headerDeadline.CancelAfter(headerTimeout);
        }
        catch (ObjectDisposedException)
        {
            // The send has already returned and released the deadline: arming it is moot.
        }
    }

    private static void DisarmHeaderDeadline(CancellationTokenSource headerDeadline)
    {
        try
        {
            headerDeadline.CancelAfter(Timeout.InfiniteTimeSpan);
        }
        catch (ObjectDisposedException)
        {
            // The send has already returned and released the deadline.
        }
    }

    public async Task<Stream> GetStreamAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        Dictionary<string, string>? headers = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        if (headers != null)
        {
            foreach (var header in headers)
            {
                request.Headers.TryAddWithoutValidation(header.Key, header.Value);
            }
        }

        var response = await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
        return await response.Content.ReadAsStreamAsync().ConfigureAwait(false);
    }

    private static async Task<byte[]> ReadContentBytesAsync(HttpContent content, CancellationToken cancellationToken)
    {
        using var source = await content.ReadAsStreamAsync().ConfigureAwait(false);
        using var buffer = new MemoryStream();
        var chunk = new byte[81920];
        int read;
        while ((read = await source.ReadAsync(chunk, 0, chunk.Length, cancellationToken).ConfigureAwait(false)) > 0)
        {
            buffer.Write(chunk, 0, read);
        }

        return buffer.ToArray();
    }

    /// <summary>
    /// Reads a response body as a string with the caller token. Transfer responses are sent with
    /// <see cref="HttpCompletionOption.ResponseHeadersRead"/>, so their bodies are not buffered
    /// by the transport and a stalled body must stay cancellable instead of blocking the caller.
    /// </summary>
    internal static async Task<string> ReadContentStringAsync(HttpContent content, CancellationToken cancellationToken)
    {
#if NET5_0_OR_GREATER
        return await content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
#else
        var bytes = await ReadContentBytesAsync(content, cancellationToken).ConfigureAwait(false);
        var charset = content.Headers.ContentType?.CharSet;
        if (!string.IsNullOrEmpty(charset))
        {
            try
            {
                return Encoding.GetEncoding(charset!.Trim('"')).GetString(bytes);
            }
            catch (ArgumentException)
            {
                // Unknown charset: fall back to the UTF-8 default of HttpContent.
            }
        }

        return Encoding.UTF8.GetString(bytes);
#endif
    }

    private string BuildUrl(string path, Dictionary<string, string?>? queryParams = null)
    {
        var url = path.StartsWith("/") ? $"{_baseUrl}{path}" : $"{_baseUrl}/{path}";

        if (queryParams == null || queryParams.Count == 0)
            return url;

        var queryString = string.Join("&",
            queryParams
                .Where(kv => kv.Value != null)
                .Select(kv => $"{Uri.EscapeDataString(kv.Key)}={Uri.EscapeDataString(kv.Value!)}"));

        return string.IsNullOrEmpty(queryString) ? url : $"{url}?{queryString}";
    }

    private void ApplyDefaultHeaders(HttpRequestMessage request)
    {
        foreach (var header in _defaultHeaders)
        {
            if (!request.Headers.Contains(header.Key))
            {
                request.Headers.TryAddWithoutValidation(header.Key, header.Value);
            }
        }
    }

    private async Task<T> HandleResponseAsync<T>(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        var content = await response.Content.ReadAsStringAsync().ConfigureAwait(false);

        if (!response.IsSuccessStatusCode)
        {
            LogHttpFailure(response);
            ThrowApiException(response, content);
        }

        if (string.IsNullOrEmpty(content))
        {
            throw new SandboxApiException(
                message: "Unexpected empty response body",
                statusCode: (int)response.StatusCode,
                error: new SandboxError(SandboxErrorCodes.UnexpectedResponse, "Unexpected empty response body"),
                rawBody: content);
        }

        try
        {
            return JsonSerializer.Deserialize<T>(content, JsonOptions)!;
        }
        catch (JsonException ex)
        {
            throw new SandboxApiException(
                message: $"Failed to deserialize response: {ex.Message}",
                statusCode: (int)response.StatusCode,
                rawBody: content,
                innerException: ex);
        }
    }

    private async Task EnsureSuccessAsync(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        if (!response.IsSuccessStatusCode)
        {
            var content = await ReadContentStringAsync(response.Content, cancellationToken).ConfigureAwait(false);
            LogHttpFailure(response);
            ThrowApiException(response, content);
        }
    }

    private void LogHttpFailure(HttpResponseMessage response)
    {
        var request = response.RequestMessage;
        var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
            ? values.FirstOrDefault()
            : null;

        _logger.LogError(
            "HTTP request failed: method={Method}, url={Url}, status={StatusCode}, requestId={RequestId}",
            request?.Method.Method ?? "UNKNOWN",
            request?.RequestUri?.ToString() ?? "UNKNOWN",
            (int)response.StatusCode,
            requestId ?? string.Empty);
    }

    private static void ThrowApiException(HttpResponseMessage response, string content)
    {
        var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
            ? values.FirstOrDefault()
            : null;

        string? errorMessage = null;
        string? errorCode = null;
        object? rawBody = content;

        if (!string.IsNullOrEmpty(content))
        {
            try
            {
                var parsed = JsonSerializer.Deserialize<Dictionary<string, JsonElement>>(content, JsonOptions);
                if (parsed != null)
                {
                    rawBody = parsed;
                    if (parsed.TryGetValue("message", out var msg))
                        errorMessage = msg.GetString();
                    if (parsed.TryGetValue("error", out var err) && err.ValueKind == JsonValueKind.Object)
                    {
                        if (err.TryGetProperty("message", out var errMsg))
                            errorMessage = errorMessage ?? errMsg.GetString();
                        if (err.TryGetProperty("code", out var errCode))
                            errorCode = errCode.GetString();
                    }
                    if (parsed.TryGetValue("code", out var code))
                        errorCode = errorCode ?? code.GetString();
                }
            }
            catch
            {
                // Ignore JSON parse errors
            }
        }

        var message = errorMessage ?? $"Request failed with status code {(int)response.StatusCode}" +
            (string.IsNullOrEmpty(content) ? string.Empty : $": {content}");
        var sandboxErrorCode = errorCode ?? SandboxErrorCodes.UnexpectedResponse;

        throw new SandboxApiException(
            message: message,
            statusCode: (int)response.StatusCode,
            requestId: requestId,
            rawBody: rawBody,
            error: new SandboxError(sandboxErrorCode, errorMessage ?? message));
    }
}
