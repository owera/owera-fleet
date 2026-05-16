BINARY     := fleetctl
CMD        := ./cmd/fleetctl
BIN_DIR    := bin
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X github.com/owera/owera-fleet/cmd/fleetctl/commands.Version=$(VERSION)

.PHONY: build test vet lint clean release-dry

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD)

test:
	go test -race ./...

vet:
	go vet ./...

lint: vet

release-dry:
	@mkdir -p $(BIN_DIR)
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-darwin-arm64 $(CMD)
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-darwin-amd64 $(CMD)
	@echo ""
	@echo "Built release binaries:"
	@ls -lh $(BIN_DIR)/$(BINARY)-darwin-*

clean:
	rm -rf $(BIN_DIR)
