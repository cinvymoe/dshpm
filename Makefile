VERSION ?= 0.1.0
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X github.com/cinvymoe/dshpm/internal/version.Version=$(VERSION) \
	-X github.com/cinvymoe/dshpm/internal/version.Commit=$(COMMIT)

.PHONY: build test vet clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o dshpm ./cmd/dshpm

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f dshpm
