# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ogefest/hoarfrost/internal/version.Version=${VERSION}" -o /out/hoarfrost ./cmd/hoarfrost \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/hoarfrost-testsite ./cmd/testsite

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini chromium fonts-dejavu-core fonts-noto-color-emoji poppler-utils \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --home-dir /var/lib/hoarfrost hoarfrost
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
