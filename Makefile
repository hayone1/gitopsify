VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/hayone1/gitopsify/internal/cli.Version=$(VERSION)

.PHONY: build test golden docs lint e2e live clean

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

# Regenerate docs/commands.md and docs/gitopsify.schema.json after changing a
# command, a flag or the configuration.
docs:
	UPDATE_DOCS=1 go test ./internal/cli -run TestCommandReferenceIsCurrent
	UPDATE_DOCS=1 go test ./pkg/config -run TestSchemaIsCurrent

lint:
	go vet ./...
	gofmt -l . | (! grep .)

# Real clusters: kind, Argo CD, Flux, sealed-secrets. About 15 minutes.
e2e:
	./test/e2e/run.sh

# Remote k3s clusters on Hetzner Cloud and a private GitHub repository; needs
# TF_VAR_hcloud_token. About 30 minutes; destroys what it creates.
live:
	./test/live/hetzner/run.sh

clean:
	rm -rf bin dist
