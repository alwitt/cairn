FROM python:3.12-slim

RUN mkdir /app
COPY README.md poetry.lock pyproject.toml /app
COPY cairn_sidecar /app/cairn_sidecar

RUN cd /app && \
    pip install poetry==2.4.1 && \
    poetry build && poetry install --no-root && \
    pip install dist/*.whl && \
    cd / && rm -rf /app
