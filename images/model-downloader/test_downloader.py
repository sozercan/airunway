import base64
import contextlib
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch
import urllib.error

import downloader as d


class Response(io.BytesIO):
    def __init__(self, data=b"", status=200, headers=None, url="https://registry.example/v2/org/model/manifests/v1"):
        super().__init__(data)
        self.status, self.headers, self.url = status, headers or {}, url

    def geturl(self):
        return self.url


def digest(value):
    return "sha256:" + hashlib.sha256(value).hexdigest()


def tar_bytes(entries, mode="w"):
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode=mode) as archive:
        for name, value, kind in entries:
            item = tarfile.TarInfo(name)
            if kind != tarfile.REGTYPE:
                item.type = kind
                item.linkname = "/outside"
                archive.addfile(item)
            else:
                item.size = len(value)
                archive.addfile(item, io.BytesIO(value))
    return out.getvalue()


class DownloaderTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.output = d.Output(self.root, d.Budget())

    def test_paths(self):
        for name in ("../escape", "/absolute", "a/./b", "a//b", "a\\b", "a\x00b", "%2e%2e/a", "x/" + d.MARKER):
            with self.subTest(name=name), self.assertRaises(d.DownloadError):
                self.output.write(name, [b"bad"])
        (self.root / "link").symlink_to(self.root / "other")
        with self.assertRaises(d.DownloadError):
            self.output.write("link/file", [b"bad"])

    def test_url_validation(self):
        for url in ("http://example.com/file", "https://user:password@example.com/file", "s3://bucket/../bad", "gs://bucket/%252e%252e/file", "https://example.com/file?token=secret", "https://example.com/file#fragment"):
            with self.subTest(url=url), self.assertRaises(d.DownloadError):
                d.source_url(url)

    def test_limits_and_truncation(self):
        with self.assertRaises(d.DownloadError):
            d.Output(self.root, d.Budget(max_bytes=3)).write("too-big", [b"1234"])
        with self.assertRaises(d.DownloadError):
            self.output.write("short", [b"12"], 3)
        budget = d.Budget(max_files=1)
        out = d.Output(self.root, budget)
        out.write("one", [b"x"])
        with self.assertRaises(d.DownloadError):
            out.write("two", [b"x"])

    def test_https_filename_and_auth(self):
        with patch.object(d, "request", return_value=Response(b"weights", headers={"Content-Length": "7"})) as req:
            d.download_https("https://example.com/model.bin", "quant/model.gguf", {"bearer_token": "test-token"}, self.output)
        self.assertEqual((self.root / "quant/model.gguf").read_bytes(), b"weights")
        self.assertEqual(req.call_args.args[1], {"Authorization": "Bearer test-token"})

    def test_redirect_strips_auth_and_rejects_downgrade(self):
        first = urllib.error.HTTPError("https://first/file", 302, "", {"Location": "https://second/file"}, None)
        with patch.object(d.HTTP, "open", side_effect=[first, Response()]) as request:
            d.request("https://first/file", {"Authorization": "Bearer test-token", "X-Api-Key": "test-key"}).close()
            self.assertEqual(request.call_args_list[1].args[0].headers, {})
            self.assertEqual(request.call_args.kwargs["timeout"], d.TIMEOUT)
        first = urllib.error.HTTPError("https://first/file", 302, "", {"Location": "http://second/file"}, None)
        with patch.object(d.HTTP, "open", side_effect=[first]), self.assertRaises(d.DownloadError):
            d.request("https://first/file")

    def test_local_http_streaming(self):
        # Production is HTTPS-only. Replace only the TLS transport boundary with
        # loopback HTTP to exercise urllib status, headers, and streaming locally.
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", "7")
                self.end_headers()
                self.wfile.write(b"weights")
            def log_message(self, *_):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        opener = d.urllib.request.build_opener(d.NoRedirect())
        def local(req, timeout):
            url = "http://127.0.0.1:" + str(server.server_port) + "/file"
            return opener.open(url, timeout=timeout)
        with patch.object(d.HTTP, "open", side_effect=local):
            d.download_https("https://example.com/model.gguf", "", {}, self.output)
        self.assertEqual((self.root / "model.gguf").read_bytes(), b"weights")

    def test_atomic_publish_and_retry(self):
        destination = self.root / "artifacts"
        with patch.object(d, "download_https") as fetch:
            def fail(uri, file, creds, output):
                output.write("partial", [b"partial"])
                raise ValueError("remote secret")
            fetch.side_effect = fail
            with self.assertRaises(ValueError):
                d.stage("https://example.com/model.gguf", "", "", {}, destination)
            self.assertFalse(destination.exists())
            self.assertEqual(list(self.root.iterdir()), [])
            fetch.side_effect = lambda uri, file, creds, out: out.write("model.gguf", [b"ok"])
            d.stage("https://example.com/model.gguf", "", "", {}, destination)
            d.stage("https://example.com/model.gguf", "", "", {}, destination)
            self.assertEqual(fetch.call_count, 2)
            with self.assertRaises(d.DownloadError):
                d.stage("https://other.example.com/model.gguf", "", "", {}, destination)
        self.assertEqual((destination / "model.gguf").read_bytes(), b"ok")
        self.assertEqual(destination.stat().st_mode & 0o777, 0o755)

    def test_hf_snapshot_and_selected_revision(self):
        entries = [SimpleNamespace(path="weights/model.gguf", size=3), SimpleNamespace(path="config.json", size=2)]
        api = MagicMock(token="test-token")
        api.repo_info.return_value = SimpleNamespace(sha="a" * 40)
        api.list_repo_tree.return_value = entries
        with patch("huggingface_hub.HfApi", return_value=api), patch.object(d, "request", side_effect=[Response(b"123"), Response(b"{}")]) as download:
            d.download_hf("hf://org/model", "v1", "", {"token": "test-token"}, self.output)
            self.assertIn("/resolve/" + "a" * 40 + "/weights/model.gguf", download.call_args_list[0].args[0])
            self.assertEqual(download.call_args_list[0].args[1], {"Authorization": "Bearer test-token"})
            api.repo_info.assert_called_once_with("org/model", revision="v1", timeout=d.TIMEOUT)
        selected = self.root / "selected"
        selected.mkdir()
        with patch("huggingface_hub.HfApi", return_value=api), patch.object(d, "request", return_value=Response(b"123")) as download:
            d.download_hf("hf://org/model", "v1", "weights/model.gguf", {}, d.Output(selected, d.Budget()))
            self.assertEqual(download.call_count, 1)
            self.assertTrue((selected / "weights/model.gguf").is_file())
        with patch("huggingface_hub.HfApi", return_value=api), self.assertRaises(d.DownloadError):
            d.download_hf("hf://org/model", "", "missing", {}, self.output)

    def test_s3_recursive_and_selected(self):
        client = MagicMock()
        client.get_paginator.return_value.paginate.return_value = [
            {"Contents": [{"Key": "prefix/"}, {"Key": "prefix/a/model.bin"}]},
            {"Contents": [{"Key": "prefix/config.json"}]}]
        client.get_object.side_effect = lambda **_: {"Body": io.BytesIO(b"ok"), "ContentLength": 2}
        with patch("boto3.client", return_value=client):
            d.download_s3("s3://bucket/prefix", "", {}, self.output)
        self.assertEqual((self.root / "a/model.bin").read_bytes(), b"ok")
        client.close.assert_called_once()
        with patch("boto3.client", return_value=client):
            d.download_s3("s3://bucket/prefix", "one.bin", {}, self.output)
        self.assertEqual(client.get_object.call_args.kwargs, {"Bucket": "bucket", "Key": "prefix/one.bin"})
        client.get_paginator.return_value.paginate.return_value = [{"Contents": [{"Key": "prefix/../../outside"}]}]
        with patch("boto3.client", return_value=client), self.assertRaises(d.DownloadError):
            d.download_s3("s3://bucket/prefix", "", {}, self.output)

    def test_gcs_recursive_generation(self):
        client, blob = MagicMock(), MagicMock()
        blob.name, blob.size, blob.generation = "prefix/model.bin", 2, 42
        blob.open.return_value = io.BytesIO(b"ok")
        client.list_blobs.return_value = [blob]
        with patch("google.cloud.storage.Client.create_anonymous_client", return_value=client):
            d.download_gcs("gs://bucket/prefix", "", {}, self.output)
        self.assertEqual(blob.open.call_args.kwargs["if_generation_match"], 42)
        self.assertEqual((self.root / "model.bin").read_bytes(), b"ok")
        with self.assertRaises(d.DownloadError):
            d.download_gcs("gs://bucket/prefix", "", {"type": "service_account", "token_uri": "https://evil.example/token"}, self.output)

    def test_azure_identity_and_key(self):
        client = MagicMock()
        client.download_blob.return_value.size = 2
        client.download_blob.return_value.chunks.return_value = iter([b"ok"])
        context = MagicMock()
        context.__enter__.return_value = client
        with patch("azure.storage.blob.BlobClient.from_blob_url", return_value=context) as factory, patch("azure.identity.DefaultAzureCredential") as identity, patch.dict(os.environ, {"ARTIFACT_WORKLOAD_IDENTITY": "true"}):
            d.download_azure("https://account.blob.core.windows.net/container/model.bin", "", {}, self.output)
            self.assertEqual(factory.call_args.kwargs["credential"], identity.return_value)
            self.assertEqual(factory.call_args.kwargs["redirect_max"], 0)
            identity.return_value.close.assert_called_once()
        client.download_blob.return_value.chunks.return_value = iter([b"ok"])
        with patch("azure.storage.blob.BlobClient.from_blob_url", return_value=context) as factory:
            d.download_azure("https://account.blob.core.windows.net/container/model.bin", "key.bin", {"account_key": "test-key"}, self.output)
            self.assertEqual(factory.call_args.kwargs["credential"], "test-key")
        self.assertFalse(d.azure_blob("https://blob.core.windows.net.evil.example/file"))

    def test_oci_tar_and_raw_blobs(self):
        layer = tar_bytes([("model/config.json", b"{}", tarfile.REGTYPE)], "w:gz")
        raw = b"weights"
        manifest = json.dumps({"schemaVersion": 2, "layers": [
            {"digest": digest(layer), "size": len(layer), "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip"},
            {"digest": digest(raw), "size": len(raw), "mediaType": "application/octet-stream", "annotations": {"org.opencontainers.image.title": "model/weights.bin"}}]}).encode()
        def get(kind, ref):
            return Response(manifest if kind == "manifests" else layer if ref == digest(layer) else raw)
        with patch.object(d.Registry, "get", side_effect=get):
            d.download_oci("oci://registry.example/org/model@" + digest(manifest), "model/weights.bin", {}, self.output)
        self.assertEqual((self.root / "model/config.json").read_bytes(), b"{}")
        self.assertEqual((self.root / "model/weights.bin").read_bytes(), raw)

    def test_oci_corruption_and_unsupported(self):
        for media, checksum in (("application/octet-stream", digest(b"wrong")), ("unsupported/zstd", digest(b"ok"))):
            manifest = json.dumps({"schemaVersion": 2, "layers": [{"digest": checksum, "size": 2, "mediaType": media}]}).encode()
            with patch.object(d.Registry, "get", side_effect=[Response(manifest), Response(b"ok")]), self.assertRaises(d.DownloadError):
                d.download_oci("oci://registry.example/org/model:v1", "", {}, self.output)
        with patch.object(d.Registry, "get", return_value=Response(b"{}")), self.assertRaises(d.DownloadError):
            d.download_oci("oci://registry.example/org/model@sha256:" + "a" * 64, "", {}, self.output)
        with patch.object(d.Registry, "get", return_value=Response(b'{"schemaVersion":2,"manifests":[]}')), self.assertRaises(d.DownloadError):
            d.download_oci("oci://registry.example/org/model:v1", "", {}, self.output)

    def test_safe_tar(self):
        for name, kind in (("../escape", tarfile.REGTYPE), ("/escape", tarfile.REGTYPE), ("link", tarfile.SYMTYPE), ("hard", tarfile.LNKTYPE), ("device", tarfile.CHRTYPE), (".wh.config", tarfile.REGTYPE)):
            payload = tar_bytes([(name, b"x", kind)])
            with self.subTest(name=name), self.assertRaises(d.DownloadError):
                d.extract_tar(io.BytesIO(payload), False, self.output, d.Budget())
        payload = tar_bytes([("oversized", b"x" * 1024, tarfile.REGTYPE)], "w:gz")
        with self.assertRaises(d.DownloadError):
            d.extract_tar(io.BytesIO(payload), True, self.output, d.Budget(max_bytes=100))
        # Bound extended header allocation before tarfile reads an untrusted size.
        header = tarfile.TarInfo("pax")
        header.type, header.size = tarfile.XHDTYPE, 1024**4
        with self.assertRaises(d.DownloadError):
            d.extract_tar(io.BytesIO(header.tobuf(format=tarfile.GNU_FORMAT)), False, self.output, d.Budget())

    def test_registry_challenge_scopes_and_credential_boundary(self):
        for realm, trusted in (("https://auth.example/token", False), ("https://auth.example/token", True), ("https://registry.example/token", False)):
            creds = {"username": "test-user", "password": "test-password"}
            if trusted:
                creds["token_service"] = realm
            responses = [Response(status=401, headers={"WWW-Authenticate": f'Bearer realm="{realm}",service="registry",scope="repository:other:push"'}), Response(b'{"token":"test-bearer"}'), Response(b"manifest")]
            with patch.object(d, "request", side_effect=responses) as req:
                d.Registry("https://registry.example", "org/model", creds).get("manifests", "v1").close()
                token_url, token_headers = req.call_args_list[1].args
                self.assertIn("repository%3Aorg%2Fmodel%3Apull", token_url)
                self.assertNotIn("push", token_url)
                self.assertEqual("Authorization" in token_headers, trusted or "registry.example" in realm)
                self.assertEqual(req.call_args_list[2].args[1]["Authorization"], "Bearer test-bearer")
        with patch.object(d, "request", return_value=Response(status=401, headers={"WWW-Authenticate": 'Bearer realm="http://auth.example/token"'})), self.assertRaises(d.DownloadError):
            d.Registry("https://registry.example", "org/model", {}).get("manifests", "v1")

    def test_cli_legacy_and_sanitized_failure(self):
        with patch.object(sys, "argv", ["downloader.py", "download", "org/model"]), patch.object(os, "execvp", side_effect=RuntimeError("exec")) as run:
            with self.assertRaises(RuntimeError):
                d.main()
            run.assert_called_once_with("hf", ["hf", "download", "org/model"])
        env = {**os.environ, "ARTIFACT_URI": "https://user:DO_NOT_DISCLOSE@example.com/file", "ARTIFACT_DESTINATION": str(self.root / "artifacts")}
        result = subprocess.run([sys.executable, str(Path(d.__file__)), "artifact"], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertNotIn("DO_NOT_DISCLOSE", result.stdout + result.stderr)
        self.assertNotIn("Traceback", result.stderr)


if __name__ == "__main__":
    unittest.main()
