VERSION ?= 0.1.0
LDFLAGS := -s -w -X github.com/cinvymoe/dshpm/internal/version.Version=$(VERSION)

.PHONY: build test vet clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o dshpm ./cmd/dshpm

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f dshpm
