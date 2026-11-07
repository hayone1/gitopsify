# --match: the SDK's tags (sdk/vX.Y.Z) aren't versions of the CLI.
VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/hayone1/gitopsify/internal/cli.Version=$(VERSION)

.PHONY: build test golden docs gifs lint e2e e2e-vm live clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/gitopsify ./cmd/gitopsify

# The SDK (sdk/) is its own Go module, so it is tested and vetted separately.
test:
	go test ./...
	cd sdk && go test ./...

# Regenerate the golden trees (migrations: testdata/golden; exports:
# pkg/iac/testdata/golden) after an intended change of the output. They
# contain generated .gitignore files that ignore the secret files (.secrets/,
# *.secrets.auto.tfvars), so those fixtures are added with git add -f.
golden:
	go test ./pkg/pipeline -run TestGolden -update
	go test ./pkg/iac -run TestGolden -update
	git add -f testdata/golden/*/.secrets pkg/iac/testdata/golden/*/.secrets pkg/iac/testdata/golden/*/terraform/*/*/*.secrets.auto.tfvars

# Regenerate docs/commands.md and docs/gitopsify.schema.json after changing a
# command, a flag or the configuration.
docs:
	UPDATE_DOCS=1 go test ./internal/cli -run TestCommandReferenceIsCurrent
	UPDATE_DOCS=1 go test ./pkg/config -run "TestSchemaIsCurrent|TestExportSchemaIsCurrent"

# Terminal recordings in docs/images, from docs/tapes, with VHS in Docker.
gifs:
	CGO_ENABLED=0 go build -o bin/gitopsify ./cmd/gitopsify
	for t in scan generate interactive init; do \
	  docker run --rm -v "$$PWD:/vhs" ghcr.io/charmbracelet/vhs docs/tapes/$$t.tape || exit 1; \
	done

lint:
	go vet ./...
	cd sdk && go vet ./...
	gofmt -l . | (! grep .)

# Real clusters: kind, Argo CD, Flux, sealed-secrets. About 15 minutes.
e2e:
	./test/e2e/run.sh

# Remote k3s clusters on Hetzner Cloud and a private GitHub repository; needs
# TF_VAR_hcloud_token. About 30 minutes; destroys what it creates.
live:
	./test/live/hetzner/run.sh

# test/e2e/run.sh on a throwaway Hetzner machine; needs TF_VAR_hcloud_token.
e2e-vm:
	./test/live/hetzner/e2e-on-vm.sh

clean:
	rm -rf bin dist
