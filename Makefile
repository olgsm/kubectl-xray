BINARY      := kubectl-xray
PKG         := ./cmd/kubectl-xray
INSTALL_DIR ?= /usr/local/bin

.PHONY: build install uninstall test lint clean

build:
	go build -o $(BINARY) $(PKG)

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "installed $(INSTALL_DIR)/$(BINARY) — try: kubectl xray --help"

uninstall:
	rm -f $(INSTALL_DIR)/$(BINARY)

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)
