#!/usr/bin/env python3
"""Stage model artifacts, or delegate unchanged arguments to the legacy HF CLI."""
import base64
import contextlib
import gzip
import hashlib
import json
import logging
import os
from pathlib import Path
import re
import shutil
import signal
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

CHUNK = 1024 * 1024
MAX_BYTES = 1024**4  # 1 TiB of downloaded AND expanded data per attempt.
MAX_FILES = 100_000
MAX_SECONDS = 24 * 60 * 60
TIMEOUT = 60
MAX_MANIFEST = 4 * 1024 * 1024
MARKER = "._airunway_complete.json"
DIGEST = re.compile(r"sha256:[a-f0-9]{64}\Z")
REPOSITORY = re.compile(r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*\Z")
TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\Z")


class DownloadError(Exception):
    """Messages are constant, never SDK errors that could disclose credentials."""


def require(condition, message):
    if not condition:
        raise DownloadError(message)


def relative_path(value):
    require(isinstance(value, str) and 0 < len(value) <= 1024, "Invalid relative path")
    require(not any(ord(c) < 32 or ord(c) == 127 for c in value), "Unsafe path")
    require(not any(c in value for c in "\\%") and not value.startswith("/"), "Unsafe path")
    parts = value.split("/")
    require(all(p not in ("", ".", "..", MARKER) for p in parts), "Unsafe path")
    return value


def parsed_url(value, schemes=("https",), query=False):
    require(isinstance(value, str) and len(value) <= 8192, "Invalid URL")
    require(not any(c.isspace() or ord(c) < 32 for c in value) and "\\" not in value, "Unsafe URL")
    u = urllib.parse.urlsplit(value)
    require(u.scheme in schemes and u.hostname and not u.username and not u.password,
            "Unsupported URL or inline credentials")
    require(not u.fragment and "#" not in value and (query or "?" not in value), "URL query credentials are not allowed")
    _ = u.port  # Reject malformed ports.
    return u


def source_url(value):
    u = parsed_url(value, ("hf", "s3", "gs", "https", "oci"))
    require(len(value) <= 4096, "Source URL is too long")
    if u.scheme != "oci" and u.path not in ("", "/"):
        relative_path(urllib.parse.unquote(u.path[1:].removesuffix("/")))
    if u.scheme == "https":
        require(u.path and not u.path.endswith("/"), "HTTPS requires a single file")
    if u.scheme in ("hf", "s3", "gs"):
        require(u.port is None, "Source does not support a port")
    return u


def read_credentials():
    raw = os.environ.get("ARTIFACT_CREDENTIALS_JSON", "{}")
    require(len(raw) <= 65536, "Credentials document is too large")
    result = json.loads(raw)
    require(isinstance(result, dict), "Credentials must be a JSON object")
    return result


def credential_fields(credentials, allowed):
    require(set(credentials) <= set(allowed), "Unsupported credentials fields for this source")
    require(all(isinstance(v, str) and v and "\r" not in v and "\n" not in v
                for v in credentials.values()), "Invalid credentials fields")


class Budget:
    def __init__(self, max_bytes=MAX_BYTES, max_files=MAX_FILES):
        self.max_bytes, self.max_files = max_bytes, max_files
        self.bytes = self.files = 0
        self.started = time.monotonic()

    def add(self, count):
        require(isinstance(count, int) and count >= 0, "Invalid content size")
        self.bytes += count
        require(self.bytes <= self.max_bytes, "Artifact exceeds byte limit")
        require(time.monotonic() - self.started <= MAX_SECONDS, "Artifact download timed out")

    def file(self, size=0):
        self.files += 1
        require(self.files <= self.max_files, "Artifact exceeds file limit")
        require(isinstance(size, int) and 0 <= size <= self.max_bytes - self.bytes, "Artifact exceeds byte limit")
        self.add(0)


class Output:
    def __init__(self, root, budget):
        self.root, self.budget = Path(root), budget
        self.written = set()

    def path(self, name):
        name = relative_path(name)
        target = self.root / name
        current = self.root
        require(not current.is_symlink(), "Symlink destination is not allowed")
        for part in Path(name).parts:
            current = current / part
            require(not current.is_symlink(), "Symlink destination is not allowed")
        target.parent.mkdir(parents=True, exist_ok=True)
        return target

    def write(self, name, chunks, size=None):
        target = self.path(name)
        require(name not in self.written and not target.exists(), "Duplicate artifact file")
        self.budget.file(size or 0)
        count = 0
        with target.open("xb") as f:
            for chunk in chunks:
                self.budget.add(len(chunk))
                count += len(chunk)
                if size is not None:
                    require(count <= size, "Content exceeds declared size")
                f.write(chunk)
        require(size is None or size == count, "Truncated artifact file")
        self.written.add(name)


def chunks(reader):
    while True:
        data = reader.read(CHUNK)
        if not data:
            return
        yield data


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


# No automatic redirects, netrc, cookies, or global authentication handlers.
HTTP = urllib.request.build_opener(NoRedirect())


def request(url, headers=None):
    """Follow HTTPS redirects, discarding ALL auth on every redirect."""
    headers = dict(headers or {})
    for _ in range(6):
        parsed_url(url, query=True)
        try:
            return HTTP.open(urllib.request.Request(url, headers=headers), timeout=TIMEOUT)
        except urllib.error.HTTPError as error:
            if error.code not in (301, 302, 303, 307, 308):
                return error
            location = error.headers.get("Location")
            error.close()
            require(location is not None, "Redirect has no location")
            url = urllib.parse.urljoin(url, location)
            # Preserve only representation selection. Never forward credentials,
            # even on a same-host redirect or a redirect chain that returns home.
            headers = {k: v for k, v in headers.items() if k.lower() == "accept"}
    raise DownloadError("Too many redirects")


def success(response):
    require(response.status == 200, "Remote source rejected the download")
    require(response.headers.get("Content-Encoding", "identity") == "identity", "Unsupported HTTP content encoding")


def small_body(response, limit):
    success(response)
    body = response.read(limit + 1)
    require(len(body) <= limit, "Remote metadata exceeds size limit")
    return body


def basic_auth(credentials):
    require(bool(credentials.get("username")) == bool(credentials.get("password")), "Username and password must be supplied together")
    if "username" in credentials:
        require(":" not in credentials["username"], "Invalid basic auth username")
        return "Basic " + base64.b64encode((credentials["username"] + ":" + credentials["password"]).encode()).decode()
    return None


def http_auth(credentials):
    credential_fields(credentials, ("username", "password", "bearer_token"))
    require(not ("bearer_token" in credentials and "username" in credentials), "Choose one HTTP authentication method")
    auth = "Bearer " + credentials["bearer_token"] if "bearer_token" in credentials else basic_auth(credentials)
    return {"Authorization": auth} if auth else {}


def download_https(uri, file, credentials, output):
    u = source_url(uri)
    name = file or urllib.parse.unquote(u.path.rsplit("/", 1)[-1])
    with contextlib.closing(request(uri, http_auth(credentials))) as response:
        success(response)
        size = response.headers.get("Content-Length")
        output.write(name, chunks(response), int(size) if size is not None else None)


def download_hf(uri, revision, file, credentials, output):
    credential_fields(credentials, ("token",))
    repo = uri.removeprefix("hf://")
    require(1 <= len(repo.split("/")) <= 2 and all(re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]*", p) and ".." not in p for p in repo.split("/")), "Invalid HF repository")
    if revision:
        relative_path(revision)
    # Use the official SDK for snapshot discovery, pin its commit, then stream
    # each file ourselves so byte limits and redirect credential rules also apply
    # to HF. SDK metadata/pagination requests may never leave huggingface.co.
    import httpx
    from huggingface_hub import HfApi, hf_hub_url
    from huggingface_hub.utils import set_client_factory
    def metadata_request(req):
        parsed = parsed_url(str(req.url), query=True)
        require(parsed.netloc == "huggingface.co", "HF metadata left the trusted endpoint")
    set_client_factory(lambda: httpx.Client(timeout=TIMEOUT, follow_redirects=False, max_redirects=0,
                                           event_hooks={"request": [metadata_request]}))
    token = credentials.get("token") or os.environ.get("HF_TOKEN")
    api = HfApi(endpoint="https://huggingface.co", token=token or False)
    sha = api.repo_info(repo, revision=revision or "main", timeout=TIMEOUT).sha
    require(isinstance(sha, str) and re.fullmatch(r"[a-f0-9]{40,64}", sha), "Invalid HF revision response")
    expected = {}
    total = 0
    entries = 0
    for entry in api.list_repo_tree(repo, revision=sha, recursive=True):
        entries += 1
        require(entries <= MAX_FILES * 2, "HF repository tree exceeds limits")
        relative_path(entry.path)
        if not hasattr(entry, "size") or (file and entry.path != file):
            continue
        require(isinstance(entry.size, int) and entry.size >= 0, "Missing HF file size")
        expected[entry.path] = entry.size
        total += entry.size
        require(len(expected) <= output.budget.max_files and total <= output.budget.max_bytes, "HF snapshot exceeds limits")
    require(expected and (not file or file in expected), "HF source or selected file is empty")
    headers = {"Authorization": "Bearer " + token} if token else {}
    for name, size in expected.items():
        url = hf_hub_url(repo, name, revision=sha, endpoint="https://huggingface.co")
        with contextlib.closing(request(url, headers)) as response:
            success(response)
            output.write(name, chunks(response), size)


def object_prefix(uri, file):
    u = source_url(uri)
    prefix = urllib.parse.unquote(u.path.lstrip("/")).rstrip("/")
    if file:
        return u.netloc, (prefix + "/" if prefix else "") + relative_path(file)
    return u.netloc, prefix + "/" if prefix else ""


def download_s3(uri, file, credentials, output):
    credential_fields(credentials, ("aws_access_key_id", "aws_secret_access_key", "aws_session_token", "region_name"))
    require(bool(credentials.get("aws_access_key_id")) == bool(credentials.get("aws_secret_access_key")), "Incomplete AWS credentials")
    import boto3
    from botocore.config import Config
    from botocore import UNSIGNED
    identity = os.environ.get("ARTIFACT_WORKLOAD_IDENTITY") == "true"
    options = {} if identity or "aws_access_key_id" in credentials else {"signature_version": UNSIGNED}
    client = boto3.client("s3", **credentials, config=Config(connect_timeout=TIMEOUT, read_timeout=TIMEOUT, retries={"max_attempts": 3}, **options))
    # Cloud SDK retries are allowed; redirects cannot replay authorization.
    def deny_redirect(response, **_):
        if response is not None and 300 <= response[0].status_code < 400:
            raise DownloadError("S3 redirects are not supported; configure the bucket region")
    client.meta.events.register_first("needs-retry.s3", deny_redirect)
    bucket, prefix = object_prefix(uri, file)
    count = 0
    try:
        if file:
            objects = [{"Key": prefix}]
        else:
            objects = (obj for page in client.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=prefix) for obj in page.get("Contents", []))
        for index, obj in enumerate(objects):
            require(index < MAX_FILES * 2, "S3 listing exceeds limits")
            key = obj["Key"]
            if key.endswith("/"):
                continue
            require(key.startswith(prefix), "Object outside requested prefix")
            name = file or key[len(prefix):]
            relative_path(name)
            response = client.get_object(Bucket=bucket, Key=key)
            with contextlib.closing(response["Body"]) as body:
                output.write(name, chunks(body), response["ContentLength"])
            count += 1
        require(count, "S3 source prefix is empty")
    finally:
        client.close()


def download_gcs(uri, file, credentials, output):
    from google.cloud import storage
    from google.oauth2 import service_account
    auth = None
    if credentials:
        # Service account JSON is the official Google key format. Do not allow
        # a supplied token_uri to exfiltrate a private-key signed assertion.
        require(credentials.get("type") == "service_account" and credentials.get("token_uri") == "https://oauth2.googleapis.com/token" and credentials.get("universe_domain", "googleapis.com") == "googleapis.com", "Expected Google service account credentials")
        auth = service_account.Credentials.from_service_account_info(credentials)
    if not credentials and os.environ.get("ARTIFACT_WORKLOAD_IDENTITY") != "true":
        client = storage.Client.create_anonymous_client()
    else:
        client = storage.Client(project=credentials.get("project_id"), credentials=auth)
    # requests strips auth on cross-host redirects but retains it on same-host
    # redirects. Disable redirects entirely for authenticated object API calls.
    original_request = client._http.request
    def no_redirect(method, url, **kwargs):
        kwargs["allow_redirects"] = False
        return original_request(method, url, **kwargs)
    client._http.request = no_redirect
    bucket, prefix = object_prefix(uri, file)
    count = 0
    try:
        if file:
            blob = client.bucket(bucket).blob(prefix)
            blob.reload(timeout=TIMEOUT)
            blobs = [blob]
        else:
            blobs = client.list_blobs(bucket, prefix=prefix, timeout=TIMEOUT, page_size=1000)
        for index, blob in enumerate(blobs):
            require(index < MAX_FILES * 2, "GCS listing exceeds limits")
            if blob.name.endswith("/"):
                continue
            require(blob.name.startswith(prefix), "Object outside requested prefix")
            name = file or blob.name[len(prefix):]
            relative_path(name)
            # A generation precondition prevents mixing versions across ranged reads.
            with blob.open("rb", chunk_size=CHUNK, timeout=TIMEOUT, if_generation_match=blob.generation, raw_download=True) as body:
                output.write(name, chunks(body), blob.size)
            count += 1
        require(count, "GCS source prefix is empty")
    finally:
        client.close()


def azure_blob(uri):
    host = source_url(uri).hostname
    return any(host.endswith(suffix) and host != suffix.lstrip(".") for suffix in (".blob.core.windows.net", ".blob.core.usgovcloudapi.net", ".blob.core.chinacloudapi.cn"))


def download_azure(uri, file, credentials, output):
    from azure.storage.blob import BlobClient
    from azure.identity import DefaultAzureCredential, ClientSecretCredential
    from azure.core.pipeline.transport import RequestsTransport
    import requests
    class NoRedirectSession(requests.Session):
        def request(self, method, url, **kwargs):
            kwargs["allow_redirects"] = False
            return super().request(method, url, **kwargs)
    transport = RequestsTransport(session=NoRedirectSession())
    credential_fields(credentials, ("account_key", "sas_token", "tenant_id", "client_id", "client_secret"))
    modes = int("account_key" in credentials) + int("sas_token" in credentials) + int(any(k in credentials for k in ("tenant_id", "client_id", "client_secret")))
    require(modes <= 1, "Choose one Azure credential method")
    auth = credentials.get("account_key") or credentials.get("sas_token")
    if auth is None:
        if credentials:
            require(all(k in credentials for k in ("tenant_id", "client_id", "client_secret")), "Incomplete Azure identity credentials")
            auth = ClientSecretCredential(**credentials, transport=transport)
        else:
            auth = DefaultAzureCredential(exclude_interactive_browser_credential=True, transport=transport) if os.environ.get("ARTIFACT_WORKLOAD_IDENTITY") == "true" else None
    u = source_url(uri)
    require(len(u.path.strip("/").split("/")) >= 2, "Azure URL must include container and blob")
    try:
        with BlobClient.from_blob_url(uri, credential=auth, connection_timeout=TIMEOUT, read_timeout=TIMEOUT, retry_total=3, redirect_max=0, transport=transport) as client:
            stream = client.download_blob(max_concurrency=1, timeout=TIMEOUT)
            output.write(file or urllib.parse.unquote(u.path.rsplit("/", 1)[-1]), stream.chunks(), stream.size)
    finally:
        if hasattr(auth, "close"):
            auth.close()
        transport.close()


def oci_reference(uri):
    u = source_url(uri)
    repo = u.path.lstrip("/")
    if "@" in repo:
        repo, ref = repo.rsplit("@", 1)
        require(DIGEST.fullmatch(ref), "Unsupported OCI digest")
        if ":" in repo:
            repo, tag = repo.rsplit(":", 1)
            require(TAG.fullmatch(tag), "Invalid OCI tag")
    else:
        require(":" in repo, "OCI requires an explicit tag or digest")
        repo, ref = repo.rsplit(":", 1)
        require(TAG.fullmatch(ref), "Invalid OCI tag")
    require(REPOSITORY.fullmatch(repo), "Invalid OCI repository")
    registry = u.netloc
    # Docker Hub aliases name the registry, not its V2 HTTP endpoint. Resolve
    # only the standard HTTPS aliases here, without following a redirect or
    # changing the original URI used by stage() to identify completed caches.
    if u.hostname in ("docker.io", "index.docker.io") and u.port in (None, 443):
        registry = "registry-1.docker.io"
    return "https://" + registry, repo, ref


class Registry:
    def __init__(self, origin, repo, credentials):
        credential_fields(credentials, ("username", "password", "bearer_token", "token_service"))
        self.origin, self.repo, self.credentials = origin, repo, credentials
        self.headers = http_auth({k: v for k, v in credentials.items() if k != "token_service"})
        if "token_service" in credentials:
            parsed_url(credentials["token_service"])

    def get(self, kind, ref):
        url = f"{self.origin}/v2/{self.repo}/{kind}/{ref}"
        headers = {"Accept": "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json", **self.headers}
        response = request(url, headers)
        if response.status != 401:
            return response
        # Never trust an authentication challenge from a redirected blob host.
        require(response.geturl() == url, "Redirected authentication challenge rejected")
        challenge = response.headers.get("WWW-Authenticate", "")
        response.close()
        require(len(challenge) <= 8192 and challenge.lower().startswith("bearer "), "Unsupported registry authentication challenge")
        fields = urllib.request.parse_keqv_list(urllib.request.parse_http_list(challenge[7:]))
        realm = fields.get("realm", "")
        r = parsed_url(realm)
        require(r.path and not r.query, "Invalid registry token service")
        auth = {}
        # Cross-origin token services may issue anonymous tokens. Send static
        # credentials only to the registry origin or an explicitly trusted realm.
        if urllib.parse.urlsplit(self.origin).netloc == r.netloc or realm == self.credentials.get("token_service"):
            value = basic_auth(self.credentials)
            if value:
                auth["Authorization"] = value
        params = {"scope": "repository:" + self.repo + ":pull"}
        if "service" in fields:
            require(len(fields["service"]) <= 1024, "Invalid registry service")
            params["service"] = fields["service"]
        with contextlib.closing(request(realm + "?" + urllib.parse.urlencode(params), auth)) as token_response:
            payload = json.loads(small_body(token_response, 65536))
        token = payload.get("token") or payload.get("access_token")
        require(isinstance(token, str) and 0 < len(token) <= 32768 and not any(c.isspace() for c in token), "Invalid registry bearer token")
        self.headers = {"Authorization": "Bearer " + token}
        return request(url, {**headers, **self.headers})


class LimitedReader:
    """Count decompressed bytes including tar headers/padding, not just files."""
    def __init__(self, reader, budget):
        self.reader, self.budget = reader, budget

    def read(self, count=-1):
        data = self.reader.read(CHUNK if count < 0 else min(count, CHUNK))
        self.budget.add(len(data))
        return data


class SafeTarInfo(tarfile.TarInfo):
    def _proc_member(self, archive):
        # tarfile otherwise allocates a PAX/longname record using its untrusted size.
        metadata = (tarfile.XHDTYPE, tarfile.XGLTYPE, tarfile.SOLARIS_XHDTYPE, tarfile.GNUTYPE_LONGNAME, tarfile.GNUTYPE_LONGLINK)
        if self.type in metadata:
            require(self.size <= 65536, "OCI tar metadata exceeds limit")
        return super()._proc_member(archive)


def extract_tar(fileobj, compressed, output, expanded):
    raw = gzip.GzipFile(fileobj=fileobj) if compressed else fileobj
    limited = LimitedReader(raw, expanded)
    try:
        with tarfile.open(fileobj=limited, mode="r|", tarinfo=SafeTarInfo) as archive:
            for entry in archive:
                name = entry.name
                # A single leading ./ is customary in tar, but never collapse .. .
                if name.startswith("./"):
                    name = name[2:]
                if entry.isdir() and name in ("", "."):
                    continue
                name = relative_path(name.rstrip("/") if entry.isdir() else name)
                expanded.file()
                require(not any(p.startswith(".wh.") for p in name.split("/")), "OCI whiteouts are unsupported")
                require(not entry.issparse() and (entry.isfile() or entry.isdir()), "OCI links and special files are not allowed")
                if entry.isdir():
                    output.path(name).mkdir(exist_ok=True)
                else:
                    require(entry.size <= output.budget.max_bytes - output.budget.bytes, "Expanded artifact exceeds limits")
                    output.write(name, chunks(archive.extractfile(entry)), entry.size)
        # Drain to verify gzip CRC/trailer and account for trailing compressed data.
        for _ in chunks(limited):
            pass
    finally:
        if compressed:
            raw.close()


def download_oci(uri, file, credentials, output):
    origin, repo, ref = oci_reference(uri)
    registry = Registry(origin, repo, credentials)
    with contextlib.closing(registry.get("manifests", ref)) as response:
        body = small_body(response, MAX_MANIFEST)
        advertised = response.headers.get("Docker-Content-Digest")
    actual = "sha256:" + hashlib.sha256(body).hexdigest()
    require(not ref.startswith("sha256:") or actual == ref, "OCI manifest digest mismatch")
    require(not advertised or advertised == actual, "OCI manifest digest mismatch")
    manifest = json.loads(body)
    require(manifest.get("schemaVersion") == 2 and "manifests" not in manifest and isinstance(manifest.get("layers"), list), "Only OCI/Docker v2 artifact manifests are supported, not indexes")
    require(0 < len(manifest["layers"]) <= 1024, "Invalid OCI layer count")
    transfer = Budget(output.budget.max_bytes)
    for layer in manifest["layers"]:
        digest, size, media = layer.get("digest", ""), layer.get("size"), layer.get("mediaType", "")
        require(DIGEST.fullmatch(digest) and isinstance(size, int) and size >= 0, "Invalid OCI blob descriptor")
        require(not layer.get("urls"), "External OCI layer URLs are unsupported")
        transfer.file(size)
        with tempfile.TemporaryFile(dir=output.root.parent) as blob:
            with contextlib.closing(registry.get("blobs", digest)) as response:
                success(response)
                hasher = hashlib.sha256()
                count = 0
                for chunk in chunks(response):
                    transfer.add(len(chunk))
                    count += len(chunk)
                    require(count <= size, "OCI blob exceeds declared size")
                    hasher.update(chunk)
                    blob.write(chunk)
            require(count == size and "sha256:" + hasher.hexdigest() == digest, "OCI blob digest or size mismatch")
            blob.seek(0)
            if media in ("application/vnd.oci.image.layer.v1.tar", "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip"):
                expanded = Budget(min(output.budget.max_bytes, max(1024 * 1024, size * 1000)))
                extract_tar(blob, media.endswith("gzip"), output, expanded)
            elif media in ("application/octet-stream", "application/vnd.oci.image.layer.v1.blob", "application/json", "application/vnd.safetensors", "application/vnd.gguf"):
                name = layer.get("annotations", {}).get("org.opencontainers.image.title")
                output.write(relative_path(name), chunks(blob), size)
            else:
                raise DownloadError("Unsupported OCI layer media type")
    require(output.written and (not file or file in output.written), "OCI selected file was not found")


def audit(root, max_bytes=MAX_BYTES):
    budget = Budget(max_bytes)
    for parent, dirs, files in os.walk(root, followlinks=False):
        for name in dirs + files:
            p = Path(parent) / name
            require(not p.is_symlink(), "Artifact contains a symlink")
            if p.is_file():
                budget.file(p.stat().st_size)
                budget.add(p.stat().st_size)
            else:
                require(p.is_dir(), "Artifact contains a special file")
    return budget


def stage(uri, revision, file, credentials, destination):
    u = source_url(uri)
    if file:
        relative_path(file)
    require(not revision or (u.scheme == "hf" and relative_path(revision)), "Revision is supported only for HF")
    destination = Path(destination)
    require(destination.is_absolute() and destination.name == "artifacts", "Destination must be the cache artifacts directory")
    for p in (destination, *destination.parents):
        require(not p.is_symlink(), "Symlink destination is not allowed")
    require(destination.parent.is_dir(), "Cache mount is missing")
    identity = hashlib.sha256(json.dumps([uri, revision, file]).encode()).hexdigest()
    if destination.exists():
        marker = destination / MARKER
        require(marker.is_file() and not marker.is_symlink() and marker.stat().st_size <= 128 and marker.read_text() == identity,
                "Destination exists without a matching completed download; use an empty cache")
        audit(destination)
        require(not file or (destination / file).is_file(), "Selected file is missing from cache")
        return
    temporary = Path(tempfile.mkdtemp(prefix=".airunway-download-", dir=destination.parent))
    try:
        output = Output(temporary, Budget())
        if u.scheme == "hf":
            download_hf(uri, revision, file, credentials, output)
        elif u.scheme == "s3":
            download_s3(uri, file, credentials, output)
        elif u.scheme == "gs":
            download_gcs(uri, file, credentials, output)
        elif u.scheme == "oci":
            download_oci(uri, file, credentials, output)
        elif azure_blob(uri):
            download_azure(uri, file, credentials, output)
        else:
            download_https(uri, file, credentials, output)
        require(output.written, "Source produced no files")
        audit(temporary)
        (temporary / MARKER).write_text(identity)
        require(not destination.exists() and not destination.is_symlink(), "Destination changed during download")
        # mkdtemp creates mode 0700. Serving images may use a different UID,
        # so publish with the same read/traverse access as the downloaded files.
        temporary.chmod(0o755)
        temporary.rename(destination)
    finally:
        # Only remove the unique staging directory this process created, never
        # the destination, mounted cache, or another job's partially written files.
        if temporary.exists():
            shutil.rmtree(temporary)


def main():
    if sys.argv[1:2] != ["artifact"]:
        os.execvp("hf", ["hf", *sys.argv[1:]])
    logging.disable(logging.CRITICAL)
    def interrupted(_signum, _frame):
        # The Job deadline can send SIGTERM before our own alarm expires.
        # Neither that alarm nor repeated termination should interrupt cleanup.
        signal.alarm(0)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        raise DownloadError("Artifact download interrupted")
    signal.signal(signal.SIGALRM, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.alarm(MAX_SECONDS)
    try:
        stage(os.environ["ARTIFACT_URI"], os.environ.get("ARTIFACT_REVISION", ""),
              os.environ.get("ARTIFACT_FILE", ""), read_credentials(), os.environ["ARTIFACT_DESTINATION"])
        print("Artifact download complete")
    except Exception:
        # SDK/HTTP exceptions may contain signed URLs, keys, or response bodies.
        # Do not print exception messages or tracebacks to Kubernetes logs.
        print("Artifact download failed; check source, credentials, supported format, and cache capacity", file=sys.stderr)
        return 1
    finally:
        signal.alarm(0)
    return 0


if __name__ == "__main__":
    sys.exit(main())
