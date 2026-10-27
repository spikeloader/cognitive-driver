# Versão: a tag git (v1.2.3 -> 1.2.3) ou, sem tags, 0.1.0-dev.
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null | sed 's/^v//' | grep . || echo 0.1.0-dev)
BIN     := bin/cognitive-driver
# Usa o go do PATH ou, na falta dele (shells não interativos, sudo), a instalação padrão.
GO      ?= $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)

.PHONY: build test cover vet fmt-check run install uninstall validate dist clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/cognitive-driver

test:
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.out ./... && $(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

# Falha se algum arquivo Go não estiver formatado com gofmt.
fmt-check:
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "gofmt pendente:"; echo "$$out"; exit 1; fi

# Execução local (desenvolvimento), sem instalar.
run: build
	$(BIN) -config configs/config.yaml

# Instalação no host: rode `make build` como usuário e depois `sudo make install`.
install:
	scripts/install.sh $(ARGS)

uninstall:
	scripts/uninstall.sh $(ARGS)

# Validação ponta a ponta contra o serviço instalado (~6–10 min).
validate:
	scripts/validate-e2e.sh $(ARGS)

# Pacotes de release (linux amd64/arm64) em dist/: binário em bin/ mais
# scripts, configs e deploy, no mesmo layout do repositório, para que
# scripts/install.sh funcione direto do pacote, sem Go.
PLATFORMS := linux/amd64 linux/arm64
dist:
	rm -rf dist && mkdir -p dist
	set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=cognitive-driver-$(VERSION)-$$os-$$arch; dir=dist/$$name; \
		mkdir -p $$dir/bin; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
			-o $$dir/bin/cognitive-driver ./cmd/cognitive-driver; \
		cp -r configs deploy scripts Makefile README.md README.pt-BR.md LICENSE NOTICE CHANGELOG.md $$dir/; \
		tar -C dist -czf dist/$$name.tar.gz $$name; rm -rf $$dir; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS

clean:
	rm -rf bin dist data coverage.out
