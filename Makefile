VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
WEBDIST := internal/hub/webdist/build
export CGO_ENABLED=0

.PHONY: build test vet web docker release clean

build:
	go build -ldflags '$(LDFLAGS)' -o bin/smhub ./cmd/smhub
	go build -ldflags '$(LDFLAGS)' -o bin/smagent ./cmd/smagent

test:
	go test ./...

vet:
	go vet ./...

web:
	cd web && npm ci && npm run build
	rm -rf $(WEBDIST) && mkdir -p $(WEBDIST) && cp -R web/build/. $(WEBDIST)/ && touch $(WEBDIST)/.gitkeep

docker:
	docker build -f deploy/Dockerfile -t serversmonitor/hub:$(VERSION) .

release: web
	mkdir -p release
	for os_arch in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do \
	  os=$${os_arch%/*}; arch=$${os_arch#*/}; \
	  GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' -o release/smhub-$$os-$$arch ./cmd/smhub; \
	  GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' -o release/smagent-$$os-$$arch ./cmd/smagent; \
	done

clean:
	rm -rf bin release web/build
	find $(WEBDIST) -mindepth 1 ! -name .gitkeep -delete
