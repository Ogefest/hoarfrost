# syntax=docker/dockerfile:1
# The Go stage runs on the build machine and cross-compiles for the target
# platform, so multi-platform images build without emulating the compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X github.com/ogefest/hoarfrost/internal/version.Version=${VERSION}" -o /out/hoarfrost ./cmd/hoarfrost \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/hoarfrost-testsite ./cmd/testsite

FROM debian:bookworm-slim
ARG VERSION=dev
LABEL org.opencontainers.image.title="hoarfrost" \
      org.opencontainers.image.description="Self-hosted web archiving for news articles and web pages" \
      org.opencontainers.image.source="https://github.com/Ogefest/hoarfrost" \
      org.opencontainers.image.documentation="https://github.com/Ogefest/hoarfrost/blob/main/docs/install.md" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini chromium fonts-dejavu-core fonts-noto-color-emoji poppler-utils \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --home-dir /var/lib/hoarfrost hoarfrost \
 && install -d -o hoarfrost -g hoarfrost /srv/archive
COPY --from=build /out/ /usr/local/bin/
USER hoarfrost
WORKDIR /var/lib/hoarfrost
ENV HOARFROST_DATA_DIR=/var/lib/hoarfrost \
    HOARFROST_LISTEN=:8080 \
    HOARFROST_CHROME=/usr/bin/chromium \
    HOARFROST_CHROME_NO_SANDBOX=true
VOLUME /var/lib/hoarfrost
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--", "hoarfrost"]
CMD ["serve"]
