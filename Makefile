VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ogefest/hoarfrost/internal/version.Version=$(VERSION)

.PHONY: build test race lint fuzz e2e conformance conformance-tools docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o hoarfrost ./cmd/hoarfrost

test:
	go test ./...

race:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

# Short fuzzing pass over every parser.
fuzz:
	go test ./internal/warcio -run '^$$' -fuzz FuzzReader -fuzztime 30s
	go test ./internal/warcio -run '^$$' -fuzz FuzzParseHTTPResponse -fuzztime 15s
	go test ./internal/cdxj -run '^$$' -fuzz FuzzParse -fuzztime 15s
	go test ./internal/cdxj -run '^$$' -fuzz FuzzSURT -fuzztime 15s
	go test ./internal/engine/static -run '^$$' -fuzz FuzzParsePage -fuzztime 30s
	go test ./internal/engine/static -run '^$$' -fuzz FuzzCSS -fuzztime 30s
	go test ./internal/extract -run '^$$' -fuzz FuzzHTML -fuzztime 15s
	go test ./internal/sources -run '^$$' -fuzz FuzzSitemap -fuzztime 15s
	go test ./internal/upload -run '^$$' -fuzz FuzzParseMHTML -fuzztime 15s

# End-to-end test of the binary; uses S3 when HOARFROST_TEST_S3 is set.
e2e:
	go test -count=1 -v ./e2e

# Archives checked by independent tools (warcio, cdxj-indexer, py-wacz,
# pywb, surt); fails when a tool is missing.
conformance:
	HOARFROST_CONFORMANCE=required go test -count=1 -v ./conformance ./internal/cdxj

# Independent tools used by the conformance suite and the exit test.
conformance-tools:
	python3 -m pip install --user warcio wacz pywb

docker:
	docker build --build-arg VERSION=$(VERSION) -t hoarfrost:$(VERSION) .

clean:
	rm -f hoarfrost
