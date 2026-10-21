VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/hayone1/gitopsify/internal/cli.Version=$(VERSION)

.PHONY: build test golden lint e2e clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/gitopsify ./cmd/gitopsify

test:
	go test ./...

# Regenerate testdata/golden after an intended change of the output. The
# golden trees contain generated .gitignore files that ignore .secrets/, so
# those fixtures are added with git add -f.
golden:
	go test ./pkg/pipeline -run TestGolden -update
	git add -f testdata/golden/*/.secrets

lint:
	go vet ./...
	gofmt -l . | (! grep .)

# Real clusters: kind, Argo CD, Flux, sealed-secrets. About 15 minutes.
e2e:
	./test/e2e/run.sh

clean:
	rm -rf bin dist
