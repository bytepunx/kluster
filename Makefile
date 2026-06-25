.PHONY: build test vet lint tidy clean install check tools

BINARY := bin/kluster

build:
	@mkdir -p bin
	go build -o $(BINARY) ./kluster

install:
	go install ./kluster

test:
	go test github.com/bytepunx/kluster-lib/... github.com/bytepunx/kluster/...

vet:
	go vet github.com/bytepunx/kluster-lib/... github.com/bytepunx/kluster/...

lint:
	golangci-lint run ./kluster-lib/... ./kluster/...

tidy:
	cd kluster-lib && go mod tidy
	cd kluster && go mod tidy
	go work sync

check: vet test

clean:
	rm -f $(BINARY)

tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
