# Builder-образ для air-gapped релизной сборки (AIRGAP=1 make release,
# см. GNUmakefile и Jenkinsfile). Поверх зеркала golang добавляет то, чего
# не хватает для полного `make release` внутри одного контейнера:
#   - gcc-aarch64-linux-gnu — CGO-кросс-компиляция linux-arm64
#     (аналог deployment/docker/builder/Dockerfile);
#   - zip — упаковка windows-архивов.
#
# Собирается во внешнем контуре и зеркалируется во внутренний registry:
#   docker build --build-arg go_builder_image=golang:1.26-bookworm \
#     -t registry.corp.local/mirror/vl-airgap-builder:1.26-bookworm \
#     deployment/jenkins/builder
ARG go_builder_image=golang:1.26-bookworm
FROM $go_builder_image
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc-x86-64-linux-gnu \
        gcc-aarch64-linux-gnu \
        libc6-dev-arm64-cross \
        zip \
    && rm -rf /var/lib/apt/lists/*
