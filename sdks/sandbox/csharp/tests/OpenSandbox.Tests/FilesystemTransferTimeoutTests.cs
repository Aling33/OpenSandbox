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

using System.Diagnostics;
using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Net.Sockets;
using System.Text;
using FluentAssertions;
using Microsoft.Extensions.Logging.Abstractions;
using OpenSandbox.Config;
using OpenSandbox.Factory;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Xunit;

namespace OpenSandbox.Tests;

/// <summary>
/// File uploads and downloads must survive longer than <see cref="ConnectionConfig.RequestTimeoutSeconds"/>
/// while they keep making progress, and must still fail when the server stops responding. These tests
/// run a loopback HTTP server and use the SDK's real wiring (ConnectionConfig -> HttpClientProvider ->
/// DefaultAdapterFactory), so they exercise the exact clients file adapters receive.
/// </summary>
public class FilesystemTransferTimeoutTests
{
    private const int RequestTimeoutSeconds = 1;
    private static readonly TimeSpan ResponseBound = TimeSpan.FromSeconds(5);

    [Fact]
    public async Task UploadOutlivingRequestTimeoutCompletes()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest(async (stream, head, _) =>
        {
            head.Path.Should().Be("/files/upload");
            head.ContentLength.Should().BeGreaterThan(0, "uploads must keep a Content-Length");
            await ReadRequestBodyAsync(stream, head);
            await WriteResponseAsync(stream, "ok");
        });

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;
        var serving = server.ServeAsync();

        var slowBody = new SlowReadStream(
            totalBytes: 1024 * 1024,
            chunkBytes: 256 * 1024,
            delayPerChunk: TimeSpan.FromMilliseconds(500));
        var elapsed = Stopwatch.StartNew();
        await files.WriteFilesAsync(new[] { new WriteEntry { Path = "/tmp/slow.bin", Data = slowBody } });
        elapsed.Stop();

        elapsed.Elapsed.Should().BeGreaterThan(
            TimeSpan.FromSeconds(RequestTimeoutSeconds),
            "the upload has to outlive the request timeout to prove transfers are not capped by it");
        slowBody.BytesProduced.Should().Be(slowBody.Length);
        await serving;
    }

    [Fact]
    public async Task DownloadOutlivingRequestTimeoutCompletes()
    {
        const int bodySize = 512 * 1024;
        await using var server = new LoopbackHttpServer();
        server.OnRequest((stream, _, _) => WriteSlowBodyAsync(stream, bodySize, 64 * 1024, TimeSpan.FromMilliseconds(300)));

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;
        var serving = server.ServeAsync();

        var elapsed = Stopwatch.StartNew();
        var bytes = await files.ReadBytesAsync("/tmp/slow.bin");
        elapsed.Stop();

        bytes.Should().HaveCount(bodySize);
        elapsed.Elapsed.Should().BeGreaterThan(
            TimeSpan.FromSeconds(RequestTimeoutSeconds),
            "the download has to outlive the request timeout to prove transfers are not capped by it");
        await serving;
    }

    [Fact]
    public async Task StreamingDownloadOutlivingRequestTimeoutCompletes()
    {
        const int bodySize = 512 * 1024;
        await using var server = new LoopbackHttpServer();
        server.OnRequest((stream, _, _) => WriteSlowBodyAsync(stream, bodySize, 64 * 1024, TimeSpan.FromMilliseconds(300)));

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;
        var serving = server.ServeAsync();

        var elapsed = Stopwatch.StartNew();
        var total = 0L;
        await foreach (var chunk in files.ReadBytesStreamAsync("/tmp/slow.bin"))
        {
            total += chunk.Length;
        }
        elapsed.Stop();

        total.Should().Be(bodySize);
        elapsed.Elapsed.Should().BeGreaterThan(TimeSpan.FromSeconds(RequestTimeoutSeconds));
        await serving;
    }

    [Fact]
    public async Task IsolatedSessionUploadOutlivingRequestTimeoutCompletes()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest(async (stream, head, _) =>
        {
            head.Path.Should().Be("/v1/isolated/session");
            await WriteResponseAsync(stream, "{\"session_id\":\"s-1\"}");
        });
        server.OnRequest(async (stream, head, _) =>
        {
            head.Path.Should().Be("/v1/isolated/session/s-1/files/upload");
            head.ContentLength.Should().BeGreaterThan(0);
            await ReadRequestBodyAsync(stream, head);
            await WriteResponseAsync(stream, "ok");
        });

        var (stack, provider) = CreateStack(server.Port);
        using var _ = provider;
        var serving = server.ServeAsync();
        var session = await stack.Isolation.CreateAsync(new CreateIsolatedSessionRequest(Profile: "default"));

        var elapsed = Stopwatch.StartNew();
        await session.Files.WriteFilesAsync(new[]
        {
            new WriteEntry
            {
                Path = "/tmp/slow.bin",
                Data = new SlowReadStream(1024 * 1024, 256 * 1024, TimeSpan.FromMilliseconds(500)),
            },
        });
        elapsed.Stop();

        elapsed.Elapsed.Should().BeGreaterThan(TimeSpan.FromSeconds(RequestTimeoutSeconds));
        await serving;
    }

    [Fact]
    public async Task UploadStillFailsWhenServerStopsRespondingAfterTheBody()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest(async (stream, head, cancellationToken) =>
        {
            await ReadRequestBodyAsync(stream, head);
            await WaitForPeerCloseAsync(stream, cancellationToken);
        });
        var serving = server.ServeIgnoringClientAbortsAsync();

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;

        var elapsed = Stopwatch.StartNew();
        var exception = await FluentActions.Awaiting(() => files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = "/tmp/f.bin", Data = new byte[1024] },
            }))
            .Should().ThrowAsync<TaskCanceledException>();
        elapsed.Stop();

        exception.Which.InnerException.Should().BeOfType<TimeoutException>(
            "the header deadline must be distinguishable from caller cancellation");
        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "a server that stops responding after receiving the body must fail the upload, not hang it");
        await serving;
    }

    [Fact]
    public async Task DownloadStillFailsWhenServerNeverSendsHeaders()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest((stream, _, cancellationToken) => WaitForPeerCloseAsync(stream, cancellationToken));
        var serving = server.ServeIgnoringClientAbortsAsync();

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;

        var elapsed = Stopwatch.StartNew();
        var exception = await FluentActions.Awaiting(() => files.ReadBytesAsync("/tmp/f.bin"))
            .Should().ThrowAsync<TaskCanceledException>();
        elapsed.Stop();

        exception.Which.InnerException.Should().BeOfType<TimeoutException>(
            "the header deadline must be distinguishable from caller cancellation");
        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "the wait for response headers must stay bounded even though transfers are not capped");
        await serving;
    }

    [Fact]
    public async Task NonTransferCallKeepsTheRequestTimeout()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest((stream, _, cancellationToken) => WaitForPeerCloseAsync(stream, cancellationToken));
        var serving = server.ServeIgnoringClientAbortsAsync();

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;

        var elapsed = Stopwatch.StartNew();
        await FluentActions.Awaiting(() => files.GetFileInfoAsync(new[] { "/tmp/f.bin" }))
            .Should().ThrowAsync<OperationCanceledException>();
        elapsed.Stop();

        elapsed.Elapsed.Should().BeLessThan(ResponseBound);
        await serving;
    }

    [Fact]
    public async Task TransferStillHonorsCallerCancellation()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest((stream, _, cancellationToken) =>
            WriteSlowBodyAsync(stream, 8 * 1024 * 1024, 8 * 1024, TimeSpan.FromMilliseconds(200), cancellationToken));
        var serving = server.ServeIgnoringClientAbortsAsync();

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;

        using var cts = new CancellationTokenSource(TimeSpan.FromMilliseconds(300));
        var elapsed = Stopwatch.StartNew();
        await FluentActions.Awaiting(() => files.ReadBytesAsync("/tmp/slow.bin", null, cts.Token))
            .Should().ThrowAsync<OperationCanceledException>();
        elapsed.Stop();

        elapsed.Elapsed.Should().BeLessThan(ResponseBound, "a caller token must still cancel a transfer in flight");
        await serving;
    }

    [Fact]
    public async Task UploadBodyReadHonorsCallerCancellation()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest(async (stream, head, _) => await ReadRequestBodyAsync(stream, head));
        var serving = server.ServeIgnoringClientAbortsAsync();

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;

        // The body read blocks for far longer than the caller token lives, so only a token that
        // reaches the body read itself can end the upload promptly.
        var stalledBody = new SlowReadStream(
            totalBytes: 1024 * 1024,
            chunkBytes: 256 * 1024,
            delayPerChunk: TimeSpan.FromSeconds(30));
        using var cts = new CancellationTokenSource(TimeSpan.FromMilliseconds(300));
        var elapsed = Stopwatch.StartNew();
        await FluentActions.Awaiting(() => files.WriteFilesAsync(
                new[] { new WriteEntry { Path = "/tmp/slow.bin", Data = stalledBody } },
                cts.Token))
            .Should().ThrowAsync<OperationCanceledException>();
        elapsed.Stop();

        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "a caller token must interrupt an upload body read that blocks");
        await serving;
    }

    [Fact]
    public void TransferHttpClientHasNoWholeCallTimeout()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions { RequestTimeoutSeconds = RequestTimeoutSeconds });

        using var shared = config.CreateHttpClient();
        using var transfer = config.CreateTransferHttpClient();

        shared.Timeout.Should().Be(TimeSpan.FromSeconds(RequestTimeoutSeconds));
        transfer.Timeout.Should().Be(Timeout.InfiniteTimeSpan);
    }

    [Fact]
    public async Task RedirectedUploadBodySendIsNotCappedByThePreviousHopsDeadline()
    {
        // A 307/308 makes the transport serialize the same request content once per hop, so the
        // deadline armed after the first hop's body send is still ticking while the second hop's
        // body is written. The handler reproduces that double serialization; the slow sink keeps
        // the second hop's body send past RequestTimeoutSeconds while every individual write
        // stays far inside it, and the response follows immediately afterwards.
        var handler = new RedirectEmulatingHandler();
        using var shared = new HttpClient { Timeout = TimeSpan.FromSeconds(RequestTimeoutSeconds) };
        using var transfer = new HttpClient(handler) { Timeout = Timeout.InfiniteTimeSpan };
        var wrapper = new HttpClientWrapper(
            shared,
            "http://127.0.0.1:1",
            transferHttpClient: transfer,
            transferHeaderTimeout: TimeSpan.FromSeconds(RequestTimeoutSeconds));

        using var request = new HttpRequestMessage(HttpMethod.Post, "http://127.0.0.1:1/files/upload")
        {
            Content = new ByteArrayContent(new byte[512 * 1024]),
        };

        var elapsed = Stopwatch.StartNew();
        using var response = await wrapper.SendTransferAsync(request);
        elapsed.Stop();

        response.StatusCode.Should().Be(HttpStatusCode.OK);
        handler.Serializations.Should().Be(2, "a redirect serializes the body once per hop");
        elapsed.Elapsed.Should().BeGreaterThan(
            TimeSpan.FromSeconds(RequestTimeoutSeconds),
            "the second hop's body send has to outlive the request timeout");
    }

    [Theory]
    [InlineData(null)]
    [InlineData(0)]
    public async Task ConfiguredTransferClientWithoutHeaderTimeoutKeepsTransfersUncapped(int? headerTimeoutSeconds)
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest(async (stream, _, cancellationToken) =>
        {
            await Task.Delay(TimeSpan.FromSeconds(2), cancellationToken);
            await WriteResponseAsync(stream, "ok");
        });
        var serving = server.ServeIgnoringClientAbortsAsync();

        TimeSpan? headerTimeout = headerTimeoutSeconds is { } seconds
            ? TimeSpan.FromSeconds(seconds)
            : null;
        using var shared = new HttpClient { Timeout = TimeSpan.FromSeconds(RequestTimeoutSeconds) };
        using var transfer = new HttpClient { Timeout = Timeout.InfiniteTimeSpan };
        var wrapper = new HttpClientWrapper(
            shared,
            FormattableString.Invariant($"http://127.0.0.1:{server.Port}"),
            transferHttpClient: transfer,
            transferHeaderTimeout: headerTimeout);

        using var request = new HttpRequestMessage(
            HttpMethod.Get,
            FormattableString.Invariant($"http://127.0.0.1:{server.Port}/files/download?path=%2Ftmp%2Ff.bin"));
        using var response = await wrapper.SendTransferAsync(request);

        response.StatusCode.Should().Be(HttpStatusCode.OK);
        await serving;
    }

    [Fact]
    public async Task DownloadErrorBodyHonorsCallerCancellation()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest((stream, _, cancellationToken) => WriteStalledErrorAsync(stream, cancellationToken));
        var serving = server.ServeIgnoringClientAbortsAsync();

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;

        using var cts = new CancellationTokenSource(TimeSpan.FromMilliseconds(300));
        var download = files.ReadBytesAsync("/tmp/failure.bin", null, cts.Token);
        var elapsed = Stopwatch.StartNew();
        await FluentActions.Awaiting(() => download.WaitAsync(ResponseBound))
            .Should().ThrowAsync<OperationCanceledException>();
        elapsed.Stop();

        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "a caller token must release a transfer error body that stops making progress");
        await serving;
    }

    [Fact]
    public async Task UploadErrorBodyHonorsCallerCancellation()
    {
        await using var server = new LoopbackHttpServer();
        server.OnRequest(async (stream, head, cancellationToken) =>
        {
            await ReadRequestBodyAsync(stream, head);
            await WriteStalledErrorAsync(stream, cancellationToken);
        });
        var serving = server.ServeIgnoringClientAbortsAsync();

        var (files, provider) = CreateFiles(server.Port);
        using var _ = provider;

        using var cts = new CancellationTokenSource(TimeSpan.FromMilliseconds(300));
        var upload = files.WriteFilesAsync(
            new[] { new WriteEntry { Path = "/tmp/failure.bin", Data = new byte[1024] } },
            cts.Token);
        var elapsed = Stopwatch.StartNew();
        await FluentActions.Awaiting(() => upload.WaitAsync(ResponseBound))
            .Should().ThrowAsync<OperationCanceledException>();
        elapsed.Stop();

        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "a caller token must release a transfer error body that stops making progress");
        await serving;
    }

    [Fact]
    public async Task UploadConnectPhaseIsBoundedByTheHeaderDeadline()
    {
        // Until the first body byte is serialized there is no body send to clear the deadline:
        // without arming it up front, a connect phase that never completes would hang an upload.
        using var handler = new StallingSendHandler();
        using var shared = new HttpClient { Timeout = TimeSpan.FromSeconds(RequestTimeoutSeconds) };
        using var transfer = new HttpClient(handler) { Timeout = Timeout.InfiniteTimeSpan };
        var wrapper = new HttpClientWrapper(
            shared,
            "http://127.0.0.1:1",
            transferHttpClient: transfer,
            transferHeaderTimeout: TimeSpan.FromSeconds(RequestTimeoutSeconds));

        var content = new TrackingContent(4096);
        using var request = new HttpRequestMessage(HttpMethod.Post, "http://127.0.0.1:1/files/upload")
        {
            Content = content,
        };

        var elapsed = Stopwatch.StartNew();
        var exception = await FluentActions.Awaiting(() => wrapper.SendTransferAsync(request))
            .Should().ThrowAsync<TaskCanceledException>();
        elapsed.Stop();

        exception.Which.InnerException.Should().BeOfType<TimeoutException>(
            "the header deadline must be distinguishable from caller cancellation");
        content.Serializations.Should().Be(
            0,
            "the connect phase must be bounded before the body is serialized");
        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "the connect phase must be bounded by the header deadline for uploads too");
    }

    [Fact]
    public async Task UploadFileContentInterruptsABlockedProducerRead()
    {
        // This is the manual copy loop that also runs on the netstandard2.0/2.1 builds: the
        // caller token must reach the producer read, not just the socket, or an upload whose
        // source blocks would hang on those targets.
        var producer = new SlowReadStream(
            totalBytes: 1024 * 1024,
            chunkBytes: 256 * 1024,
            delayPerChunk: TimeSpan.FromSeconds(30));
        using var cts = new CancellationTokenSource(TimeSpan.FromMilliseconds(300));
        using var content = new UploadFileContent(producer, cts.Token);

        var elapsed = Stopwatch.StartNew();
        await FluentActions.Awaiting(() => content.CopySourceToAsync(Stream.Null, CancellationToken.None))
            .Should().ThrowAsync<OperationCanceledException>();
        elapsed.Stop();

        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "a caller token must interrupt a producer read that blocks");
    }

    [Fact]
    public async Task UploadFileContentInterruptsABlockedDestinationWrite()
    {
        using var source = new MemoryStream(new byte[512 * 1024]);
        using var cts = new CancellationTokenSource(TimeSpan.FromMilliseconds(300));
        using var content = new UploadFileContent(source, cts.Token);
        var sink = new SlowWriteStream(chunkBytes: 64 * 1024, delayPerChunk: TimeSpan.FromSeconds(30));

        var elapsed = Stopwatch.StartNew();
        await FluentActions.Awaiting(() => content.CopySourceToAsync(sink, CancellationToken.None))
            .Should().ThrowAsync<OperationCanceledException>();
        elapsed.Stop();

        elapsed.Elapsed.Should().BeLessThan(
            ResponseBound,
            "the caller token must reach the destination write as well");
    }

    [Fact]
    public void UploadFileContentKeepsSeekableUploadsLengthDelimited()
    {
        // StreamContent derives the part's Content-Length from a seekable source; the
        // replacement content must do the same or multipart uploads would turn chunked.
        using var source = new MemoryStream(new byte[10]);
        source.Position = 4;

        using var content = new UploadFileContent(source, CancellationToken.None);

        content.Headers.ContentLength.Should().Be(6);
    }

    [Fact]
    public async Task ReadContentStringAsyncDecodesTheContentCharset()
    {
        // One code path on every TFM: the bytes are read with the caller token and decoded with
        // the charset of the Content-Type.
        var latin1 = new ByteArrayContent(new[] { (byte)0xE9 });
        latin1.Headers.ContentType = new MediaTypeHeaderValue("text/plain") { CharSet = "iso-8859-1" };
        (await HttpClientWrapper.ReadContentStringAsync(latin1, CancellationToken.None)).Should().Be("\u00E9");

        var quoted = new ByteArrayContent(Encoding.Unicode.GetBytes("h\u00E9llo"));
        quoted.Headers.ContentType = new MediaTypeHeaderValue("text/plain") { CharSet = "\"utf-16\"" };
        (await HttpClientWrapper.ReadContentStringAsync(quoted, CancellationToken.None)).Should().Be("h\u00E9llo");

        var unknown = new ByteArrayContent(Encoding.UTF8.GetBytes("h\u00E9llo"));
        unknown.Headers.ContentType = new MediaTypeHeaderValue("text/plain") { CharSet = "x-unknown-charset" };
        (await HttpClientWrapper.ReadContentStringAsync(unknown, CancellationToken.None)).Should().Be(
            "h\u00E9llo",
            "an unknown charset falls back to the UTF-8 default");

        var bom = new ByteArrayContent(
            new byte[] { 0xEF, 0xBB, 0xBF }.Concat(Encoding.UTF8.GetBytes("h\u00E9llo")).ToArray());
        bom.Headers.ContentType = new MediaTypeHeaderValue("text/plain");
        (await HttpClientWrapper.ReadContentStringAsync(bom, CancellationToken.None)).Should().Be(
            "h\u00E9llo",
            "a byte order mark is not part of the text");
    }

    private static (ISandboxFiles Files, HttpClientProvider Provider) CreateFiles(int port)
    {
        var (stack, provider) = CreateStack(port);
        return (stack.Files, provider);
    }

    private static (ExecdStack Stack, HttpClientProvider Provider) CreateStack(int port)
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = FormattableString.Invariant($"127.0.0.1:{port}"),
            RequestTimeoutSeconds = RequestTimeoutSeconds,
        });

        var provider = new HttpClientProvider(config, NullLoggerFactory.Instance);
        var stack = new DefaultAdapterFactory().CreateExecdStack(new CreateExecdStackOptions
        {
            ConnectionConfig = config,
            // Mirrors Sandbox.CreateAsync: the execd base URL carries no path so
            // adapters add their own routes.
            ExecdBaseUrl = FormattableString.Invariant($"http://127.0.0.1:{port}"),
            HttpClientProvider = provider,
            LoggerFactory = NullLoggerFactory.Instance,
        });
        return (stack, provider);
    }

    private static async Task ReadRequestBodyAsync(Stream stream, RequestHead head)
    {
        var remaining = head.ContentLength - head.BodyPrefix.Length;
        var buffer = new byte[64 * 1024];
        while (remaining > 0)
        {
            var read = await stream.ReadAsync(buffer.AsMemory(0, Math.Min(buffer.Length, remaining)), CancellationToken.None);
            if (read == 0)
            {
                throw new IOException("The client closed the connection before sending the full request body.");
            }

            remaining -= read;
        }
    }

    private static async Task WaitForPeerCloseAsync(Stream stream, CancellationToken cancellationToken)
    {
        var buffer = new byte[1];
        try
        {
            while (await stream.ReadAsync(buffer, cancellationToken) > 0)
            {
            }
        }
        catch (IOException)
        {
        }
        catch (SocketException)
        {
        }
    }

    private static async Task WriteResponseAsync(Stream stream, string body)
    {
        var payload = Encoding.UTF8.GetBytes(body);
        var head = FormattableString.Invariant(
            $"HTTP/1.1 200 OK\r\nContent-Length: {payload.Length}\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n");
        await stream.WriteAsync(Encoding.ASCII.GetBytes(head));
        await stream.WriteAsync(payload);
        await stream.FlushAsync();
    }

    private static async Task WriteStalledErrorAsync(Stream stream, CancellationToken cancellationToken)
    {
        var head = "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 4096\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n";
        await stream.WriteAsync(Encoding.ASCII.GetBytes(head), cancellationToken);
        await stream.WriteAsync(Encoding.ASCII.GetBytes("{\"message\":\"boom\""), cancellationToken);
        await stream.FlushAsync(cancellationToken);
        await WaitForPeerCloseAsync(stream, cancellationToken);
    }

    private static async Task WriteSlowBodyAsync(
        Stream stream,
        int contentLength,
        int chunkBytes,
        TimeSpan delayPerChunk,
        CancellationToken cancellationToken = default)
    {
        var head = FormattableString.Invariant(
            $"HTTP/1.1 200 OK\r\nContent-Length: {contentLength}\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n");
        await stream.WriteAsync(Encoding.ASCII.GetBytes(head), cancellationToken);
        await stream.FlushAsync(cancellationToken);

        var chunk = new byte[chunkBytes];
        var remaining = contentLength;
        while (remaining > 0)
        {
            await Task.Delay(delayPerChunk, cancellationToken);
            var size = Math.Min(chunkBytes, remaining);
            await stream.WriteAsync(chunk.AsMemory(0, size), cancellationToken);
            await stream.FlushAsync(cancellationToken);
            remaining -= size;
        }
    }

    private sealed class RequestHead
    {
        public string RequestLine { get; init; } = string.Empty;

        public string Path { get; init; } = string.Empty;

        public int ContentLength { get; init; }

        public byte[] BodyPrefix { get; init; } = Array.Empty<byte>();

        public static async Task<RequestHead> ReadAsync(Stream stream, CancellationToken cancellationToken)
        {
            var received = new List<byte>(4096);
            var chunk = new byte[1024];
            var headerEnd = -1;
            while (headerEnd < 0)
            {
                var read = await stream.ReadAsync(chunk, cancellationToken);
                if (read == 0)
                {
                    throw new IOException("The client closed the connection before sending the request headers.");
                }

                received.AddRange(chunk.Take(read));
                headerEnd = FindHeaderEnd(received);
            }

            var lines = Encoding.ASCII.GetString(received.ToArray(), 0, headerEnd).Split("\r\n");
            var requestLine = lines[0];
            var target = requestLine.Split(' ').ElementAtOrDefault(1) ?? string.Empty;
            var contentLength = 0;
            foreach (var line in lines)
            {
                if (line.StartsWith("Content-Length:", StringComparison.OrdinalIgnoreCase))
                {
                    contentLength = int.Parse(line["Content-Length:".Length..].Trim(), CultureInfo.InvariantCulture);
                }
            }

            return new RequestHead
            {
                RequestLine = requestLine,
                Path = target.Split('?')[0],
                ContentLength = contentLength,
                BodyPrefix = received.Skip(headerEnd + 4).ToArray(),
            };
        }

        private static int FindHeaderEnd(List<byte> bytes)
        {
            for (var i = 3; i < bytes.Count; i++)
            {
                if (bytes[i - 3] == (byte)'\r' && bytes[i - 2] == (byte)'\n' && bytes[i - 1] == (byte)'\r' && bytes[i] == (byte)'\n')
                {
                    return i - 3;
                }
            }

            return -1;
        }
    }

    private sealed class LoopbackHttpServer : IAsyncDisposable
    {
        private readonly TcpListener _listener;
        private readonly CancellationTokenSource _cts = new();
        private readonly Queue<Func<NetworkStream, RequestHead, CancellationToken, Task>> _handlers = new();

        public LoopbackHttpServer()
        {
            _listener = new TcpListener(IPAddress.Loopback, 0);
            _listener.Start();
        }

        public int Port => ((IPEndPoint)_listener.LocalEndpoint).Port;

        public void OnRequest(Func<NetworkStream, RequestHead, CancellationToken, Task> handler) => _handlers.Enqueue(handler);

        /// <summary>Accepts the queued connections in order and runs their handlers.</summary>
        public async Task ServeAsync()
        {
            while (_handlers.Count > 0)
            {
                using var client = await _listener.AcceptTcpClientAsync(_cts.Token);
                using var stream = client.GetStream();
                var head = await RequestHead.ReadAsync(stream, _cts.Token);
                await _handlers.Dequeue()(stream, head, _cts.Token);
            }
        }

        /// <summary>Runs <see cref="ServeAsync"/> and swallows the connection errors a client abort produces.</summary>
        public async Task ServeIgnoringClientAbortsAsync()
        {
            try
            {
                await ServeAsync();
            }
            catch (Exception ex) when (ex is IOException or SocketException or ObjectDisposedException or OperationCanceledException)
            {
            }
        }

        public ValueTask DisposeAsync()
        {
            _cts.Cancel();
            _listener.Stop();
            _cts.Dispose();
            return ValueTask.CompletedTask;
        }
    }

    /// <summary>
    /// Serializes the request body twice like the transport does for a 307/308 redirect, the
    /// second time into a sink that is slower than the header timeout.
    /// </summary>
    private sealed class RedirectEmulatingHandler : HttpMessageHandler
    {
        public int Serializations { get; private set; }

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            await request.Content!.CopyToAsync(Stream.Null, cancellationToken);
            Serializations++;

            await request.Content!.CopyToAsync(
                new SlowWriteStream(chunkBytes: 64 * 1024, delayPerChunk: TimeSpan.FromMilliseconds(300)),
                cancellationToken);
            Serializations++;

            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                RequestMessage = request,
                Content = new ByteArrayContent(Array.Empty<byte>()),
            };
        }
    }

    /// <summary>Stalls like a connect that never completes, without touching the request body.</summary>
    private sealed class StallingSendHandler : HttpMessageHandler
    {
        protected override async Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            await Task.Delay(Timeout.InfiniteTimeSpan, cancellationToken);
            return new HttpResponseMessage(HttpStatusCode.OK);
        }
    }

    /// <summary>Counts body serializations so a test can prove the body was never touched.</summary>
    private sealed class TrackingContent : ByteArrayContent
    {
        public TrackingContent(int size)
            : base(new byte[size])
        {
        }

        public int Serializations { get; private set; }

#if NET5_0_OR_GREATER
        protected override Task SerializeToStreamAsync(
            Stream stream,
            TransportContext? context,
            CancellationToken cancellationToken)
        {
            Serializations++;
            return base.SerializeToStreamAsync(stream, context, cancellationToken);
        }
#endif

        protected override Task SerializeToStreamAsync(Stream stream, TransportContext? context)
        {
            Serializations++;
            return base.SerializeToStreamAsync(stream, context);
        }
    }

    private sealed class SlowWriteStream : Stream
    {
        private readonly int _chunkBytes;
        private readonly TimeSpan _delayPerChunk;

        public SlowWriteStream(int chunkBytes, TimeSpan delayPerChunk)
        {
            _chunkBytes = chunkBytes;
            _delayPerChunk = delayPerChunk;
        }

        public override bool CanRead => false;

        public override bool CanSeek => false;

        public override bool CanWrite => true;

        public override long Length => throw new NotSupportedException();

        public override long Position
        {
            get => throw new NotSupportedException();
            set => throw new NotSupportedException();
        }

        public override async Task WriteAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken)
        {
            var written = 0;
            while (written < count)
            {
                await Task.Delay(_delayPerChunk, cancellationToken);
                written += Math.Min(_chunkBytes, count - written);
            }
        }

#if NETSTANDARD2_1_OR_GREATER || NET5_0_OR_GREATER
        // Modern runtimes copy through the memory-based overload; it forwards to the token-aware
        // array-based write above so a canceled deadline interrupts the write instead of being
        // silently dropped by the sync path.
        public override ValueTask WriteAsync(ReadOnlyMemory<byte> buffer, CancellationToken cancellationToken = default) =>
            new(WriteAsync(buffer.ToArray(), 0, buffer.Length, cancellationToken));
#endif

        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();

        public override void Flush()
        {
        }

        public override int Read(byte[] buffer, int offset, int count) => throw new NotSupportedException();

        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();

        public override void SetLength(long value) => throw new NotSupportedException();
    }

    private sealed class SlowReadStream : Stream
    {
        private readonly int _chunkBytes;
        private readonly TimeSpan _delayPerChunk;
        private long _position;

        public SlowReadStream(long totalBytes, int chunkBytes, TimeSpan delayPerChunk)
        {
            Length = totalBytes;
            _chunkBytes = chunkBytes;
            _delayPerChunk = delayPerChunk;
        }

        public long BytesProduced => _position;

        public override bool CanRead => true;

        public override bool CanSeek => true;

        public override bool CanWrite => false;

        public override long Length { get; }

        public override long Position
        {
            get => _position;
            set => _position = value;
        }

        public override async Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken cancellationToken)
        {
            if (_position >= Length)
            {
                return 0;
            }

            await Task.Delay(_delayPerChunk, cancellationToken);
            var size = (int)Math.Min(Math.Min(count, _chunkBytes), Length - _position);
            for (var i = 0; i < size; i++)
            {
                buffer[offset + i] = 0x61;
            }
            _position += size;
            return size;
        }

#if NETSTANDARD2_1_OR_GREATER || NET5_0_OR_GREATER
        // Modern runtimes read through the memory-based overload; forward it to the token-aware
        // array-based read above.
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
        {
            var chunk = new byte[buffer.Length];
            var size = await ReadAsync(chunk, 0, chunk.Length, cancellationToken).ConfigureAwait(false);
            chunk.AsSpan(0, size).CopyTo(buffer.Span);
            return size;
        }
#endif

        public override int Read(byte[] buffer, int offset, int count) =>
            ReadAsync(buffer, offset, count, CancellationToken.None).GetAwaiter().GetResult();

        public override void Flush()
        {
        }

        public override long Seek(long offset, SeekOrigin origin)
        {
            _position = origin switch
            {
                SeekOrigin.Begin => offset,
                SeekOrigin.Current => _position + offset,
                SeekOrigin.End => Length + offset,
                _ => throw new ArgumentOutOfRangeException(nameof(origin)),
            };
            return _position;
        }

        public override void SetLength(long value) => throw new NotSupportedException();

        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }
}
