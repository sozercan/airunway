"""Exercise installed SDKs, replacing only HTTP transport and identity issuance."""
import base64
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import requests
from botocore.awsrequest import AWSResponse
from google.oauth2.credentials import Credentials

import downloader as d


def response(request, body=b"ok", status=200, headers=None):
    result = requests.Response()
    result.status_code, result.url, result.request = status, request.url, request
    result.headers.update(headers or {})
    result.headers.setdefault("Content-Length", str(len(body)))
    result.raw = AWSBody(body)
    result._content = body
    result._content_consumed = True
    return result


class AWSBody(io.BytesIO):
    def stream(self, amt=1024, **_):
        yield from iter(lambda: self.read(amt), b"")


class AdapterTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = d.Output(self.root, d.Budget())

    def test_s3_sdk_signed_prefix_download(self):
        seen = []
        def send(_transport, request):
            seen.append(request)
            if "list-type=2" in request.url:
                data = b'<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated><Contents><Key>prefix/model.bin</Key><Size>2</Size></Contents></ListBucketResult>'
            else:
                data = b"ok"
            return AWSResponse(request.url, 200, {"content-length": str(len(data))}, AWSBody(data))
        credentials = {"aws_access_key_id": "fixture-key", "aws_secret_access_key": "fixture-secret", "aws_session_token": "fixture-session", "region_name": "us-west-2"}
        with patch("botocore.httpsession.URLLib3Session.send", send):
            d.download_s3("s3://bucket/prefix", "", credentials, self.output)
        self.assertEqual((self.root / "model.bin").read_bytes(), b"ok")
        self.assertEqual(len(seen), 2)
        for request in seen:
            self.assertIn(b"Credential=fixture-key/", request.headers["Authorization"])
            self.assertEqual(request.headers["X-Amz-Security-Token"], b"fixture-session")
            self.assertNotIn("fixture-secret", request.url)

    def test_s3_sdk_rejects_redirect_before_replaying_credentials(self):
        seen = []
        def send(_transport, request):
            seen.append(request)
            return AWSResponse(request.url, 307, {"location": "https://other.example/model"}, AWSBody(b""))
        with patch("botocore.httpsession.URLLib3Session.send", send), self.assertRaises(d.DownloadError):
            d.download_s3("s3://bucket/prefix", "model.bin", {"aws_access_key_id": "fixture", "aws_secret_access_key": "secret"}, self.output)
        self.assertEqual(len(seen), 1)

    def test_gcs_sdk_identity_generation_and_bytes(self):
        seen = []
        def send(_adapter, request, **_):
            seen.append(request)
            if "/download/" not in request.url:
                return response(request, json.dumps({"items": [{"name": "prefix/model.bin", "size": "2", "generation": "42"}]}).encode(), headers={"Content-Type": "application/json"})
            if request.headers.get("Range", "").startswith("bytes=2-"):
                return response(request, b"", status=416, headers={"Content-Range": "bytes */2"})
            return response(request, status=206, headers={"Content-Range": "bytes 0-1/2", "x-goog-generation": "42"})
        with patch.dict(os.environ, {"ARTIFACT_WORKLOAD_IDENTITY": "true"}), patch("google.auth.default", return_value=(Credentials("fixture-token"), "fixture-project")), patch("requests.adapters.HTTPAdapter.send", send):
            d.download_gcs("gs://bucket/prefix", "", {}, self.output)
        self.assertEqual((self.root / "model.bin").read_bytes(), b"ok")
        self.assertGreaterEqual(len(seen), 3)  # listing, file bytes, and EOF range probes
        for request in seen:
            self.assertEqual(request.headers["authorization"], "Bearer fixture-token")
        self.assertIn("ifGenerationMatch=42", seen[-1].url)
        self.assertIn("generation=42", seen[-1].url)

    def test_gcs_sdk_rejects_redirect(self):
        seen = []
        def send(_adapter, request, **_):
            seen.append(request)
            return response(request, status=302, headers={"Location": "https://other.example/file"})
        with patch("requests.adapters.HTTPAdapter.send", send), self.assertRaises(Exception):
            d.download_gcs("gs://bucket/prefix", "model.bin", {}, self.output)
        self.assertEqual(len(seen), 1)

    def test_azure_sdk_key_and_sas(self):
        for name, credentials in (("key.bin", {"account_key": base64.b64encode(b"fixture-key").decode()}), ("sas.bin", {"sas_token": "sv=2024-11-04&sr=b&sp=r&sig=fixture-signature"})):
            seen = []
            def send(_adapter, request, **_):
                seen.append(request)
                return response(request, status=206, headers={"Content-Range": "bytes 0-1/2", "x-ms-blob-type": "BlockBlob", "ETag": '"fixture"', "Last-Modified": "Wed, 01 Jan 2025 00:00:00 GMT"})
            with patch("requests.adapters.HTTPAdapter.send", send):
                d.download_azure("https://account.blob.core.windows.net/container/model.bin", name, credentials, self.output)
            self.assertEqual((self.root / name).read_bytes(), b"ok")
            self.assertEqual(len(seen), 1)
            if name == "key.bin":
                self.assertTrue(seen[0].headers["Authorization"].startswith("SharedKey account:"))
            else:
                self.assertIn("sig=fixture-signature", seen[0].url)

    def test_azure_sdk_rejects_redirect_with_sas(self):
        seen = []
        def send(_adapter, request, **_):
            seen.append(request)
            return response(request, status=307, headers={"Location": "https://other.example/file"})
        with patch("requests.adapters.HTTPAdapter.send", send), self.assertRaises(Exception):
            d.download_azure("https://account.blob.core.windows.net/container/model.bin", "", {"sas_token": "sig=fixture"}, self.output)
        self.assertEqual(len(seen), 1)


if __name__ == "__main__":
    unittest.main()
