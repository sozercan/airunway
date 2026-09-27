FROM python:3.14-slim

COPY requirements.txt /opt/downloader/requirements.txt
RUN pip install --no-cache-dir --require-hashes --index-url https://pypi.org/simple -r /opt/downloader/requirements.txt && \
    hf version
COPY downloader.py /opt/downloader/downloader.py

# The wrapper execs hf with unchanged args unless the first arg is "artifact".
ENTRYPOINT ["python", "/opt/downloader/downloader.py"]
